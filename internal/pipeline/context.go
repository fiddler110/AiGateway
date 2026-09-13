// Package pipeline implements the middleware chain that every chat request
// and response passes through for DLP, accounting, and policy enforcement.
package pipeline

import (
	"sort"
	"strings"
)

// Scratch holds fields that specific middlewares read/write to communicate
// with each other and with the handler. Unlike the Python reference's
// untyped extra dict, this is a typed struct so cross-middleware data flow
// is explicit and compiler-checked: a typo in a field name is a build error,
// not a silent no-op at runtime.
type Scratch struct {
	PseudonymMap        map[string]string // real -> fake, full merged session map for this request
	ReverseMap          map[string]string // fake -> real, inverse of PseudonymMap
	PseudonymizedCount  int
	EstimatedTokens     int
	ActualPromptTokens  *int
	ActualComplTokens   *int
	EstimatedCost       float64
	TotalSpend          float64
	CompletionCost      float64
	ContentPolicyMatch  string // matched pattern, deliberately kept OUT of BlockReason
	SemanticCacheHit    bool
	SecretsFlagged      []string
	SecretsFlaggedResp  []string
	PIIRedacted         int
	PIIRedactedResponse bool
	CostFinalized       bool // idempotency guard: response pipeline may run once per choice
	TokensFinalized     bool
	RequestModel        string
}

// GatewayContext is the mutable per-request state threaded through the
// entire pipeline. It is request-local and never shared across goroutines,
// so it needs no internal synchronization.
type GatewayContext struct {
	ClientID    string
	Upstream    string // mutated by the upstream manager to reflect the upstream that actually served the request
	SourceIP    string
	Blocked     bool
	BlockReason string

	// BlockMiddleware/BlockDirection are set once, by whichever middleware
	// first blocks the request, and never overwritten thereafter.
	BlockMiddleware string
	BlockDirection  string // "request" | "response"

	Scratch Scratch
}

// ReverseSubstitute applies gctx.Scratch.ReverseMap (fake -> real) to text,
// longest-fake-first to avoid partial-substring collisions. Used by
// passthrough-mode streaming to reverse pseudonymization on each complete
// line as it's forwarded, without waiting for the full response.
func (gctx *GatewayContext) ReverseSubstitute(text string) string {
	if len(gctx.Scratch.ReverseMap) == 0 {
		return text
	}
	keys := make([]string, 0, len(gctx.Scratch.ReverseMap))
	for k := range gctx.Scratch.ReverseMap {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		text = strings.ReplaceAll(text, k, gctx.Scratch.ReverseMap[k])
	}
	return text
}

// NewGatewayContext constructs a context for a new request with sane zero
// values (ClientID defaults to "anonymous" per the auth layer's contract).
func NewGatewayContext(clientID, upstream, sourceIP string) *GatewayContext {
	if clientID == "" {
		clientID = "anonymous"
	}
	return &GatewayContext{
		ClientID: clientID,
		Upstream: upstream,
		SourceIP: sourceIP,
	}
}
