package server

import (
	"crypto/sha256"
	"net/http/httptest"
	"testing"

	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/secret"
)

// P0.12: keys are compared as precomputed fixed-length digests, so raw keys
// of different lengths never reach subtle.ConstantTimeCompare (which exits
// early on a length mismatch). Timing itself isn't asserted; this checks the
// digests are what's stored and compared.
func TestAuthenticatorStoresKeyDigests(t *testing.T) {
	cfg := &config.Config{
		Users: map[string]config.UserConfig{
			"bob":   {GatewayKey: secret.String("a-much-longer-key-for-bob")},
			"alice": {GatewayKey: secret.String("short")},
		},
		Settings: config.GatewaySettings{AuthKey: secret.String("shared")},
	}
	a := NewAuthenticator(cfg)
	if len(a.users) != 2 || a.users[0].name != "alice" || a.users[1].name != "bob" {
		t.Fatalf("users = %+v, want alice then bob", a.users)
	}
	for _, u := range a.users {
		if want := sha256.Sum256([]byte(string(cfg.Users[u.name].GatewayKey))); u.digest != want {
			t.Errorf("user %s: stored value is not sha256 of the key", u.name)
		}
	}
	if !a.hasAuthKey || a.authKeyDigest != sha256.Sum256([]byte("shared")) {
		t.Errorf("auth_key digest not precomputed")
	}
}

// P0.12: with keys of different lengths, only the exact key authenticates,
// and it maps to the right user.
func TestAuthenticatorDifferentLengthKeys(t *testing.T) {
	users := NewAuthenticator(&config.Config{Users: map[string]config.UserConfig{
		"alice": {GatewayKey: secret.String("short"), Upstream: "a-up"},
		"bob":   {GatewayKey: secret.String("a-much-longer-key-for-bob"), Upstream: "b-up"},
	}})
	shared := NewAuthenticator(&config.Config{Settings: config.GatewaySettings{AuthKey: secret.String("shared-key")}})

	cases := []struct {
		name      string
		auth      *Authenticator
		presented string
		wantOK    bool
		wantID    string
	}{
		{"alice exact", users, "short", true, "alice"},
		{"bob exact", users, "a-much-longer-key-for-bob", true, "bob"},
		{"alice prefix", users, "shor", false, ""},
		{"alice extended", users, "short-and-more", false, ""},
		{"bob prefix", users, "a-much-longer", false, ""},
		{"empty", users, "", false, ""},
		{"shared exact", shared, "shared-key", true, "anonymous"},
		{"shared prefix", shared, "shared", false, ""},
		{"shared extended", shared, "shared-key-x", false, ""},
		{"shared empty", shared, "", false, ""},
		{"nil authenticator", nil, "short", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tc.presented != "" {
				r.Header.Set("Authorization", "Bearer "+tc.presented)
			}
			res, ok := tc.auth.Authenticate(r)
			if ok != tc.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tc.wantOK)
			}
			if ok && res.ClientID != tc.wantID {
				t.Errorf("ClientID = %q, want %q", res.ClientID, tc.wantID)
			}
		})
	}
}
