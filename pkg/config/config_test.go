package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	assert.Equal(t, ModeLog, cfg.DriftDetection.DefaultMode)
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "valid log mode",
			config: Config{
				DriftDetection: DriftDetectionConfig{
					DefaultMode: ModeLog,
				},
			},
			wantErr: false,
		},
		{
			name: "valid enforce mode",
			config: Config{
				DriftDetection: DriftDetectionConfig{
					DefaultMode: ModeEnforce,
				},
			},
			wantErr: false,
		},
		{
			name: "invalid default mode",
			config: Config{
				DriftDetection: DriftDetectionConfig{
					DefaultMode: "invalid",
				},
			},
			wantErr: true,
		},
		{
			name: "valid with overrides",
			config: Config{
				DriftDetection: DriftDetectionConfig{
					DefaultMode: ModeLog,
					Overrides: []DriftDetectionOverride{
						{
							APIGroups: []string{"apps"},
							Resources: []string{"deployments"},
							Mode:      ModeEnforce,
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid override - empty apiGroups",
			config: Config{
				DriftDetection: DriftDetectionConfig{
					DefaultMode: ModeLog,
					Overrides: []DriftDetectionOverride{
						{
							APIGroups: []string{},
							Resources: []string{"deployments"},
							Mode:      ModeEnforce,
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid override - empty resources",
			config: Config{
				DriftDetection: DriftDetectionConfig{
					DefaultMode: ModeLog,
					Overrides: []DriftDetectionOverride{
						{
							APIGroups: []string{"apps"},
							Resources: []string{},
							Mode:      ModeEnforce,
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid override - invalid mode",
			config: Config{
				DriftDetection: DriftDetectionConfig{
					DefaultMode: ModeLog,
					Overrides: []DriftDetectionOverride{
						{
							APIGroups: []string{"apps"},
							Resources: []string{"deployments"},
							Mode:      "invalid",
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGetModeForResource(t *testing.T) {
	cfg := &Config{
		DriftDetection: DriftDetectionConfig{
			DefaultMode: ModeLog,
			Overrides: []DriftDetectionOverride{
				{
					APIGroups: []string{"apps"},
					Resources: []string{"deployments"},
					Mode:      ModeEnforce,
				},
				{
					APIGroups: []string{"example.com"},
					Resources: []string{"*"},
					Mode:      ModeEnforce,
				},
				{
					APIGroups: []string{""},
					Resources: []string{"configmaps"},
					Mode:      ModeLog,
				},
			},
		},
	}

	tests := []struct {
		name     string
		gvk      schema.GroupVersionKind
		wantMode string
	}{
		{
			name:     "deployment matches override",
			gvk:      schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			wantMode: ModeEnforce,
		},
		{
			name:     "replicaset uses default",
			gvk:      schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			wantMode: ModeLog,
		},
		{
			name:     "custom resource matches wildcard",
			gvk:      schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "MyResource"},
			wantMode: ModeEnforce,
		},
		{
			name:     "core configmap matches override",
			gvk:      schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			wantMode: ModeLog,
		},
		{
			name:     "core secret uses default",
			gvk:      schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"},
			wantMode: ModeLog,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode := cfg.GetModeForResource(tt.gvk)
			assert.Equal(t, tt.wantMode, mode)
		})
	}
}

func TestIsEnforceMode(t *testing.T) {
	cfg := &Config{
		DriftDetection: DriftDetectionConfig{
			DefaultMode: ModeLog,
			Overrides: []DriftDetectionOverride{
				{
					APIGroups: []string{"apps"},
					Resources: []string{"deployments"},
					Mode:      ModeEnforce,
				},
			},
		},
	}

	tests := []struct {
		name string
		gvk  schema.GroupVersionKind
		want bool
	}{
		{
			name: "deployment is enforce",
			gvk:  schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			want: true,
		},
		{
			name: "pod is not enforce",
			gvk:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.IsEnforceMode(tt.gvk)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoad(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name          string
		content       string
		wantErr       bool
		wantMode      string
		wantOverrides int
	}{
		{
			name: "valid config",
			content: `
driftDetection:
  defaultMode: enforce
  overrides:
    - apiGroups: ["apps"]
      resources: ["deployments"]
      mode: log
`,
			wantErr:       false,
			wantMode:      ModeEnforce,
			wantOverrides: 1,
		},
		{
			name: "minimal config",
			content: `
driftDetection:
  defaultMode: log
`,
			wantErr:       false,
			wantMode:      ModeLog,
			wantOverrides: 0,
		},
		{
			name: "empty defaultMode uses default",
			content: `
driftDetection: {}
`,
			wantErr:       false,
			wantMode:      ModeLog,
			wantOverrides: 0,
		},
		{
			name: "invalid mode",
			content: `
driftDetection:
  defaultMode: invalid
`,
			wantErr: true,
		},
		{
			name:    "invalid yaml",
			content: "not: valid: yaml: here",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(tempDir, tt.name+".yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0644))

			cfg, err := Load(path)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantMode, cfg.DriftDetection.DefaultMode)
			assert.Len(t, cfg.DriftDetection.Overrides, tt.wantOverrides)
		})
	}

	// Test file not found
	_, err := Load("/nonexistent/path/config.yaml")
	assert.Error(t, err)
}

func TestOverrideMatches(t *testing.T) {
	tests := []struct {
		name     string
		override DriftDetectionOverride
		gvk      schema.GroupVersionKind
		want     bool
	}{
		{
			name: "exact match",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				Mode:      ModeEnforce,
			},
			gvk:  schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			want: true,
		},
		{
			name: "wildcard resource",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"*"},
				Mode:      ModeEnforce,
			},
			gvk:  schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			want: true,
		},
		{
			name: "multiple apiGroups",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps", "extensions"},
				Resources: []string{"deployments"},
				Mode:      ModeEnforce,
			},
			gvk:  schema.GroupVersionKind{Group: "extensions", Version: "v1beta1", Kind: "Deployment"},
			want: true,
		},
		{
			name: "core group empty string",
			override: DriftDetectionOverride{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Mode:      ModeEnforce,
			},
			gvk:  schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
			want: true,
		},
		{
			name: "no match - wrong group",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				Mode:      ModeEnforce,
			},
			gvk:  schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"},
			want: false,
		},
		{
			name: "no match - wrong resource",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				Mode:      ModeEnforce,
			},
			gvk:  schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.override.Matches(tt.gvk)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOverrideMatchesContext_Namespaces(t *testing.T) {
	tests := []struct {
		name     string
		override DriftDetectionOverride
		ctx      ResourceContext
		want     bool
	}{
		{
			name: "namespace in list",
			override: DriftDetectionOverride{
				APIGroups:  []string{"apps"},
				Resources:  []string{"deployments"},
				Namespaces: []string{"production", "staging"},
				Mode:       ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "production",
			},
			want: true,
		},
		{
			name: "namespace not in list",
			override: DriftDetectionOverride{
				APIGroups:  []string{"apps"},
				Resources:  []string{"deployments"},
				Namespaces: []string{"production", "staging"},
				Mode:       ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "development",
			},
			want: false,
		},
		{
			name: "empty namespace list matches all",
			override: DriftDetectionOverride{
				APIGroups:  []string{"apps"},
				Resources:  []string{"deployments"},
				Namespaces: []string{},
				Mode:       ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "any-namespace",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.override.MatchesContext(tt.ctx)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOverrideMatchesContext_NamespaceSelector(t *testing.T) {
	tests := []struct {
		name     string
		override DriftDetectionOverride
		ctx      ResourceContext
		want     bool
	}{
		{
			name: "namespace labels match selector",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"env": "production"},
				},
				Mode: ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:             schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace:       "my-namespace",
				NamespaceLabels: map[string]string{"env": "production", "team": "platform"},
			},
			want: true,
		},
		{
			name: "namespace labels do not match selector",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"env": "production"},
				},
				Mode: ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:             schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace:       "my-namespace",
				NamespaceLabels: map[string]string{"env": "staging"},
			},
			want: false,
		},
		{
			name: "namespace selector with matchExpressions",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "env",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"production", "staging"},
						},
					},
				},
				Mode: ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:             schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace:       "my-namespace",
				NamespaceLabels: map[string]string{"env": "staging"},
			},
			want: true,
		},
		{
			name: "nil namespace selector matches all",
			override: DriftDetectionOverride{
				APIGroups:         []string{"apps"},
				Resources:         []string{"deployments"},
				NamespaceSelector: nil,
				Mode:              ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:             schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace:       "my-namespace",
				NamespaceLabels: map[string]string{},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.override.MatchesContext(tt.ctx)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOverrideMatchesContext_ObjectSelector(t *testing.T) {
	tests := []struct {
		name     string
		override DriftDetectionOverride
		ctx      ResourceContext
		want     bool
	}{
		{
			name: "object labels match selector",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				ObjectSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "critical"},
				},
				Mode: ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:          schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace:    "default",
				ObjectLabels: map[string]string{"app": "critical", "version": "v1"},
			},
			want: true,
		},
		{
			name: "object labels do not match selector",
			override: DriftDetectionOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				ObjectSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "critical"},
				},
				Mode: ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:          schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace:    "default",
				ObjectLabels: map[string]string{"app": "normal"},
			},
			want: false,
		},
		{
			name: "nil object selector matches all",
			override: DriftDetectionOverride{
				APIGroups:      []string{"apps"},
				Resources:      []string{"deployments"},
				ObjectSelector: nil,
				Mode:           ModeEnforce,
			},
			ctx: ResourceContext{
				GVK:          schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace:    "default",
				ObjectLabels: map[string]string{"any": "label"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.override.MatchesContext(tt.ctx)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveModeWithAnnotations(t *testing.T) {
	cfg := &Config{
		DriftDetection: DriftDetectionConfig{
			DefaultMode: ModeLog,
			Overrides: []DriftDetectionOverride{
				{
					APIGroups:  []string{"apps"},
					Resources:  []string{"deployments"},
					Namespaces: []string{"enforce-ns"},
					Mode:       ModeEnforce,
				},
			},
		},
	}

	tests := []struct {
		name          string
		objectAnns    map[string]string
		namespaceAnns map[string]string
		ctx           ResourceContext
		wantMode      string
	}{
		{
			name:          "object annotation enforce takes precedence",
			objectAnns:    map[string]string{ModeAnnotation: ModeEnforce},
			namespaceAnns: map[string]string{ModeAnnotation: ModeLog},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "default",
			},
			wantMode: ModeEnforce,
		},
		{
			name:          "object annotation log overrides namespace enforce",
			objectAnns:    map[string]string{ModeAnnotation: ModeLog},
			namespaceAnns: map[string]string{ModeAnnotation: ModeEnforce},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "default",
			},
			wantMode: ModeLog,
		},
		{
			name:          "namespace annotation takes precedence over config",
			objectAnns:    map[string]string{},
			namespaceAnns: map[string]string{ModeAnnotation: ModeEnforce},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "default",
			},
			wantMode: ModeEnforce,
		},
		{
			name:          "falls back to config override",
			objectAnns:    map[string]string{},
			namespaceAnns: map[string]string{},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "enforce-ns",
			},
			wantMode: ModeEnforce,
		},
		{
			name:          "falls back to config default",
			objectAnns:    map[string]string{},
			namespaceAnns: map[string]string{},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "default",
			},
			wantMode: ModeLog,
		},
		{
			name:          "invalid object annotation ignored",
			objectAnns:    map[string]string{ModeAnnotation: "invalid"},
			namespaceAnns: map[string]string{ModeAnnotation: ModeEnforce},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "default",
			},
			wantMode: ModeEnforce,
		},
		{
			name:          "invalid namespace annotation ignored",
			objectAnns:    map[string]string{},
			namespaceAnns: map[string]string{ModeAnnotation: "invalid"},
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "default",
			},
			wantMode: ModeLog,
		},
		{
			name:          "nil annotations handled",
			objectAnns:    nil,
			namespaceAnns: nil,
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "default",
			},
			wantMode: ModeLog,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode := cfg.ResolveModeWithAnnotations(tt.objectAnns, tt.namespaceAnns, tt.ctx)
			assert.Equal(t, tt.wantMode, mode)
		})
	}
}

