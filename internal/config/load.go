package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/scottymacleod/aigateway/internal/secret"
)

// Load reads and validates a gateway config file, starting from Defaults()
// so any field omitted in YAML keeps its documented default value.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// AuthKeyEnv overrides settings.auth_key when set.
const AuthKeyEnv = "AIGATEWAY_AUTH_KEY"

// Parse decodes and validates YAML config bytes on top of Defaults().
// Unknown keys are rejected (P0.14): a typo like `midleware:` must not
// silently disable DLP. Env-sourced keys are resolved before Validate, so
// the auth checks apply to the effective keys.
func Parse(data []byte) (*Config, error) {
	cfg := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := resolveEnv(&cfg, os.LookupEnv); err != nil {
		return nil, fmt.Errorf("invalid: %w", err)
	}
	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid: %w", err)
	}
	return &cfg, nil
}

// resolveEnv applies AIGATEWAY_AUTH_KEY and each user's gateway_key_env.
// Errors name variables and users, never values. A named variable that is
// unset or empty is an error rather than a silent fallback to open auth.
func resolveEnv(cfg *Config, lookup func(string) (string, bool)) error {
	if v, ok := lookup(AuthKeyEnv); ok {
		if v == "" {
			return fmt.Errorf("%s is set but empty; unset it or give it a value", AuthKeyEnv)
		}
		cfg.Settings.AuthKey = secret.String(v)
	}
	for name, u := range cfg.Users {
		if u.GatewayKeyEnv == "" {
			continue
		}
		if u.GatewayKey != "" {
			return fmt.Errorf("users.%s sets both gateway_key and gateway_key_env; use one", name)
		}
		v, ok := lookup(u.GatewayKeyEnv)
		if !ok || v == "" {
			return fmt.Errorf("users.%s.gateway_key_env names %q, which is unset or empty", name, u.GatewayKeyEnv)
		}
		u.GatewayKey = secret.String(v)
		cfg.Users[name] = u
	}
	return nil
}
