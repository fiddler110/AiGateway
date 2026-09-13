package config

import (
	"strings"
	"testing"
)

// P0.14: unknown YAML keys are errors at every level, so a typo can't
// silently disable a protection.
func TestParseRejectsUnknownKeys(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"empty document", "", false},
		{"known keys", "middleware: [secrets_scanner]\nsettings:\n  listen_port: 9000\n", false},
		{"top-level typo midleware", "midleware: [secrets_scanner]\n", true},
		{"nested settings typo", "settings:\n  listen_prot: 9000\n", true},
		{"nested upstream typo", "upstreams:\n  a:\n    base_ur: \"http://x\"\n", true},
		{"nested user typo", "users:\n  alice:\n    gateway_kye: \"k\"\n", true},
		{"deep resilience typo", "resilience:\n  circuit_breaker:\n    failure_treshold: 3\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

// P0.14: middleware_config for a middleware that isn't enabled is an error
// (it usually means the operator believes a protection is on).
func TestValidateMiddlewareConfigNeedsListedMiddleware(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"listed", "middleware: [pii_redactor]\nmiddleware_config:\n  pii_redactor:\n    fail_open: false\n", false},
		{"not listed", "middleware: [audit_log]\nmiddleware_config:\n  pii_redactor:\n    fail_open: false\n", true},
		{"no middleware at all", "middleware_config:\n  secrets_scanner: {}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Parse error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

// P0.14: trust_proxy_headers: true is rejected with a pointer to
// trusted_proxies, false loads (with a warning, see TestConfigWarnings), and
// trusted_proxies entries must parse.
func TestValidateTrustedProxies(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantErr   string // substring; empty means success
		wantCount int
	}{
		{"none", "{}", "", 0},
		{"cidrs and ips", "settings:\n  trusted_proxies: [\"10.0.0.0/8\", \"192.168.1.10\", \"fd00::/8\", \"::1\"]\n", "", 4},
		{"legacy true", "settings:\n  trust_proxy_headers: true\n", "trusted_proxies", 0},
		{"legacy false", "settings:\n  trust_proxy_headers: false\n", "", 0},
		{"bad prefix length", "settings:\n  trusted_proxies: [\"10.0.0.0/33\"]\n", "trusted_proxies", 0},
		{"not an ip", "settings:\n  trusted_proxies: [\"proxy.local\"]\n", "trusted_proxies", 0},
		{"empty entry", "settings:\n  trusted_proxies: [\"\"]\n", "trusted_proxies", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Parse error = %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := len(cfg.Settings.TrustedProxyPrefixes); got != tc.wantCount {
				t.Errorf("parsed %d prefixes, want %d", got, tc.wantCount)
			}
		})
	}
}

// P0.14: AIGATEWAY_AUTH_KEY overrides settings.auth_key, and P0.12's checks
// apply to the resolved value.
func TestAuthKeyEnvOverride(t *testing.T) {
	t.Run("overrides file value", func(t *testing.T) {
		t.Setenv(AuthKeyEnv, "env-provided-key")
		cfg, err := Parse([]byte("settings:\n  auth_key: \"file-key\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if string(cfg.Settings.AuthKey) != "env-provided-key" {
			t.Errorf("auth_key not overridden by %s", AuthKeyEnv)
		}
	})
	t.Run("changeme rejected after resolution", func(t *testing.T) {
		t.Setenv(AuthKeyEnv, "changeme")
		if _, err := Parse([]byte("{}")); err == nil {
			t.Fatal("env auth key changeme accepted")
		}
	})
	t.Run("set but empty rejected", func(t *testing.T) {
		t.Setenv(AuthKeyEnv, "")
		if _, err := Parse([]byte("settings:\n  auth_key: \"file-key\"\n")); err == nil {
			t.Fatal("empty AIGATEWAY_AUTH_KEY accepted")
		}
	})
}

// P0.14: gateway_key_env resolves a user's key from the environment; both
// fields, or an unset/empty variable, are errors. P0.12 checks run after.
func TestGatewayKeyEnv(t *testing.T) {
	const secretVal = "env-secret-for-alice"
	t.Setenv("P014_TEST_KEY_ALICE", secretVal)
	t.Setenv("P014_TEST_KEY_DUP", secretVal)
	t.Setenv("P014_TEST_KEY_EMPTY", "")
	t.Setenv("P014_TEST_KEY_BLANK", "   ")

	t.Run("resolves", func(t *testing.T) {
		cfg, err := Parse([]byte("users:\n  alice:\n    gateway_key_env: P014_TEST_KEY_ALICE\n"))
		if err != nil {
			t.Fatal(err)
		}
		if string(cfg.Users["alice"].GatewayKey) != secretVal {
			t.Errorf("gateway_key not resolved from env")
		}
	})
	errCases := []struct{ name, yaml string }{
		{"both set", "users:\n  alice:\n    gateway_key: \"k\"\n    gateway_key_env: P014_TEST_KEY_ALICE\n"},
		{"unset var", "users:\n  alice:\n    gateway_key_env: P014_TEST_KEY_DOES_NOT_EXIST\n"},
		{"empty var", "users:\n  alice:\n    gateway_key_env: P014_TEST_KEY_EMPTY\n"},
		{"whitespace var", "users:\n  alice:\n    gateway_key_env: P014_TEST_KEY_BLANK\n"},
		{"duplicate via env", "users:\n  alice:\n    gateway_key_env: P014_TEST_KEY_ALICE\n  bob:\n    gateway_key_env: P014_TEST_KEY_DUP\n"},
		{"duplicate env and file", "users:\n  alice:\n    gateway_key_env: P014_TEST_KEY_ALICE\n  bob:\n    gateway_key: \"" + secretVal + "\"\n"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if strings.Contains(err.Error(), secretVal) {
				t.Errorf("error leaks the key: %v", err)
			}
		})
	}
}