func TestIsEnforceModeWithAnnotations(t *testing.T) {
	cfg := Default()

	tests := []struct {
		name          string
		objectAnns    map[string]string
		namespaceAnns map[string]string
		want          bool
	}{
		{
			name:          "enforce from object annotation",
			objectAnns:    map[string]string{ModeAnnotation: ModeEnforce},
			namespaceAnns: map[string]string{},
			want:          true,
		},
		{
			name:          "enforce from namespace annotation",
			objectAnns:    map[string]string{},
			namespaceAnns: map[string]string{ModeAnnotation: ModeEnforce},
			want:          true,
		},
		{
			name:          "log from default",
			objectAnns:    map[string]string{},
			namespaceAnns: map[string]string{},
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.IsEnforceModeWithAnnotations(tt.objectAnns, tt.namespaceAnns, ResourceContext{})
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetModeForResourceContext(t *testing.T) {
	cfg := &Config{
		DriftDetection: DriftDetectionConfig{
			DefaultMode: ModeLog,
			Overrides: []DriftDetectionOverride{
				{
					APIGroups:  []string{"apps"},
					Resources:  []string{"deployments"},
					Namespaces: []string{"production"},
					Mode:       ModeEnforce,
				},
				{
					APIGroups: []string{"apps"},
					Resources: []string{"statefulsets"},
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"critical": "true"},
					},
					Mode: ModeEnforce,
				},
				{
					APIGroups: []string{""},
					Resources: []string{"configmaps"},
					ObjectSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"protected": "true"},
					},
					Mode: ModeEnforce,
				},
			},
		},
	}

	tests := []struct {
		name     string
		ctx      ResourceContext
		wantMode string
	}{
		{
			name: "deployment in production namespace",
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "production",
			},
			wantMode: ModeEnforce,
		},
		{
			name: "deployment in staging namespace",
			ctx: ResourceContext{
				GVK:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				Namespace: "staging",
			},
			wantMode: ModeLog,
		},
		{
			name: "statefulset in critical namespace",
			ctx: ResourceContext{
				GVK:             schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"},
				Namespace:       "my-ns",
				NamespaceLabels: map[string]string{"critical": "true"},
			},
			wantMode: ModeEnforce,
		},
		{
			name: "statefulset in non-critical namespace",
			ctx: ResourceContext{
				GVK:             schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"},
				Namespace:       "my-ns",
				NamespaceLabels: map[string]string{"critical": "false"},
			},
			wantMode: ModeLog,
		},
		{
			name: "protected configmap",
			ctx: ResourceContext{
				GVK:          schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
				Namespace:    "default",
				ObjectLabels: map[string]string{"protected": "true"},
			},
			wantMode: ModeEnforce,
		},
		{
			name: "non-protected configmap",
			ctx: ResourceContext{
				GVK:          schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
				Namespace:    "default",
				ObjectLabels: map[string]string{"protected": "false"},
			},
			wantMode: ModeLog,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode := cfg.GetModeForResourceContext(tt.ctx)
			assert.Equal(t, tt.wantMode, mode)
		})
	}
}

