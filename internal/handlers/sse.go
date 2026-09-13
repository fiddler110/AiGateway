package handlers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// extractSSEContent concatenates choices[].delta.content across every
// "data: {...}" line in a raw SSE byte stream (skipping "[DONE]" and
// non-JSON lines), reconstructing the full assistant message text for
// accounting/DLP scanning purposes.
func extractSSEContent(body []byte) string {
	var out strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			out.WriteString(c.Delta.Content)
		}
	}
	return out.String()
}

// extractSSEUsage looks for a terminal usage object with a truthy
// total_tokens, matching providers that emit a final chunk carrying usage
// when stream_options.include_usage is set.
func extractSSEUsage(body []byte) (prompt, completion int, ok bool) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		data, cut := strings.CutPrefix(line, "data: ")
		if !cut {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			prompt, completion, ok = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, true
		}
	}
	return prompt, completion, ok
}
