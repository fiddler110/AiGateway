// Package config defines the gateway's YAML configuration schema and loader.
//
// Security invariant: this schema never has a field capable of holding raw
// API key material. Upstreams and users reference environment variable
// NAMES (api_key_env, upstream_key_env, gateway_key is the gateway's own
// shared secret and can come from the AIGATEWAY_AUTH_KEY env var instead of
// settings.auth_key, or per user from gateway_key_env); actual upstream provider keys always
// come from the process environment, read fresh at request time.
package config

import (
	"net/netip"

	"github.com/scottymacleod/aigateway/internal/secret"
)

// UpstreamConfig describes one configured upstream AI provider/runtime.
type UpstreamConfig struct {
	BaseURL    string   `yaml:"base_url"`
	APIKeyEnv  string   `yaml:"api_key_env"`
	Timeout    float64  `yaml:"timeout"`
	ChatPath   string   `yaml:"chat_path"`
	AuthType   string   `yaml:"auth_type"` // bearer | x-api-key | none
	HealthPath string   `yaml:"health_path"`
	APIFormat  string   `yaml:"api_format"` // openai | anthropic | gemini
	Models     []string `yaml:"models"`
}

// ModelRoute maps a model-name glob pattern to an ordered list of upstreams
// (first = primary, rest = fallback). First matching pattern wins.
type ModelRoute struct {
	Pattern   string   `yaml:"pattern"`
	Upstreams []string `yaml:"upstreams"`
}

// UserConfig describes one entry in the multi-user auth table. Presence of
// ANY user makes this the sole auth mechanism (mutually exclusive with
// settings.auth_key).
//
// GatewayKeyEnv names an environment variable holding the user's gateway key,
// as an alternative to GatewayKey. Parse resolves it into GatewayKey before
// validation; setting both is an error.
type UserConfig struct {
	GatewayKey     secret.String `yaml:"gateway_key"`
	GatewayKeyEnv  string        `yaml:"gateway_key_env"`
	UpstreamKeyEnv string        `yaml:"upstream_key_env"`
	Upstream       string        `yaml:"upstream"`
}

type CircuitBreakerConfig struct {
	FailureThreshold int     `yaml:"failure_threshold"`
	CooldownSeconds  float64 `yaml:"cooldown_seconds"`
}

type HealthCheckConfig struct {
	Enabled         bool    `yaml:"enabled"`
	IntervalSeconds float64 `yaml:"interval_seconds"`
	TimeoutSeconds  float64 `yaml:"timeout_seconds"`
}

type ResilienceConfig struct {
	RetryAttempts     int                  `yaml:"retry_attempts"`
	RetryDelaySeconds float64              `yaml:"retry_delay_seconds"`
	CircuitBreaker    CircuitBreakerConfig `yaml:"circuit_breaker"`
	HealthCheck       HealthCheckConfig    `yaml:"health_check"`
}

type SemanticCacheConfig struct {
	Enabled             bool    `yaml:"enabled"`
	Upstream            string  `yaml:"upstream"`
	EmbeddingModel      string  `yaml:"embedding_model"`
	SimilarityThreshold float64 `yaml:"similarity_threshold"`
	MaxEntries          int     `yaml:"max_entries"`
}

type CacheConfig struct {
	Enabled    bool                `yaml:"enabled"`
	TTLSeconds int                 `yaml:"ttl_seconds"`
	MaxEntries int                 `yaml:"max_entries"`
	Semantic   SemanticCacheConfig `yaml:"semantic"`
}

type RedisConfig struct {
	Enabled   bool   `yaml:"enabled"`
	URL       string `yaml:"url"`
	KeyPrefix string `yaml:"key_prefix"`
}

