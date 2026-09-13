package handlers

import (
	"context"
	"encoding/json"

	"github.com/scottymacleod/aigateway/internal/pipeline"
)

func toInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// applyResponsePipeline extracts usage into gctx.Scratch, then runs the
// response-phase middleware pipeline over each choice's message content
// (mirroring the reference's per-choice scan), mutating the response body
// in place. On any middleware blocking, the caller should surface the block
// instead of returning the (possibly partially rewritten) body. Non-JSON or
// unrecognized bodies are passed through unchanged, matching the
// reference's tolerant behavior for non-standard upstream responses.
func applyResponsePipeline(ctx context.Context, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext, body []byte) ([]byte, error) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, nil
	}

	if usage, ok := parsed["usage"].(map[string]any); ok {
		if pt, ok := toInt(usage["prompt_tokens"]); ok {
			gctx.Scratch.ActualPromptTokens = &pt
		}
		if ct, ok := toInt(usage["completion_tokens"]); ok {
			gctx.Scratch.ActualComplTokens = &ct
		}
	}

	choices, ok := parsed["choices"].([]any)
	if !ok || pipe == nil {
		return body, nil
	}

	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		message, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].(string)
		if !ok {
			continue
		}
		newContent, err := pipe.RunResponse(ctx, content, gctx)
		if err != nil {
			return body, err
		}
		message["content"] = newContent
		if gctx.Blocked {
			break
		}
	}

	out, err := json.Marshal(parsed)
	if err != nil {
		return body, nil
	}
	return out, nil
}
