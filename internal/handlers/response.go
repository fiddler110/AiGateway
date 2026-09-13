package handlers

import (
	"context"
	"encoding/json"

	"github.com/scottymacleod/aigateway/internal/pipeline"
)

func toInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		f, ferr := n.Float64()
		if ferr != nil {
			return 0, false
		}
		i = int64(f)
	}
	return int(i), true
}

// applyResponsePipeline extracts usage into gctx.Scratch, then runs the
// response-phase middleware pipeline once over every model-generated string
// field of every choice (responseText), writing rewrites back into the
// body. On any middleware blocking, the caller should surface the block
// instead of returning the body. A body the pipeline didn't change is
// returned byte for byte. Non-JSON or unrecognized bodies are passed through
// unchanged, matching the reference's tolerant behavior for non-standard
// upstream responses.
func applyResponsePipeline(ctx context.Context, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext, body []byte) ([]byte, error) {
	parsed, err := decodeChunk(body)
	if err != nil {
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

	if pipe == nil {
		return body, nil
	}

	var rt responseText
	choices, _ := parsed["choices"].([]any)
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		message, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		rt.addStringFields(message)
		calls, _ := message["tool_calls"].([]any)
		for _, tc := range calls {
			call, _ := tc.(map[string]any)
			addFunctionArguments(&rt, call["function"])
		}
		addFunctionArguments(&rt, message["function_call"]) // legacy single call
	}

	if err := rt.run(ctx, pipe, gctx); err != nil || gctx.Blocked || !rt.changed {
		return body, err
	}
	out, err := marshalJSON(parsed)
	if err != nil {
		return body, err
	}
	return out, nil
}

// addFunctionArguments adds fn.arguments when fn is a function object with
// string arguments.
func addFunctionArguments(rt *responseText, fn any) {
	f, _ := fn.(map[string]any)
	if args, ok := f["arguments"].(string); ok {
		rt.addArguments(args, func(s string) { f["arguments"] = s })
	}
}
