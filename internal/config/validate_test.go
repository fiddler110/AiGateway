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
		{"auth_key with users", "settings:\n  auth_key: \"a-real-key\"\nusers:\n  alice:\n    gateway_key: \"k-alice\"\n", true},
		{"user gateway_key changeme", "users:\n  alice:\n    gateway_key: \" ChangeMe \"\n", true},
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
	cfg, err := Load("../../config/aigateway.blank.yaml")
	if err != nil {
		t.Fatalf("blank template: %v", err)
	}
	if len(cfg.Warnings) > 0 {
		t.Errorf("blank template loads with warnings %q; its values should all be honoured", cfg.Warnings)
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

func hasWarning(cfg *Config, substr string) bool {
	for _, w := range cfg.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// P0.15: settings that are accepted but have no effect load with a warning
// naming the setting. A config that uses none of them has no warnings.
func TestConfigWarnings(t *testing.T) {
	const unprobed = "upstreams:\n  local:\n    base_url: \"http://127.0.0.1:1\"\n"
	const probed = unprobed + "    health_path: /health\n"
	cases := []struct {
		name string
		yaml string
		want string // substring of one warning; empty means no warnings
	}{
		{"defaults", "{}", ""},
		{"probed upstream", probed, ""},
		{"health checks disabled", unprobed + "resilience:\n  health_check:\n    enabled: false\n", ""},
		{"no upstream has health_path", unprobed, "health_path"},
		{"cache", "cache:\n  enabled: true\n", "cache.enabled"},
		{"semantic cache", probed + "settings:\n  default_upstream: local\ncache:\n  semantic:\n    enabled: true\n", "cache.semantic.enabled"},
		{"redis", "redis:\n  enabled: true\n", "redis.enabled"},
		{"audit_db", "settings:\n  audit_db: other.db\n", "settings.audit_db"},
		{"retention_days", "settings:\n  retention_days: 7\n", "settings.retention_days"},
		{"trust_proxy_headers false", "settings:\n  trust_proxy_headers: false\n", "trust_proxy_headers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if tc.want == "" && len(cfg.Warnings) > 0 {
				t.Errorf("warnings %q, want none", cfg.Warnings)
			}
			if tc.want != "" && !hasWarning(cfg, tc.want) {
				t.Errorf("warnings %q, want one mentioning %q", cfg.Warnings, tc.want)
			}
		})
	}
}

// P0.19: a shared key can't sit alongside named users, including one that
// arrives through AIGATEWAY_AUTH_KEY, and the error names no key.
func TestAuthKeyEnvWithUsersRejected(t *testing.T) {
	t.Setenv(AuthKeyEnv, "env-shared-key")
	_, err := Parse([]byte("users:\n  alice:\n    gateway_key: \"k-alice\"\n"))
	if err == nil || !strings.Contains(err.Error(), AuthKeyEnv) {
		t.Fatalf("Parse error = %v, want one naming %s", err, AuthKeyEnv)
	}
	if strings.Contains(err.Error(), "env-shared-key") || strings.Contains(err.Error(), "k-alice") {
		t.Errorf("error leaks a key: %v", err)
	}
}

// P0.5: health_path is appended to base_url, so it must start with "/".
func TestValidateHealthPath(t *testing.T) {
	for path, wantErr := range map[string]bool{"/health": false, "/v1/models": false, "health": true, "models": true} {
		yaml := "upstreams:\n  local:\n    base_url: \"http://127.0.0.1:1\"\n    health_path: \"" + path + "\"\n"
		if _, err := Parse([]byte(yaml)); (err != nil) != wantErr {
			t.Errorf("health_path %q: Parse error = %v, wantErr %t", path, err, wantErr)
		}
	}
}
