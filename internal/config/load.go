package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
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

// Parse decodes and validates YAML config bytes on top of Defaults().
func Parse(data []byte) (*Config, error) {
	cfg := Defaults()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid: %w", err)
	}
	return &cfg, nil
}
