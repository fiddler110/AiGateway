package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

// Two fakes where one is a prefix of the other happen in practice: fake IPv4
// addresses are 203.0.113.1-254.
var testReverseMap = map[string]string{
	"203.0.113.5":  "10.0.0.5",
	"203.0.113.55": "10.0.0.55",
	"abc":          "[abc-real]", // restored value contains another fake
	"bcde":         "[bcde-real]",
	"item-ü1":      `C:\Users\"q"`,
}

func TestStreamReverserOneShot(t *testing.T) {
	r := newStreamReverser(testReverseMap, false)
	cases := []struct{ in, want string }{
		{"no fakes here", "no fakes here"},
		{"hosts 203.0.113.5 and 203.0.113.55.", "hosts 10.0.0.5 and 10.0.0.55."},
		{"xabcde", "x[abc-real]de"}, // leftmost wins over a longer later match
		{"abc", "[abc-real]"},       // restored text isn't rescanned
		{"ß item-ü1 ß", `ß C:\Users\"q" ß`},
		{"203.0.113.5", "10.0.0.5"},
	}
	for _, tc := range cases {
		got, held := r.reverse(tc.in, true)
		if got != tc.want || held != "" {
			t.Errorf("reverse(%q) = %q, held %q; want %q", tc.in, got, held, tc.want)
		}
	}
}

// Splitting text at any points must produce the same output as reversing it
// whole, and must never hold back as much as a whole fake.
func TestStreamReverserSplitsMatchOneShot(t *testing.T) {
	r := newStreamReverser(testReverseMap, false)
	texts := []string{
		"hosts 203.0.113.5 and 203.0.113.55.",
		"ends with 203.0.113.5",
		"ends with 203.0.113.55",
		"xabcde abcd bcdef",
		"ß item-ü1 ß item-ü",
		"203.0.113.",
	}
	for _, text := range texts {
		want, _ := r.reverse(text, true)
		for i := 0; i <= len(text); i++ {
			for j := i; j <= len(text); j++ {
				pieces := []string{text[:i], text[i:j], text[j:]}
				var got, held string
				for _, p := range pieces {
					var out string
					out, held = r.reverse(held+p, false)
					if len(held) >= r.maxLen {
						t.Fatalf("held %q is as long as a fake", held)
					}
					got += out
				}
				out, _ := r.reverse(held, true)
				got += out
				if got != want {
					t.Fatalf("pieces %q: got %q, want %q", pieces, got, want)
				}
			}
		}
	}
}

func TestStreamReverserJSONText(t *testing.T) {
	r := newStreamReverser(testReverseMap, true)
	got, _ := r.reverse(`{"path":"item-ü1","ip":"203.0.113.5"}`, true)
	var args struct{ Path, IP string }
	if err := json.Unmarshal([]byte(got), &args); err != nil {
		t.Fatalf("reversed arguments %q aren't valid JSON: %v", got, err)
	}
	if args.Path != `C:\Users\"q"` || args.IP != "10.0.0.5" {
		t.Errorf("got %+v", args)
	}
}

func TestNewStreamReverserEmpty(t *testing.T) {
	if r := newStreamReverser(nil, false); r != nil {
		t.Fatal("expected nil reverser for an empty map")
	}
	if p := newPassthroughReverser(map[string]string{}); p != nil {
		t.Fatal("expected nil passthrough reverser for an empty map")
	}
	var p *passthroughReverser
	if got := string(p.rewriteLine([]byte(`data: {"a":1}`))); got != `data: {"a":1}` {
		t.Errorf("nil reverser changed line: %q", got)
	}
}

// A possible fake at the end of a delta is released by the finish chunk, or
// by a synthesized chunk before [DONE] when no finish chunk comes.
func TestPassthroughReverserReleasesHeldText(t *testing.T) {
	type delta struct {
		Content   *string `json:"content"`
		ToolCalls []struct {
			Index    int
			Function struct{ Arguments string }
		} `json:"tool_calls"`
	}
	type chunk struct {
		ID      string `json:"id"`
		Choices []struct {
			Delta        delta   `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}

	run := func(t *testing.T, lines ...string) (content, args string, finished bool) {
		t.Helper()
		p := newPassthroughReverser(testReverseMap)
		var out []string
		for _, l := range lines {
			out = append(out, strings.Split(string(p.rewriteLine([]byte(l))), "\n")...)
		}
		if tail := p.finish(); tail != nil {
			out = append(out, "data: "+string(tail))
		}
		for _, l := range out {
			payload, ok := strings.CutPrefix(l, "data: ")
			if !ok || payload == "[DONE]" {
				continue
			}
			var c chunk
			if err := json.Unmarshal([]byte(payload), &c); err != nil {
				t.Fatalf("invalid chunk %q: %v", payload, err)
			}
			if c.ID != "x" {
				t.Errorf("chunk %q lost top-level id", payload)
			}
			for _, ch := range c.Choices {
				if ch.Delta.Content != nil {
					content += *ch.Delta.Content
				}
				for _, tc := range ch.Delta.ToolCalls {
					args += tc.Function.Arguments
				}
				finished = finished || ch.FinishReason != nil
			}
		}
		return content, args, finished
	}

	t.Run("finish chunk", func(t *testing.T) {
		content, args, finished := run(t,
			`data: {"id":"x","choices":[{"index":0,"delta":{"content":"at 203.0.113.5","tool_calls":[{"index":0,"function":{"arguments":"{\"ip\":\"203.0.113.5"}}]}}]}`,
			`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		)
		if content != "at 10.0.0.5" || args != `{"ip":"10.0.0.5` || !finished {
			t.Errorf("content %q args %q finished %t", content, args, finished)
		}
	})
	t.Run("done without finish", func(t *testing.T) {
		content, _, _ := run(t,
			`data: {"id":"x","choices":[{"index":0,"delta":{"content":"at 203.0.113.5"}}]}`,
			`data: [DONE]`,
		)
		if content != "at 10.0.0.5" {
			t.Errorf("content %q", content)
		}
	})
	t.Run("eof without done", func(t *testing.T) {
		content, _, _ := run(t,
			`data: {"id":"x","choices":[{"index":0,"delta":{"content":"at 203.0.113.5"}}]}`,
		)
		if content != "at 10.0.0.5" {
			t.Errorf("content %q", content)
		}
	})
}
