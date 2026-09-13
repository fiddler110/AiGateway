// Package pipeline implements the middleware chain that every chat request
// and response passes through for DLP, accounting, and policy enforcement.
package pipeline

import "time"

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
	CostFinalized       bool // idempotency guard; the pipeline already calls accounting middleware once per response (ResponseAccounting)
	TokensFinalized     bool
	RequestModel        string
	MessageCount        int  // captured by audit_log's request phase
	Stream              bool // captured by audit_log's request phase
	AuditCaptured       bool // audit_log's request phase ran (it doesn't if an earlier middleware blocked)
}

// GatewayContext is the mutable per-request state threaded through the
// entire pipeline. It is request-local and never shared across goroutines,
// so it needs no internal synchronization.
type GatewayContext struct {
	ClientID string
	// Authenticated is true only when ClientID is a verified users-table
	// identity. In shared-auth_key and open modes ClientID comes from the
	// advisory, spoofable x-client-id header and this is false, so nothing
	// security-relevant may be keyed on ClientID (see P0.10).
	Authenticated bool

	Upstream    string // mutated by the upstream manager to reflect the upstream that actually served the request
	SourceIP    string
	Blocked     bool
	BlockReason string
	// BlockStatus is the HTTP status for a block; 0 means 400. Rate-limit
	// and budget blocks set 429 with RetryAfter (P0.14); content and DLP
	// blocks leave both zero.
	BlockStatus int
	RetryAfter  time.Duration

	// BlockMiddleware/BlockDirection are set once, by whichever middleware
	// first blocks the request, and never overwritten thereafter.
	BlockMiddleware string
	BlockDirection  string // "request" | "response"

	// Outcome fields, set by the handler before Pipeline.Finish runs.
	// RequestID matches the x-request-id response header. ServedBy is the
	// upstream that answered (any status), or "" if none did, unlike
	// Upstream, which starts as the first route candidate. Status is the
	// HTTP status sent, or for a stream that began with 200 and then failed,
	// the code of its SSE error event. StartedAt is when the handler began.
	RequestID string
	ServedBy  string
	Status    int
	StartedAt time.Time

	Scratch Scratch
}

// BlockHTTPStatus returns the status to send for a block.
func (g *GatewayContext) BlockHTTPStatus() int {
	if g.BlockStatus == 0 {
		return 400
	}
	return g.BlockStatus
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
