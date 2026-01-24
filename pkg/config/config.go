// Package config provides configuration types and loading for kausality.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Config is the root configuration structure.
type Config struct {
	DriftDetection DriftDetectionConfig `yaml:"driftDetection"`
}

// DriftDetectionConfig configures drift detection behavior.
type DriftDetectionConfig struct {
	// DefaultMode is the default drift detection mode ("log" or "enforce").
	DefaultMode string `yaml:"defaultMode"`

	// Overrides allows per-resource drift detection configuration.
	Overrides []DriftDetectionOverride `yaml:"overrides,omitempty"`
}

// DriftDetectionOverride configures drift detection for specific resources.
type DriftDetectionOverride struct {
	// APIGroups specifies which API groups this override applies to.
	// Empty string "" matches core group.
	APIGroups []string `yaml:"apiGroups"`

	// Resources specifies which resources this override applies to.
	// "*" matches all resources in the API groups.
	Resources []string `yaml:"resources"`

	// Mode is the drift detection mode for matching resources ("log" or "enforce").
	Mode string `yaml:"mode"`
}

// Mode constants.
const (
	ModeLog     = "log"
	ModeEnforce = "enforce"
)

// Load reads configuration from a YAML file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults
	if cfg.DriftDetection.DefaultMode == "" {
		cfg.DriftDetection.DefaultMode = ModeLog
	}

	// Validate
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// Validate checks that the configuration is valid.
func (c *Config) Validate() error {
	if !isValidMode(c.DriftDetection.DefaultMode) {
		return fmt.Errorf("invalid defaultMode %q: must be %q or %q", c.DriftDetection.DefaultMode, ModeLog, ModeEnforce)
	}

	for i, override := range c.DriftDetection.Overrides {
		if len(override.APIGroups) == 0 {
			return fmt.Errorf("override[%d]: apiGroups must not be empty", i)
		}
		if len(override.Resources) == 0 {
			return fmt.Errorf("override[%d]: resources must not be empty", i)
		}
		if !isValidMode(override.Mode) {
			return fmt.Errorf("override[%d]: invalid mode %q: must be %q or %q", i, override.Mode, ModeLog, ModeEnforce)
		}
	}

	return nil
}

// GetModeForResource returns the drift detection mode for a specific resource.
func (c *Config) GetModeForResource(gvk schema.GroupVersionKind) string {
	// Check overrides first (first match wins)
	for _, override := range c.DriftDetection.Overrides {
		if override.Matches(gvk) {
			return override.Mode
		}
	}

	return c.DriftDetection.DefaultMode
}

// IsEnforceMode returns true if the given resource should be in enforce mode.
func (c *Config) IsEnforceMode(gvk schema.GroupVersionKind) bool {
	return c.GetModeForResource(gvk) == ModeEnforce
}

// Matches returns true if this override applies to the given GVK.
func (o *DriftDetectionOverride) Matches(gvk schema.GroupVersionKind) bool {
	// Check API group
	groupMatches := false
	for _, g := range o.APIGroups {
		if g == gvk.Group {
			groupMatches = true
			break
		}
	}
	if !groupMatches {
		return false
	}

	// Check resource (convert Kind to resource name - lowercase plural)
	resource := strings.ToLower(gvk.Kind) + "s"
	for _, r := range o.Resources {
		if r == "*" || r == resource {
			return true
		}
	}

	return false
}

func isValidMode(mode string) bool {
	return mode == ModeLog || mode == ModeEnforce
}

// Default returns a default configuration with log mode.
func Default() *Config {
	return &Config{
		DriftDetection: DriftDetectionConfig{
			DefaultMode: ModeLog,
		},
	}
}
