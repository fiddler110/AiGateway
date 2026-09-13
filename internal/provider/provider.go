// Package provider translates between the gateway's canonical OpenAI-shaped
// chat format and each upstream's native wire format. Middleware, caching,
// and the streaming handler only ever operate on the canonical shape;
// translation happens once, at the upstream-forwarding boundary.
package provider

import "github.com/scottymacleod/aigateway/internal/chatmodel"

// Usage reports token accounting extracted from an upstream response.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Actual           bool // false if these are gateway-side estimates, not provider-reported
}

// Translator converts between the canonical OpenAI chat shape and one
// upstream's native format. Implementations: openai (identity — also covers
// OpenAI-compatible clouds and local runtimes like Ollama/LM Studio/
// llama.cpp), anthropic, and gemini.
type Translator interface {
	// Format returns the api_format name this translator handles.
	Format() string

	// ToUpstream converts a canonical request into the upstream's native
	// wire body, and returns the default path suffix to use if the
	// upstream config hasn't overridden chat_path.
	ToUpstream(req *chatmodel.ChatRequest) (body []byte, defaultPath string, err error)

	// FromUpstream converts a complete non-streaming native response back
	// into the canonical OpenAI-shaped response body.
	FromUpstream(nativeBody []byte) (openaiBody []byte, usage Usage, err error)

	// NewStreamTranslator returns a fresh stateful translator for one
	// streaming request. Must not be shared across concurrent streams.
	NewStreamTranslator() StreamTranslator
}

// StreamTranslator incrementally converts one upstream's native SSE/event
// stream into canonical OpenAI-shaped SSE lines.
type StreamTranslator interface {
	// Feed processes one native stream event and returns zero or more
	// OpenAI-shaped SSE lines to forward immediately (empty = swallow,
	// e.g. framing events with no OpenAI equivalent).
	Feed(nativeEvent []byte) (openaiSSELines [][]byte, err error)

	// Done signals end-of-native-stream and returns any final line(s)
	// (e.g. "data: [DONE]") plus aggregated usage for accounting.
	Done() (openaiSSELines [][]byte, usage Usage, err error)
}

// Registry maps an api_format config value to a Translator constructor.
// Adding a future provider is additive here — no call-site changes needed
// in the upstream forwarder. Built by cmd/aigateway/main.go, which is free
// to import every provider subpackage without creating an import cycle
// (each provider subpackage imports this package, not the reverse).
type Registry map[string]func() Translator
