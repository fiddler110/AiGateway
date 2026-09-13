package config

import (
	"strings"
	"testing"
)

// P0.12: auth tables that would authenticate the wrong caller, or anyone,
// are rejected at load, and the error never echoes a key.
func TestValidateAuthTable(t *testing.T) {
	const upstreams = "upstreams:\n  local:\n    base_url: \"http://127.0.0.1:1\"\n"
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"valid users", "users:\n  alice:\n    gateway_key: \"k-alice\"\n  bob:\n    gateway_key: \"k-bob\"\n", false},
		{"valid user upstream", upstreams + "users:\n  alice:\n    gateway_key: \"k-alice\"\n    upstream: local\n", false},
		{"valid auth_key", "settings:\n  auth_key: \"a-real-key\"\n", false},
		{"empty gateway_key", "users:\n  alice:\n    gateway_key: \"\"\n", true},
		{"missing gateway_key", "users:\n  alice:\n    upstream_key_env: X\n", true},
		{"whitespace gateway_key", "users:\n  alice:\n    gateway_key: \"   \"\n", true},
		{"shared gateway_key", "users:\n  alice:\n    gateway_key: \"same-secret-key\"\n  bob:\n    gateway_key: \"same-secret-key\"\n", true},
		{"undeclared user upstream", upstreams + "users:\n  alice:\n    gateway_key: \"k-alice\"\n    upstream: nope\n", true},
		{"auth_key changeme", "settings:\n  auth_key: \"changeme\"\n", true},
		{"auth_key CHANGEME padded", "settings:\n  auth_key: \" CHANGEME \"\n", true},
		{"sanitized username collision", "users:\n  \"bob!\":\n    gateway_key: \"k1\"\n  bob_:\n    gateway_key: \"k2\"\n", true},
		{"truncated username collision", "users:\n  " + strings.Repeat("a", 64) + "x:\n    gateway_key: \"k1\"\n  " + strings.Repeat("a", 64) + "y:\n    gateway_key: \"k2\"\n", true},
		{"empty username", "users:\n  \"\":\n    gateway_key: \"k1\"\n", true},
		{"duplicate username", "users:\n  bob:\n    gateway_key: \"k1\"\n  bob:\n    gateway_key: \"k2\"\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse error = %v, wantErr %t", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "same-secret-key") {
				t.Errorf("error leaks the gateway key: %v", err)
			}
		})
	}
}

// The shipped template must still load after P0.12's stricter validation.
func TestBlankTemplateLoads(t *testing.T) {
	if _, err := Load("../../config/aigateway.blank.yaml"); err != nil {
		t.Fatalf("blank template: %v", err)
	}
}

// P0.9: response size and stream duration bounds have defaults and must be
// positive.
func TestValidateResponseLimits(t *testing.T) {
	cfg, err := Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Settings.MaxResponseBytes != 32*1024*1024 || cfg.Settings.StreamTimeout != 600 {
		t.Errorf("defaults: max_response_bytes %d stream_timeout %v", cfg.Settings.MaxResponseBytes, cfg.Settings.StreamTimeout)
	}
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"positive", "settings:\n  max_response_bytes: 1024\n  stream_timeout: 0.5\n", false},
		{"zero max_response_bytes", "settings:\n  max_response_bytes: 0\n", true},
		{"negative max_response_bytes", "settings:\n  max_response_bytes: -1\n", true},
		{"zero stream_timeout", "settings:\n  stream_timeout: 0\n", true},
		{"negative stream_timeout", "settings:\n  stream_timeout: -5\n", true},
		{"NaN stream_timeout", "settings:\n  stream_timeout: .nan\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if (err != nil) != tc.wantErr {
				t.Errorf("Parse error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

// P0.6: retry/circuit numbers that would break resilience are rejected.
func TestValidateResilience(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"zero retries and delay", "resilience:\n  retry_attempts: 0\n  retry_delay_seconds: 0\n  circuit_breaker:\n    cooldown_seconds: 0\n", false},
		{"fractional delay", "resilience:\n  retry_delay_seconds: 0.25\n", false},
		{"negative retry_attempts", "resilience:\n  retry_attempts: -1\n", true},
		{"negative retry_delay_seconds", "resilience:\n  retry_delay_seconds: -0.5\n", true},
		{"NaN retry_delay_seconds", "resilience:\n  retry_delay_seconds: .nan\n", true},
		{"infinite retry_delay_seconds", "resilience:\n  retry_delay_seconds: .inf\n", true},
		{"zero failure_threshold", "resilience:\n  circuit_breaker:\n    failure_threshold: 0\n", true},
		{"negative failure_threshold", "resilience:\n  circuit_breaker:\n    failure_threshold: -3\n", true},
		{"negative cooldown_seconds", "resilience:\n  circuit_breaker:\n    cooldown_seconds: -1\n", true},
		{"NaN cooldown_seconds", "resilience:\n  circuit_breaker:\n    cooldown_seconds: .nan\n", true},
		{"zero health interval", "resilience:\n  health_check:\n    interval_seconds: 0\n", true},
		{"negative health timeout", "resilience:\n  health_check:\n    timeout_seconds: -5\n", true},
		{"infinite health timeout", "resilience:\n  health_check:\n    timeout_seconds: .inf\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if (err != nil) != tc.wantErr {
				t.Errorf("Parse error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestValidatePersistSessionsForUnauthenticated(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"true", "true", false},
		{"false", "false", false},
		{"string", `"yes"`, true},
		{"number", "1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("middleware: [context_pseudonymizer]\nmiddleware_config:\n  context_pseudonymizer:\n    persist_sessions_for_unauthenticated: " + tc.value + "\n"))
			if (err != nil) != tc.wantErr {
				t.Errorf("Parse error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}
