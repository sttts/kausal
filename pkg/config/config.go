// Package config provides configuration types and loading for kausality.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Config is the root configuration structure.
type Config struct {
	DriftDetection DriftDetectionConfig `yaml:"driftDetection"`
	// Backends configures drift report webhook endpoints.
	// Reports are sent to all configured backends in parallel.
	Backends []BackendConfig `yaml:"backends,omitempty"`
}

// BackendConfig configures a drift report webhook endpoint.
type BackendConfig struct {
	// URL is the webhook endpoint URL.
	URL string `yaml:"url"`
	// CAFile is the path to the CA certificate file for TLS verification.
	// If empty, system CA pool is used.
	CAFile string `yaml:"caFile,omitempty"`
	// Timeout is the request timeout. Default is 10 seconds.
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// RetryCount is the number of retries on failure. Default is 3.
	RetryCount int `yaml:"retryCount,omitempty"`
	// RetryInterval is the interval between retries. Default is 1 second.
	RetryInterval time.Duration `yaml:"retryInterval,omitempty"`
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

	// Namespaces specifies which namespaces this override applies to.
	// Empty list matches all namespaces.
	Namespaces []string `yaml:"namespaces,omitempty"`

	// NamespaceSelector selects namespaces by labels.
	// Empty selector matches all namespaces.
	NamespaceSelector *metav1.LabelSelector `yaml:"namespaceSelector,omitempty"`

	// ObjectSelector selects objects by labels.
	// Empty selector matches all objects.
	ObjectSelector *metav1.LabelSelector `yaml:"objectSelector,omitempty"`

	// Mode is the drift detection mode for matching resources ("log" or "enforce").
	Mode string `yaml:"mode"`
}

// ResourceContext provides context for mode matching.
type ResourceContext struct {
	// GVK is the GroupVersionKind of the resource.
	GVK schema.GroupVersionKind
	// Namespace is the namespace of the resource.
	Namespace string
	// ObjectLabels are the labels on the resource.
	ObjectLabels map[string]string
	// NamespaceLabels are the labels on the namespace.
	NamespaceLabels map[string]string
}

// Mode constants.
const (
	ModeLog     = "log"
	ModeEnforce = "enforce"
)

// ModeAnnotation is the annotation key for runtime mode configuration.
const ModeAnnotation = "kausality.io/mode"

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
// Deprecated: Use GetModeForResourceContext for full selector support.
func (c *Config) GetModeForResource(gvk schema.GroupVersionKind) string {
	return c.GetModeForResourceContext(ResourceContext{GVK: gvk})
}

// GetModeForResourceContext returns the drift detection mode using full context.
func (c *Config) GetModeForResourceContext(ctx ResourceContext) string {
	// Check overrides first (first match wins)
	for _, override := range c.DriftDetection.Overrides {
		if override.MatchesContext(ctx) {
			return override.Mode
		}
	}

	return c.DriftDetection.DefaultMode
}

// IsEnforceMode returns true if the given resource should be in enforce mode.
// Deprecated: Use IsEnforceModeContext for full selector support.
func (c *Config) IsEnforceMode(gvk schema.GroupVersionKind) bool {
	return c.GetModeForResource(gvk) == ModeEnforce
}

// IsEnforceModeContext returns true if the given resource context should be in enforce mode.
func (c *Config) IsEnforceModeContext(ctx ResourceContext) bool {
	return c.GetModeForResourceContext(ctx) == ModeEnforce
}

// ResolveModeWithAnnotations resolves the enforcement mode using annotations and config.
// Precedence (most specific wins):
// 1. Object annotation kausality.io/mode
// 2. Namespace annotation kausality.io/mode
// 3. Config-based mode (overrides + default)
func (c *Config) ResolveModeWithAnnotations(objectAnnotations, namespaceAnnotations map[string]string, ctx ResourceContext) string {
	// Check object annotation first
	if mode := objectAnnotations[ModeAnnotation]; isValidMode(mode) {
		return mode
	}

	// Check namespace annotation second
	if mode := namespaceAnnotations[ModeAnnotation]; isValidMode(mode) {
		return mode
	}

	// Fall back to config-based resolution
	return c.GetModeForResourceContext(ctx)
}

// IsEnforceModeWithAnnotations returns true if enforcement mode should be used.
// Uses annotation-based resolution with config fallback.
func (c *Config) IsEnforceModeWithAnnotations(objectAnnotations, namespaceAnnotations map[string]string, ctx ResourceContext) bool {
	return c.ResolveModeWithAnnotations(objectAnnotations, namespaceAnnotations, ctx) == ModeEnforce
}

// Matches returns true if this override applies to the given GVK.
// Deprecated: Use MatchesContext for full selector support.
func (o *DriftDetectionOverride) Matches(gvk schema.GroupVersionKind) bool {
	return o.MatchesContext(ResourceContext{GVK: gvk})
}

// MatchesContext returns true if this override applies to the given context.
func (o *DriftDetectionOverride) MatchesContext(ctx ResourceContext) bool {
	// Check API group
	if !o.matchesAPIGroup(ctx.GVK.Group) {
		return false
	}

	// Check resource
	if !o.matchesResource(ctx.GVK.Kind) {
		return false
	}

	// Check namespace name list
	if len(o.Namespaces) > 0 && !o.matchesNamespace(ctx.Namespace) {
		return false
	}

	// Check namespace selector
	if o.NamespaceSelector != nil && !o.matchesNamespaceSelector(ctx.NamespaceLabels) {
		return false
	}

	// Check object selector
	if o.ObjectSelector != nil && !o.matchesObjectSelector(ctx.ObjectLabels) {
		return false
	}

	return true
}

func (o *DriftDetectionOverride) matchesAPIGroup(group string) bool {
	for _, g := range o.APIGroups {
		if g == group {
			return true
		}
	}
	return false
}

func (o *DriftDetectionOverride) matchesResource(kind string) bool {
	// Convert Kind to resource name - lowercase plural
	resource := strings.ToLower(kind) + "s"
	for _, r := range o.Resources {
		if r == "*" || r == resource {
			return true
		}
	}
	return false
}

func (o *DriftDetectionOverride) matchesNamespace(namespace string) bool {
	for _, ns := range o.Namespaces {
		if ns == namespace {
			return true
		}
	}
	return false
}

func (o *DriftDetectionOverride) matchesNamespaceSelector(nsLabels map[string]string) bool {
	if o.NamespaceSelector == nil {
		return true
	}
	selector, err := metav1.LabelSelectorAsSelector(o.NamespaceSelector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(nsLabels))
}

func (o *DriftDetectionOverride) matchesObjectSelector(objLabels map[string]string) bool {
	if o.ObjectSelector == nil {
		return true
	}
	selector, err := metav1.LabelSelectorAsSelector(o.ObjectSelector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(objLabels))
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
