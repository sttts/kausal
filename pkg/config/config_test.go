package config

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.DriftDetection.DefaultMode != ModeLog {
		t.Errorf("expected default mode %q, got %q", ModeLog, cfg.DriftDetection.DefaultMode)
	}
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
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
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
			if mode != tt.wantMode {
				t.Errorf("GetModeForResource() = %v, want %v", mode, tt.wantMode)
			}
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
			if got != tt.want {
				t.Errorf("IsEnforceMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	// Create a temp directory for test files
	tempDir := t.TempDir()

	tests := []struct {
		name        string
		content     string
		wantErr     bool
		wantMode    string
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
			if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}

			cfg, err := Load(path)
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if cfg.DriftDetection.DefaultMode != tt.wantMode {
					t.Errorf("DefaultMode = %v, want %v", cfg.DriftDetection.DefaultMode, tt.wantMode)
				}
				if len(cfg.DriftDetection.Overrides) != tt.wantOverrides {
					t.Errorf("Overrides count = %v, want %v", len(cfg.DriftDetection.Overrides), tt.wantOverrides)
				}
			}
		})
	}

	// Test file not found
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("Load() should fail for nonexistent file")
	}
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
			if got != tt.want {
				t.Errorf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}
