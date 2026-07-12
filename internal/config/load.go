package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Load reads, defaults, and validates a rackctl.yaml file. On a validation
// error the (partially populated) config is still returned so callers can
// display it.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return &c, err
	}
	return &c, nil
}
