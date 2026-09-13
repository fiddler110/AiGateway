package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/config"
)

// AuthResult carries the outcome of authenticating one request.
type AuthResult struct {
	ClientID           string
	UpstreamKeyEnvOverride string
	UpstreamOverride   string
}

// bearerOrGatewayKey extracts the presented credential from either the
// standard Authorization: Bearer header or the X-Gateway-Key fallback.
func bearerOrGatewayKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if token, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return token
		}
	}
	return r.Header.Get("X-Gateway-Key")
}

// Authenticate implements the gateway's three-tier auth model:
//  1. If any users are configured, that table is the SOLE auth mechanism.
//     Every configured user's key is compared in constant time on every
//     call (no early exit) — visiting all N users is what makes the total
//     comparison time independent of which user matched, and this
//     characteristic must be preserved; do not "optimize" this into a keyed
//     map lookup, which would reintroduce a timing side channel.
//  2. Else, if settings.auth_key is set, compare against that single shared
//     key in constant time.
//  3. Else, auth is open: client_id is the sanitized advisory x-client-id
//     header (default "anonymous") — NOT a security boundary.
func Authenticate(cfg *config.Config, r *http.Request) (AuthResult, bool) {
	presented := bearerOrGatewayKey(r)

	if len(cfg.Users) > 0 {
		presentedBytes := []byte(presented)
		matched := false
		var username string
		var upstreamKeyEnv, upstreamOverride string
		for uname, u := range cfg.Users {
			eq := subtle.ConstantTimeCompare([]byte(string(u.GatewayKey)), presentedBytes) == 1
			if eq {
				matched = true
				username = uname
				upstreamKeyEnv = u.UpstreamKeyEnv
				upstreamOverride = u.Upstream
			}
		}
		if !matched {
			return AuthResult{}, false
		}
		return AuthResult{
			ClientID:               chatmodel.SanitizeClientID(username),
			UpstreamKeyEnvOverride: upstreamKeyEnv,
			UpstreamOverride:       upstreamOverride,
		}, true
	}

	if cfg.Settings.AuthKey != "" {
		if presented == "" || subtle.ConstantTimeCompare([]byte(string(cfg.Settings.AuthKey)), []byte(presented)) != 1 {
			return AuthResult{}, false
		}
		clientID := chatmodel.SanitizeClientID(r.Header.Get("x-client-id"))
		if clientID == "" {
			clientID = "anonymous"
		}
		return AuthResult{ClientID: clientID}, true
	}

	clientID := chatmodel.SanitizeClientID(r.Header.Get("x-client-id"))
	if clientID == "" {
		clientID = "anonymous"
	}
	return AuthResult{ClientID: clientID}, true
}

// SourceIP returns the request's remote address, honoring
// X-Forwarded-For ONLY when settings.trust_proxy_headers is explicitly
// enabled (default false) — a security-sensitive setting since trusting
// client-supplied headers by default would let any client spoof its
// apparent source IP, which the rate limiter's anonymous-client fallback
// key relies on.
func SourceIP(cfg *config.Config, r *http.Request) string {
	if cfg.Settings.TrustProxyHeaders {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			parts := strings.Split(fwd, ",")
			return strings.TrimSpace(parts[0])
		}
	}
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		return host[:idx]
	}
	return host
}
