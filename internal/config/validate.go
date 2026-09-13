package config

import (
	"fmt"
	"sort"
	"strings"
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
