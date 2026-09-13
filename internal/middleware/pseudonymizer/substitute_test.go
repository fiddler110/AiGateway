package pseudonymizer

import (
	"context"
	"testing"

	"github.com/scottymacleod/aigateway/internal/pipeline"
)

// P0.14: substitution is one leftmost-longest pass. The old
// strings.ReplaceAll loop rescanned text it had just written, so a
// substituted value containing another key was substituted again.
func TestBuildSubstituterSinglePass(t *testing.T) {
	cases := []struct {
		name    string
		mapping map[string]string
		in      string
		want    string
	}{
		{
			name:    "forward: fake contains another real value",
			mapping: map[string]string{"db.internal": "host admin", "admin": "user-7f3a"},
			in:      "connect to db.internal as admin",
			want:    "connect to host admin as user-7f3a",
		},
		{
			name:    "longest key at a position still wins",
			mapping: map[string]string{"admin": "u1", "admin@example.com": "u2@example.net"},
			in:      "admin@example.com / admin",
			want:    "u2@example.net / u1",
		},
		{
			name:    "empty value is skipped",
			mapping: map[string]string{"keep": ""},
			in:      "keep me",
			want:    "keep me",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildSubstituter(tc.mapping)(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// P0.14 / P0.16: the middleware's own response reversal must not re-match
// inside a real value it just restored.
func TestProcessResponseRestoredValueContainingFake(t *testing.T) {
	mw, _ := New(nil)
	gctx := pipeline.NewGatewayContext("client-a", "up", "")
	gctx.Scratch.ReverseMap = map[string]string{
		"203.0.113.5": "note: host 203.0.113.9 is the fake",
		"203.0.113.9": "10.0.0.9",
	}
	got, err := mw.ProcessResponse(context.Background(), "see 203.0.113.5 and 203.0.113.9", gctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "see note: host 203.0.113.9 is the fake and 10.0.0.9"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
