package piiredactor

import "testing"

func TestRedact(t *testing.T) {
	m := &Middleware{patterns: append([]patternReplacement{}, builtinPatterns...)}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"email", "contact me at john.doe@example.com please", "contact me at [EMAIL] please"},
		{"phone", "call 555-123-4567 now", "call [PHONE] now"},
		{"ssn", "SSN: 123-45-6789", "SSN: [SSN]"},
		{"card 16-digit", "card 4111 1111 1111 1111 on file", "card [CARD] on file"},
		{"card amex 15-digit", "amex 3782 822463 10005 here", "amex [CARD] here"},
		{"no PII", "just a normal sentence", "just a normal sentence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := m.redact(tc.in)
			if got != tc.want {
				t.Errorf("redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtraPatterns(t *testing.T) {
	mw, err := New(map[string]any{
		"extra_patterns": []any{
			map[string]any{"regex": `internal-id-\d+`, "replacement": "[INTERNAL_ID]"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)
	got := m.redact("see internal-id-4821 for details")
	want := "see [INTERNAL_ID] for details"
	if got != want {
		t.Errorf("redact() = %q, want %q", got, want)
	}
}
