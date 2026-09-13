package config

import (
	"fmt"
	"math"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
)

// Validate enforces the schema's referential-integrity rules: every route
// and default upstream must point at a declared upstream, and semantic
// caching (if enabled) must resolve to a configured upstream.
func Validate(cfg *Config) error {
	available := make([]string, 0, len(cfg.Upstreams))
	for name := range cfg.Upstreams {
		available = append(available, name)
	}
	sort.Strings(available)

	if err := validateUpstreams(cfg); err != nil {
		return err
	}

	for _, route := range cfg.ModelRoutes {
		for _, up := range route.Upstreams {
			if _, ok := cfg.Upstreams[up]; !ok {
				return fmt.Errorf("model_routes pattern %q references unknown upstream %q (available: %s)",
					route.Pattern, up, strings.Join(available, ", "))
			}
		}
	}

	if cfg.Settings.DefaultUpstream != "" {
		if _, ok := cfg.Upstreams[cfg.Settings.DefaultUpstream]; !ok {
			return fmt.Errorf("settings.default_upstream %q is not a declared upstream (available: %s)",
				cfg.Settings.DefaultUpstream, strings.Join(available, ", "))
		}
	}

	if cfg.Settings.MaxResponseBytes <= 0 {
		return fmt.Errorf("settings.max_response_bytes must be greater than 0")
	}
	if !(cfg.Settings.StreamTimeout > 0) { // also rejects NaN
		return fmt.Errorf("settings.stream_timeout must be greater than 0")
	}

	if err := validateResilience(cfg.Resilience); err != nil {
		return err
	}

	if err := validateAuth(cfg, available); err != nil {
		return err
	}

	if err := validateTrustedProxies(&cfg.Settings); err != nil {
		return err
	}

	for _, name := range sortedKeys(cfg.MiddlewareConfig) {
		if !slices.Contains(cfg.Middleware, name) {
			return fmt.Errorf("middleware_config.%s is set but %q is not listed in middleware:", name, name)
		}
	}

	if v, ok := cfg.MiddlewareConfig["context_pseudonymizer"]["persist_sessions_for_unauthenticated"]; ok {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("middleware_config.context_pseudonymizer.persist_sessions_for_unauthenticated must be a boolean")
		}
	}

	if cfg.Cache.Semantic.Enabled {
		effective := cfg.Cache.Semantic.Upstream
		if effective == "" {
			effective = cfg.Settings.DefaultUpstream
		}
		if effective == "" {
			return fmt.Errorf("cache.semantic.enabled is true but no semantic upstream and no settings.default_upstream is configured")
		}
		if _, ok := cfg.Upstreams[effective]; !ok {
			return fmt.Errorf("cache.semantic resolves to upstream %q which is not declared (available: %s)",
				effective, strings.Join(available, ", "))
		}
	}

	return nil
}

