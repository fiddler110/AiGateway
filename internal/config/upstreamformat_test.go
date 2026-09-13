package config

import (
	"strings"
	"testing"
)

// P0.13: api_format and auth_type values that would silently fall back to
// openai/bearer are rejected at load; anthropic and gemini are rejected as
// not yet supported until P1.1/P1.2.
func TestValidateUpstreamFormatAndAuthType(t *testing.T) {
	up := func(field string) string {
		return "upstreams:\n  u:\n    base_url: \"http://127.0.0.1:1\"\n" + field
	}
	cases := []struct {
		name    string
		yaml    string
		wantErr string // "" means valid
	}{
		{"unset", up(""), ""},
		{"openai", up("    api_format: openai\n"), ""},
		{"auth bearer", up("    auth_type: bearer\n"), ""},
		{"auth x-api-key", up("    auth_type: x-api-key\n"), ""},
		{"auth none", up("    auth_type: none\n"), ""},
		{"anthropic", up("    api_format: anthropic\n"), "not yet supported"},
		{"gemini", up("    api_format: gemini\n"), "not yet supported"},
		{"unknown format", up("    api_format: ollama\n"), `api_format "ollama" is unknown`},
		{"format wrong case", up("    api_format: OpenAI\n"), "is unknown"},
		{"unknown auth_type", up("    auth_type: basic\n"), `auth_type "basic" is unknown`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Parse error = %v, want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("Parse error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
