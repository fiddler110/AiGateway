// Package costtracker enforces per-client monthly spend budgets and
// estimates/reconciles cost from token usage. Fails open by default: a bug
// here shouldn't block traffic, only accounting accuracy suffers.
package costtracker

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

const charsPerToken = 4

type rate struct {
	promptPer1K     float64
	completionPer1K float64
}

type Middleware struct {
	pricing map[string]rate
	// pricingKeysBySpecificity is pricing's keys sorted longest-first, so
	// substring matching against a model name prefers the most specific
	// configured key (e.g. "gpt-4o" before "gpt-4") rather than whatever
	// order the config map happened to iterate in — a deliberate fix over
	// the Python reference's iteration-order-dependent substring match.
	pricingKeysBySpecificity []string
	budgets                  map[string]float64
	defaultBudget            float64

	now func() time.Time

	mu           sync.Mutex
	monthKey     string
	monthlySpend map[string]float64 // client_id -> spend this month
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{
		pricing:      map[string]rate{},
		budgets:      map[string]float64{},
		now:          time.Now,
		monthlySpend: map[string]float64{},
	}

	if raw, ok := cfg["pricing"].(map[string]any); ok {
		for model, v := range raw {
			entry, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("cost_tracker: pricing[%q] must be an object", model)
			}
			r := rate{}
			if p, ok := numberField(entry, "prompt_per_1k"); ok {
				r.promptPer1K = p
			}
			if c, ok := numberField(entry, "completion_per_1k"); ok {
				r.completionPer1K = c
			}
			m.pricing[model] = r
			m.pricingKeysBySpecificity = append(m.pricingKeysBySpecificity, model)
		}
	}
	sort.Slice(m.pricingKeysBySpecificity, func(i, j int) bool {
		return len(m.pricingKeysBySpecificity[i]) > len(m.pricingKeysBySpecificity[j])
	})

	if raw, ok := cfg["budgets"].(map[string]any); ok {
		for client, v := range raw {
			if f, ok := toFloat(v); ok {
				if client == "default" {
					m.defaultBudget = f
				} else {
					m.budgets[client] = f
				}
			}
		}
	}

	m.monthKey = monthKeyFor(m.now())
	return m, nil
}

func numberField(m map[string]any, key string) (float64, bool) {
	return toFloat(m[key])
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func monthKeyFor(t time.Time) string {
	return t.UTC().Format("2006-01")
}

func (m *Middleware) Name() string { return "cost_tracker" }

// rateFor resolves pricing by exact model match first, then the longest
// (most specific) configured substring match.
func (m *Middleware) rateFor(model string) rate {
	if r, ok := m.pricing[model]; ok {
		return r
	}
	for _, key := range m.pricingKeysBySpecificity {
		if containsSubstring(model, key) {
			return m.pricing[key]
		}
	}
	return rate{}
}

func containsSubstring(s, sub string) bool {
	if sub == "" {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func (m *Middleware) budgetFor(clientID string) float64 {
	if b, ok := m.budgets[clientID]; ok {
		return b
	}
	return m.defaultBudget
}

// checkMonthRollover clears in-memory spend on a UTC month change. Caller
// must hold m.mu.
func (m *Middleware) checkMonthRolloverLocked(now time.Time) {
	key := monthKeyFor(now)
	if key != m.monthKey {
		m.monthKey = key
		m.monthlySpend = map[string]float64{}
	}
}

func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	now := m.now()
	budget := m.budgetFor(gctx.ClientID)

	m.mu.Lock()
	m.checkMonthRolloverLocked(now)
	current := m.monthlySpend[gctx.ClientID]
	if budget > 0 && current >= budget {
		m.mu.Unlock()
		gctx.Blocked = true
		gctx.BlockReason = fmt.Sprintf("cost_tracker: monthly budget exceeded ($%.2f spent of $%.2f budget)", current, budget)
		gctx.BlockStatus = http.StatusTooManyRequests
		// Spend resets at the start of the next UTC month.
		u := now.UTC()
		gctx.RetryAfter = time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(u)
		return nil
	}

	chars := 0
	for _, msg := range req.Messages {
		chars += len(chatmodel.MessageScanText(msg))
	}
	estimatedTokens := chars / charsPerToken
	r := m.rateFor(req.Model)
	estimatedCost := float64(estimatedTokens) / 1000.0 * r.promptPer1K

	m.monthlySpend[gctx.ClientID] = current + estimatedCost
	m.mu.Unlock()

	gctx.Scratch.EstimatedCost = estimatedCost
	gctx.Scratch.RequestModel = req.Model
	return nil
}

// AccountsWholeResponse marks cost_tracker as pipeline.ResponseAccounting:
// ProcessResponse gets the whole response's text once, not once per field.
func (m *Middleware) AccountsWholeResponse() {}

// ProcessResponse reconciles the pre-charged estimate against actual
// provider-reported usage exactly once per request. The pipeline calls it
// once per response (ResponseAccounting); CostFinalized guards against a
// second response pass anyway.
func (m *Middleware) ProcessResponse(_ context.Context, text string, gctx *pipeline.GatewayContext) (string, error) {
	if gctx.Scratch.CostFinalized {
		return text, nil
	}
	gctx.Scratch.CostFinalized = true

	r := m.rateFor(gctx.Scratch.RequestModel)

	actualPromptCost := 0.0
	haveActual := gctx.Scratch.ActualPromptTokens != nil
	if haveActual {
		actualPromptCost = float64(*gctx.Scratch.ActualPromptTokens) / 1000.0 * r.promptPer1K
	}

	completionTokens := len(text) / charsPerToken
	if gctx.Scratch.ActualComplTokens != nil {
		completionTokens = *gctx.Scratch.ActualComplTokens
	}
	completionCost := float64(completionTokens) / 1000.0 * r.completionPer1K

	m.mu.Lock()
	if haveActual {
		delta := actualPromptCost - gctx.Scratch.EstimatedCost
		m.monthlySpend[gctx.ClientID] += delta
	}
	m.monthlySpend[gctx.ClientID] += completionCost
	total := m.monthlySpend[gctx.ClientID]
	m.mu.Unlock()

	gctx.Scratch.CompletionCost = completionCost
	gctx.Scratch.TotalSpend = total
	return text, nil
}
