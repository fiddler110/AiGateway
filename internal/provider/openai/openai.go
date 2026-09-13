// Package openai implements the identity Translator: the gateway's
// canonical chat shape IS the OpenAI shape, so this translator passes
// requests and responses through unchanged. It covers native OpenAI, any
// OpenAI-compatible cloud (OpenRouter, Groq, GitHub Copilot, ...), and local
// runtimes (Ollama, LM Studio, llama.cpp) with zero special-casing — they
// all just set api_format: openai (the default).
package openai

import (
	"encoding/json"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/provider"
)

const defaultChatPath = "/chat/completions"

type Translator struct{}

func New() *Translator { return &Translator{} }

func (t *Translator) Format() string { return "openai" }

func (t *Translator) ToUpstream(req *chatmodel.ChatRequest) ([]byte, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	return body, defaultChatPath, nil
}

// usageEnvelope extracts just the usage block from an OpenAI-shaped
// response without needing the full response schema.
type usageEnvelope struct {
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (t *Translator) FromUpstream(nativeBody []byte) ([]byte, provider.Usage, error) {
	var env usageEnvelope
	usage := provider.Usage{}
	if err := json.Unmarshal(nativeBody, &env); err == nil && env.Usage.TotalTokens > 0 {
		usage = provider.Usage{
			PromptTokens:     env.Usage.PromptTokens,
			CompletionTokens: env.Usage.CompletionTokens,
			TotalTokens:      env.Usage.TotalTokens,
			Actual:           true,
		}
	}
	return nativeBody, usage, nil
}

// streamTranslator is a pure passthrough — OpenAI SSE lines need no
// reshaping since the wire format already matches the canonical shape.
type streamTranslator struct{}

func (t *Translator) NewStreamTranslator() provider.StreamTranslator {
	return &streamTranslator{}
}

func (s *streamTranslator) Feed(nativeEvent []byte) ([][]byte, error) {
	return [][]byte{nativeEvent}, nil
}

func (s *streamTranslator) Done() ([][]byte, provider.Usage, error) {
	return nil, provider.Usage{}, nil
}
