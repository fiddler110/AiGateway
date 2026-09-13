package handlers

import (
	"context"
	"encoding/json"
	"errors"

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

// errUnrecognizedResponse means a successful upstream body isn't a chat
// completion, so its model output can't be found to scan (P0.18).
var errUnrecognizedResponse = errors.New("upstream response is not a chat completion")

// decodeCompletion decodes a non-streaming chat completion and returns the
// message object of every choice. ok is false unless the body is one JSON
// object whose choices is an array of objects that each have a message
// object.
func decodeCompletion(body []byte) (parsed map[string]any, messages []map[string]any, ok bool) {
	parsed, err := decodeChunk(body)
	if err != nil {
		return nil, nil, false
	}
	choices, isArray := parsed["choices"].([]any)
	if !isArray {
		return nil, nil, false
	}
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		message, isObject := choice["message"].(map[string]any)
		if !isObject {
			return nil, nil, false
		}
		messages = append(messages, message)
	}
	return parsed, messages, true
}

// applyResponsePipeline extracts usage into gctx.Scratch, then runs the
// response-phase middleware pipeline once over every model-generated string
// field of every choice (responseText), writing rewrites back into the
// body. On any middleware blocking, the caller should surface the block
// instead of returning the body. A body the pipeline didn't change is
// returned byte for byte.
//
// A body that isn't a chat completion (see decodeCompletion) hides its model
// output from the pipeline. With any middleware configured that returns
// errUnrecognizedResponse, because DLP fails closed (P0.18). With none, the
// body is passed through unchanged, like the reference does for
// non-standard upstreams.
func applyResponsePipeline(ctx context.Context, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext, body []byte) ([]byte, error) {
	parsed, messages, ok := decodeCompletion(body)
	if !ok {
		if pipe == nil || len(pipe.Entries) == 0 {
			return body, nil
		}
		return nil, errUnrecognizedResponse
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
	for _, message := range messages {
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
