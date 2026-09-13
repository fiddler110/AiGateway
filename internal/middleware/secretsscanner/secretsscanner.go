// Package secretsscanner detects credential-shaped strings (API keys,
// tokens, private keys, connection strings) via prefix patterns, plus a
// Shannon-entropy fallback for unstructured high-entropy secrets (the same
// heuristic gitleaks/trufflehog use). Fails closed by default, and this
// middleware's presence forces the streaming handler into hard-buffer mode
// (see internal/handlers) — "flag after leak" is unacceptable for
// credentials in a live stream.
package secretsscanner

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

type action int

const (
	actionBlock action = iota
	actionFlag
)

type namedPattern struct {
	name   string
	re     *regexp.Regexp
	action action
}

// patterns are checked in order; the first match wins for a given
// substring. The entropy scan (below) only runs if none of these matched.
var patterns = []namedPattern{
	{"openai_key", regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`), actionBlock},
	{"aws_access_key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`), actionBlock},
	{"aws_secret_key", regexp.MustCompile(`(?i)(aws|secret).{0,40}[A-Za-z0-9/+=]{40}`), actionBlock},
	{"github_token", regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`), actionBlock},
	{"github_pat", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`), actionBlock},
	{"private_key", regexp.MustCompile(`-----BEGIN (RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`), actionBlock},
	{"connection_string", regexp.MustCompile(`(?i)(postgres|postgresql|mysql|mongodb|redis|amqp)://[^:/\s]+:[^@/\s]+@`), actionBlock},
	{"jwt_token", regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), actionFlag},
	{"password_field", regexp.MustCompile(`(?i)(?:password|passwd|pwd)=(\S{4,})`), actionFlag},
}

// placeholderPrefixes excludes obvious non-secret placeholder values from
// the password_field flag pattern above (RE2 has no negative lookahead, so
// this is applied as a post-match filter instead of being baked into the
// regex).
var placeholderPrefixes = []string{"your_", "example", "placeholder", "changeme", "<"}

const (
	entropyMinLength     = 20
	entropyDefaultThresh = 4.0
)

var entropyTokenRE = regexp.MustCompile(`[A-Za-z0-9+/_=-]{20,}`)

type Middleware struct {
	entropyDetection bool
	entropyThreshold float64
	entropyMinLen    int
	blockOnFlag      bool
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{
		entropyDetection: true,
		entropyThreshold: entropyDefaultThresh,
		entropyMinLen:    entropyMinLength,
	}
	if v, ok := cfg["entropy_detection"].(bool); ok {
		m.entropyDetection = v
	}
	if v, ok := numberField(cfg, "entropy_threshold"); ok {
		m.entropyThreshold = v
	}
	if v, ok := numberField(cfg, "entropy_min_length"); ok {
		m.entropyMinLen = int(v)
	}
	if v, ok := cfg["block_on_flag"].(bool); ok {
		m.blockOnFlag = v
	}
	return m, nil
}

func numberField(cfg map[string]any, key string) (float64, bool) {
	v, ok := cfg[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func (m *Middleware) Name() string { return "secrets_scanner" }

// scan returns the name of the first blocking match, or "" if none, and
// appends any flagged (non-blocking, unless block_on_flag) matches to
// flagged.
func (m *Middleware) scan(text string, flagged *[]string) (blockedBy string, err error) {
	matchedAny := false
	for _, p := range patterns {
		if p.name == "password_field" {
			sub := p.re.FindStringSubmatch(text)
			if sub == nil {
				continue
			}
			if hasPlaceholderPrefix(sub[1]) {
				continue
			}
			matchedAny = true
		} else {
			if !p.re.MatchString(text) {
				continue
			}
			matchedAny = true
		}
		eff := p.action
		if m.blockOnFlag {
			eff = actionBlock
		}
		if eff == actionBlock {
			return fmt.Sprintf("%s pattern matched", p.name), nil
		}
		*flagged = append(*flagged, p.name)
	}
	if matchedAny || !m.entropyDetection {
		return "", nil
	}
	// Entropy fallback: only runs if no prefix pattern matched at all.
	for _, tok := range entropyTokenRE.FindAllString(text, -1) {
		if len(tok) < m.entropyMinLen {
			continue
		}
		if shannonEntropy(tok) >= m.entropyThreshold {
			if m.blockOnFlag {
				return "high_entropy_token pattern matched", nil
			}
			*flagged = append(*flagged, "high_entropy_token")
		}
	}
	return "", nil
}

func hasPlaceholderPrefix(value string) bool {
	lower := strings.ToLower(value)
	for _, prefix := range placeholderPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func shannonEntropy(s string) float64 {
	freq := map[rune]int{}
	for _, r := range s {
		freq[r]++
	}
	entropy := 0.0
	n := float64(len(s))
	for _, count := range freq {
		p := float64(count) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}

func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	for i := range req.Messages {
		text := chatmodel.MessageScanText(req.Messages[i])
		if text == "" {
			continue
		}
		blockedBy, err := m.scan(text, &gctx.Scratch.SecretsFlagged)
		if err != nil {
			return err
		}
		if blockedBy != "" {
			gctx.Blocked = true
			gctx.BlockReason = "secrets_scanner: " + blockedBy
			return nil
		}
	}
	return nil
}

func (m *Middleware) ProcessResponse(_ context.Context, text string, gctx *pipeline.GatewayContext) (string, error) {
	if text == "" {
		return text, nil
	}
	blockedBy, err := m.scan(text, &gctx.Scratch.SecretsFlaggedResp)
	if err != nil {
		return text, err
	}
	if blockedBy != "" {
		gctx.Blocked = true
		gctx.BlockReason = "secrets_scanner: " + blockedBy
	}
	return text, nil
}
