package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/config"
)

// AuthResult carries the outcome of authenticating one request.
type AuthResult struct {
	ClientID string
	// Authenticated is true only when the credential matched a users-table
	// entry, making ClientID a verified identity. It is false for the shared
	// auth_key and open modes, where ClientID is the advisory x-client-id.
	Authenticated          bool
	UpstreamKeyEnvOverride string
	UpstreamOverride       string
}

// userKey is one users-table entry with its gateway key reduced to a
// fixed-length SHA-256 digest.
type userKey struct {
	name           string
	digest         [sha256.Size]byte
	upstreamKeyEnv string
	upstream       string
}

// Authenticator holds the credential digests for one config generation.
// Digests are computed once, in NewAuthenticator, so every comparison is
// over 32 fixed-length bytes: subtle.ConstantTimeCompare returns early on a
// length mismatch, so comparing raw keys would leak each key's length (P0.12).
type Authenticator struct {
	cfg           *config.Config
	users         []userKey // sorted by name
	authKeyDigest [sha256.Size]byte
	hasAuthKey    bool
}

// NewAuthenticator precomputes the key digests for cfg. Build it once per
// AppState generation, never per request.
func NewAuthenticator(cfg *config.Config) *Authenticator {
	a := &Authenticator{cfg: cfg}
	names := make([]string, 0, len(cfg.Users))
	for name := range cfg.Users {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		u := cfg.Users[name]
		a.users = append(a.users, userKey{
			name:           name,
			digest:         sha256.Sum256([]byte(string(u.GatewayKey))),
			upstreamKeyEnv: u.UpstreamKeyEnv,
			upstream:       u.Upstream,
		})
	}
	if cfg.Settings.AuthKey != "" {
		a.hasAuthKey = true
		a.authKeyDigest = sha256.Sum256([]byte(string(cfg.Settings.AuthKey)))
	}
	return a
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
//     sha256(presented) is compared in constant time against every user's
//     precomputed digest on every call (no early exit) — visiting all N
//     users is what makes the total comparison time independent of which
//     user matched, and this characteristic must be preserved; do not
//     "optimize" this into a keyed map lookup, which would reintroduce a
//     timing side channel.
//  2. Else, if settings.auth_key is set, compare against that single shared
//     key's digest in constant time.
//  3. Else, auth is open: client_id is the sanitized advisory x-client-id
//     header (default "anonymous") — NOT a security boundary.
//
// A nil Authenticator rejects every request (fail closed).
func (a *Authenticator) Authenticate(r *http.Request) (AuthResult, bool) {
	if a == nil {
		return AuthResult{}, false
	}
	presented := bearerOrGatewayKey(r)
	presentedDigest := sha256.Sum256([]byte(presented))

	if len(a.users) > 0 {
		matched := -1
		for i := range a.users {
			eq := subtle.ConstantTimeCompare(a.users[i].digest[:], presentedDigest[:])
			matched = subtle.ConstantTimeSelect(eq, i, matched)
		}
		if matched < 0 || presented == "" {
			return AuthResult{}, false
		}
		u := a.users[matched]
		return AuthResult{
			ClientID:               chatmodel.SanitizeClientID(u.name),
			Authenticated:          true,
			UpstreamKeyEnvOverride: u.upstreamKeyEnv,
			UpstreamOverride:       u.upstream,
		}, true
	}

	if a.hasAuthKey {
		eq := subtle.ConstantTimeCompare(a.authKeyDigest[:], presentedDigest[:])
		if presented == "" || eq != 1 {
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

// SourceIP returns the client address the rate limiters key anonymous
// clients on (P0.14). It starts from RemoteAddr and, only while the current
// hop is in settings.trusted_proxies, steps one X-Forwarded-For entry to the
// left. Proxies append, so entries left of the first untrusted hop are
// client-supplied and never believed. An unparseable entry stops the walk at
// the trusted proxy that sent it.
func SourceIP(cfg *config.Config, r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	current, ok := parseHop(host)
	if !ok {
		return host
	}
	trusted := cfg.Settings.TrustedProxyPrefixes
	if len(trusted) == 0 {
		return current.String()
	}

	var entries []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		entries = append(entries, strings.Split(v, ",")...)
	}
	for i := len(entries) - 1; i >= 0 && isTrusted(trusted, current); i-- {
		next, ok := parseHop(strings.TrimSpace(entries[i]))
		if !ok {
			break
		}
		current = next
	}
	return current.String()
}

// parseHop parses an IP, optionally with a port or brackets, dropping any
// zone and unmapping IPv4-in-IPv6.
func parseHop(s string) (netip.Addr, bool) {
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.WithZone("").Unmap(), true
}

func isTrusted(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