func TestLoad_WithBackends(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name         string
		content      string
		wantErr      bool
		wantBackends int
		checkBackend func(t *testing.T, cfg *Config)
	}{
		{
			name: "single backend",
			content: `
driftDetection:
  defaultMode: log
backends:
  - url: https://backend1.example.com/webhook
    timeout: 10s
    retryCount: 3
    retryInterval: 1s
`,
			wantBackends: 1,
			checkBackend: func(t *testing.T, cfg *Config) {
				b := cfg.Backends[0]
				assert.Equal(t, "https://backend1.example.com/webhook", b.URL)
				assert.Equal(t, 10*time.Second, b.Timeout)
				assert.Equal(t, 3, b.RetryCount)
				assert.Equal(t, 1*time.Second, b.RetryInterval)
			},
		},
		{
			name: "multiple backends",
			content: `
driftDetection:
  defaultMode: log
backends:
  - url: https://backend1.example.com/webhook
    timeout: 10s
  - url: https://backend2.example.com/webhook
    caFile: /path/to/ca.crt
    timeout: 5s
  - url: https://backend3.example.com/webhook
`,
			wantBackends: 3,
			checkBackend: func(t *testing.T, cfg *Config) {
				assert.Equal(t, "https://backend1.example.com/webhook", cfg.Backends[0].URL)
				assert.Equal(t, "https://backend2.example.com/webhook", cfg.Backends[1].URL)
				assert.Equal(t, "/path/to/ca.crt", cfg.Backends[1].CAFile)
				assert.Equal(t, "https://backend3.example.com/webhook", cfg.Backends[2].URL)
			},
		},
		{
			name: "no backends",
			content: `
driftDetection:
  defaultMode: log
`,
			wantBackends: 0,
		},
		{
			name: "empty backends array",
			content: `
driftDetection:
  defaultMode: log
backends: []
`,
			wantBackends: 0,
		},
		{
			name: "backend with all options",
			content: `
driftDetection:
  defaultMode: log
backends:
  - url: https://secure.example.com/webhook
    caFile: /etc/ssl/ca.crt
    timeout: 30s
    retryCount: 5
    retryInterval: 2s
`,
			wantBackends: 1,
			checkBackend: func(t *testing.T, cfg *Config) {
				b := cfg.Backends[0]
				assert.Equal(t, "https://secure.example.com/webhook", b.URL)
				assert.Equal(t, "/etc/ssl/ca.crt", b.CAFile)
				assert.Equal(t, 30*time.Second, b.Timeout)
				assert.Equal(t, 5, b.RetryCount)
				assert.Equal(t, 2*time.Second, b.RetryInterval)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(tempDir, tt.name+".yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0644))

			cfg, err := Load(path)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, cfg.Backends, tt.wantBackends)

			if tt.checkBackend != nil {
				tt.checkBackend(t, cfg)
			}
		})
	}
}