// validateResilience rejects retry and circuit settings that silently break
// resilience (P0.6): a negative retry count or delay, a failure threshold
// that opens the circuit on the first failure, and non-finite or
// non-positive durations. Health-check durations are checked even while
// disabled so enabling it later can't produce a zero-interval busy loop.
func validateResilience(r ResilienceConfig) error {
	finite := func(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
	if r.RetryAttempts < 0 {
		return fmt.Errorf("resilience.retry_attempts must be 0 or greater")
	}
	if !finite(r.RetryDelaySeconds) || r.RetryDelaySeconds < 0 {
		return fmt.Errorf("resilience.retry_delay_seconds must be a finite number, 0 or greater")
	}
	if r.CircuitBreaker.FailureThreshold <= 0 {
		return fmt.Errorf("resilience.circuit_breaker.failure_threshold must be greater than 0")
	}
	if !finite(r.CircuitBreaker.CooldownSeconds) || r.CircuitBreaker.CooldownSeconds < 0 {
		return fmt.Errorf("resilience.circuit_breaker.cooldown_seconds must be a finite number, 0 or greater")
	}
	if !finite(r.HealthCheck.IntervalSeconds) || !(r.HealthCheck.IntervalSeconds > 0) {
		return fmt.Errorf("resilience.health_check.interval_seconds must be a finite number greater than 0")
	}
	if !finite(r.HealthCheck.TimeoutSeconds) || !(r.HealthCheck.TimeoutSeconds > 0) {
		return fmt.Errorf("resilience.health_check.timeout_seconds must be a finite number greater than 0")
	}
	return nil
}

// supportedAPIFormats are the api_format values with a translator in this
// build (empty means openai). plannedAPIFormats are recognised but not yet
// implemented (P1.1, P1.2): accepting them would forward OpenAI-shaped
// requests to an endpoint that speaks something else (P0.13, P0.15).
var (
	supportedAPIFormats = []string{"", "openai"}
	plannedAPIFormats   = []string{"anthropic", "gemini"}
	// validAuthTypes are the auth_type values buildHeaders understands
	// (empty means bearer).
	validAuthTypes = []string{"", "bearer", "x-api-key", "none"}
)

// validateUpstreams rejects api_format and auth_type values that would
// otherwise fall back silently to openai and bearer.
func validateUpstreams(cfg *Config) error {
	for _, name := range sortedKeys(cfg.Upstreams) {
		up := cfg.Upstreams[name]
		switch {
		case slices.Contains(supportedAPIFormats, up.APIFormat):
		case slices.Contains(plannedAPIFormats, up.APIFormat):
			return fmt.Errorf("upstreams.%s.api_format %q is not yet supported (only openai is implemented)", name, up.APIFormat)
		default:
			return fmt.Errorf("upstreams.%s.api_format %q is unknown (valid: openai)", name, up.APIFormat)
		}
		if !slices.Contains(validAuthTypes, up.AuthType) {
			return fmt.Errorf("upstreams.%s.auth_type %q is unknown (valid: bearer, x-api-key, none)", name, up.AuthType)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateTrustedProxies rejects the removed trust_proxy_headers boolean and
// parses trusted_proxies into s.TrustedProxyPrefixes (P0.14). Bare IPs are
// accepted as single-host prefixes.
func validateTrustedProxies(s *GatewaySettings) error {
	if s.TrustProxyHeaders != nil {
		return fmt.Errorf("settings.trust_proxy_headers was removed; delete it and list your reverse proxies' CIDRs in settings.trusted_proxies")
	}
	prefixes := make([]netip.Prefix, 0, len(s.TrustedProxies))
	for _, raw := range s.TrustedProxies {
		entry := strings.TrimSpace(raw)
		p, err := netip.ParsePrefix(entry)
		if err != nil {
			addr, aerr := netip.ParseAddr(entry)
			if aerr != nil || addr.Zone() != "" {
				return fmt.Errorf("settings.trusted_proxies entry %q is not a CIDR or IP address", raw)
			}
			addr = addr.Unmap()
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		prefixes = append(prefixes, p.Masked())
	}
	s.TrustedProxyPrefixes = prefixes
	return nil
}

// templateAuthKey is the placeholder shown in the config template. A gateway
// still using it is effectively unauthenticated.
const templateAuthKey = "changeme"

// validateAuth rejects auth configurations that authenticate the wrong
// caller or none (P0.12). Error messages name users, never keys.
func validateAuth(cfg *Config, available []string) error {
	if strings.EqualFold(strings.TrimSpace(string(cfg.Settings.AuthKey)), templateAuthKey) {
		return fmt.Errorf("settings.auth_key is still the template placeholder; set a long random value")
	}

	names := make([]string, 0, len(cfg.Users))
	for name := range cfg.Users {
		names = append(names, name)
	}
	sort.Strings(names)

	keyOwner := make(map[string]string, len(names))
	sanitizedOwner := make(map[string]string, len(names))
	for _, name := range names {
		u := cfg.Users[name]
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("users: username must not be empty")
		}
		// Auth and pseudonymizer sessions key identities by the sanitized
		// username, so two names that sanitize alike would share them.
		sanitized := chatmodel.SanitizeClientID(name)
		if other, dup := sanitizedOwner[sanitized]; dup {
			return fmt.Errorf("users %q and %q map to the same client ID %q; rename one", other, name, sanitized)
		}
		sanitizedOwner[sanitized] = name

		key := string(u.GatewayKey)
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("users.%s.gateway_key (or gateway_key_env) must not be empty", name)
		}
		if other, dup := keyOwner[key]; dup {
			return fmt.Errorf("users %q and %q have the same gateway_key; each user needs a unique key", other, name)
		}
		keyOwner[key] = name

		if u.Upstream != "" {
			if _, ok := cfg.Upstreams[u.Upstream]; !ok {
				return fmt.Errorf("users.%s.upstream %q is not a declared upstream (available: %s)",
					name, u.Upstream, strings.Join(available, ", "))
			}
		}
	}
	return nil
}

// ResolveModelRoute returns the ordered list of candidate upstream names for
// a given model, applying (in priority order): a per-user forced upstream
// override, the first fnmatch-style matching model_routes pattern, then the
// default_upstream catch-all. Returns an error naming the model, configured
// patterns, and actionable guidance if nothing matches (surfaced as HTTP 400
// by the caller).
func ResolveModelRoute(cfg *Config, model string, userUpstream string) ([]string, error) {
	var order []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			order = append(order, name)
			seen[name] = true
		}
	}

	add(userUpstream)

	patterns := make([]string, 0, len(cfg.ModelRoutes))
	for _, route := range cfg.ModelRoutes {
		patterns = append(patterns, route.Pattern)
		if matchGlob(route.Pattern, model) {
			for _, up := range route.Upstreams {
				add(up)
			}
			break
		}
	}

	if len(order) == 0 {
		add(cfg.Settings.DefaultUpstream)
	}

	if len(order) == 0 {
		return nil, fmt.Errorf("no route found for model %q (configured patterns: %s; configure a matching model_routes entry or settings.default_upstream)",
			model, strings.Join(patterns, ", "))
	}
	return order, nil
}

// matchGlob implements fnmatch-style glob matching (only '*' and '?' are
// supported, matching the Python reference's fnmatch usage for model_routes
// patterns).
func matchGlob(pattern, s string) bool {
	return globMatch(pattern, s)
}

func globMatch(pattern, s string) bool {
	// Simple recursive glob matcher supporting '*' (any sequence) and '?'
	// (any single char). Sufficient for model-name patterns like "gpt-*".
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		if globMatch(pattern[1:], s) {
			return true
		}
		for i := 0; i < len(s); i++ {
			if globMatch(pattern[1:], s[i+1:]) {
				return true
			}
		}
		return false
	case '?':
		if len(s) == 0 {
			return false
		}
		return globMatch(pattern[1:], s[1:])
	default:
		if len(s) == 0 || s[0] != pattern[0] {
			return false
		}
		return globMatch(pattern[1:], s[1:])
	}
}