// GatewaySettings holds gateway-wide operational settings.
//
// MaxResponseBytes bounds how many bytes of one upstream response body
// (non-streaming, buffered stream, or passthrough stream total) the gateway
// reads; exceeding it is an error, never a silent truncation. StreamTimeout
// bounds the total duration of a streaming request, in seconds like
// RequestTimeout (which covers non-streaming requests only). See P0.9.
type GatewaySettings struct {
	ListenPort       int           `yaml:"listen_port"`
	ListenHost       string        `yaml:"listen_host"`
	DefaultUpstream  string        `yaml:"default_upstream"`
	AuditDB          string        `yaml:"audit_db"`
	RequestTimeout   float64       `yaml:"request_timeout"`
	RetentionDays    int           `yaml:"retention_days"`
	LogLevel         string        `yaml:"log_level"`
	AuthKey          secret.String `yaml:"auth_key"`
	MaxRequestBytes  int64         `yaml:"max_request_bytes"`
	MaxResponseBytes int64         `yaml:"max_response_bytes"`
	StreamTimeout    float64       `yaml:"stream_timeout"`
	StreamBuffer     bool          `yaml:"stream_buffer"`
	// TrustedProxies lists CIDRs (or bare IPs) of reverse proxies whose
	// X-Forwarded-For entries are believed. Empty means XFF is ignored.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// TrustProxyHeaders is the removed boolean predecessor of TrustedProxies.
	// It is decoded only so Validate can warn on false and reject true with a
	// pointer to trusted_proxies, instead of a generic unknown-field error.
	TrustProxyHeaders *bool `yaml:"trust_proxy_headers"`

	// TrustedProxyPrefixes is TrustedProxies parsed by Validate. A config
	// that never went through Validate trusts no proxy.
	TrustedProxyPrefixes []netip.Prefix `yaml:"-"`
}

// Config is the top-level gateway configuration, loaded from YAML.
type Config struct {
	Upstreams        map[string]UpstreamConfig `yaml:"upstreams"`
	ModelRoutes      []ModelRoute              `yaml:"model_routes"`
	Middleware       []string                  `yaml:"middleware"`
	MiddlewareConfig map[string]map[string]any `yaml:"middleware_config"`
	Settings         GatewaySettings           `yaml:"settings"`
	Users            map[string]UserConfig     `yaml:"users"`
	Cache            CacheConfig               `yaml:"cache"`
	Redis            RedisConfig               `yaml:"redis"`
	Resilience       ResilienceConfig          `yaml:"resilience"`

	// Warnings are non-fatal problems found by Validate, such as settings
	// that are accepted but have no effect yet (P0.15). The caller logs
	// them at startup. They name settings, never values.
	Warnings []string `yaml:"-"`
}

// Defaults applies the gateway's documented default values to a freshly
// unmarshaled Config, matching the Python reference's safe-by-default
// posture (no upstreams configured => every request 400s with a clear
// error, rather than silently routing somewhere).
func Defaults() Config {
	return Config{
		Upstreams:   map[string]UpstreamConfig{},
		ModelRoutes: []ModelRoute{},
		Middleware:  []string{},
		Settings: GatewaySettings{
			ListenPort:       8080,
			ListenHost:       "0.0.0.0",
			AuditDB:          "logs/gateway.db",
			RequestTimeout:   300.0,
			RetentionDays:    90,
			LogLevel:         "info",
			MaxRequestBytes:  10 * 1024 * 1024,
			MaxResponseBytes: 32 * 1024 * 1024,
			StreamTimeout:    600.0,
		},
		Cache: CacheConfig{
			// Off until P2.1: sharing cached responses across clients is a
			// data-sharing decision the operator should make explicitly.
			Enabled:    false,
			TTLSeconds: 300,
			MaxEntries: 1000,
			Semantic: SemanticCacheConfig{
				EmbeddingModel:      "text-embedding-3-small",
				SimilarityThreshold: 0.97,
				MaxEntries:          500,
			},
		},
		Redis: RedisConfig{
			URL:       "redis://localhost:6379/0",
			KeyPrefix: "aigateway",
		},
		Resilience: ResilienceConfig{
			RetryAttempts:     2,
			RetryDelaySeconds: 1.0,
			CircuitBreaker: CircuitBreakerConfig{
				FailureThreshold: 5,
				CooldownSeconds:  60,
			},
			HealthCheck: HealthCheckConfig{
				Enabled:         true,
				IntervalSeconds: 30,
				TimeoutSeconds:  5,
			},
		},
	}
}
