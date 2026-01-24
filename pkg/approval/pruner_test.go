package approval

import (
	"testing"
)

func TestPruner_ConsumeOnce(t *testing.T) {
	pruner := NewPruner()

	tests := []struct {
		name       string
		approvals  []Approval
		consumed   *Approval
		wantLen    int
		wantChange bool
	}{
		{
			name:       "nil consumed",
			approvals:  []Approval{{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Mode: ModeOnce}},
			consumed:   nil,
			wantLen:    1,
			wantChange: false,
		},
		{
			name:       "consume once approval",
			approvals:  []Approval{{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ModeOnce}},
			consumed:   &Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ModeOnce},
			wantLen:    0,
			wantChange: true,
		},
		{
			name: "consume one of multiple",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ModeOnce},
				{APIVersion: "v1", Kind: "ConfigMap", Name: "b", Generation: 5, Mode: ModeOnce},
			},
			consumed:   &Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ModeOnce},
			wantLen:    1,
			wantChange: true,
		},
		{
			name:       "don't consume mode=always",
			approvals:  []Approval{{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Mode: ModeAlways}},
			consumed:   &Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Mode: ModeAlways},
			wantLen:    1,
			wantChange: false,
		},
		{
			name:       "don't consume mode=generation",
			approvals:  []Approval{{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ModeGeneration}},
			consumed:   &Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ModeGeneration},
			wantLen:    1,
			wantChange: false,
		},
		{
			name:       "consume default mode (empty = once)",
			approvals:  []Approval{{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ""}},
			consumed:   &Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ""},
			wantLen:    0,
			wantChange: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, changed := pruner.ConsumeOnce(tt.approvals, tt.consumed)
			if len(result) != tt.wantLen {
				t.Errorf("len(result) = %d, want %d", len(result), tt.wantLen)
			}
			if changed != tt.wantChange {
				t.Errorf("changed = %v, want %v", changed, tt.wantChange)
			}
		})
	}
}

func TestPruner_PruneStale(t *testing.T) {
	pruner := NewPruner()

	tests := []struct {
		name             string
		approvals        []Approval
		parentGeneration int64
		wantLen          int
		wantNames        []string
	}{
		{
			name:             "empty list",
			approvals:        nil,
			parentGeneration: 5,
			wantLen:          0,
		},
		{
			name: "keep mode=always",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Mode: ModeAlways},
			},
			parentGeneration: 99,
			wantLen:          1,
			wantNames:        []string{"a"},
		},
		{
			name: "keep current generation",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 5, Mode: ModeOnce},
			},
			parentGeneration: 5,
			wantLen:          1,
			wantNames:        []string{"a"},
		},
		{
			name: "prune stale generation",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 4, Mode: ModeOnce},
			},
			parentGeneration: 5,
			wantLen:          0,
		},
		{
			name: "mixed - keep some prune others",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "always", Mode: ModeAlways},
				{APIVersion: "v1", Kind: "ConfigMap", Name: "current", Generation: 5, Mode: ModeOnce},
				{APIVersion: "v1", Kind: "ConfigMap", Name: "stale", Generation: 3, Mode: ModeOnce},
				{APIVersion: "v1", Kind: "ConfigMap", Name: "future", Generation: 6, Mode: ModeGeneration},
			},
			parentGeneration: 5,
			wantLen:          3, // always, current, future
			wantNames:        []string{"always", "current", "future"},
		},
		{
			name: "prune mode=generation when stale",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Generation: 4, Mode: ModeGeneration},
			},
			parentGeneration: 5,
			wantLen:          0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pruner.PruneStale(tt.approvals, tt.parentGeneration)
			if len(result) != tt.wantLen {
				t.Errorf("len(result) = %d, want %d", len(result), tt.wantLen)
			}
			if tt.wantNames != nil {
				for i, name := range tt.wantNames {
					if i >= len(result) {
						t.Errorf("missing approval at index %d", i)
						continue
					}
					if result[i].Name != name {
						t.Errorf("result[%d].Name = %q, want %q", i, result[i].Name, name)
					}
				}
			}
		})
	}
}

func TestPruner_Prune(t *testing.T) {
	pruner := NewPruner()

	tests := []struct {
		name             string
		approvals        []Approval
		consumed         *Approval
		parentGeneration int64
		wantLen          int
		wantChanged      bool
	}{
		{
			name: "consume and prune",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "consumed", Generation: 5, Mode: ModeOnce},
				{APIVersion: "v1", Kind: "ConfigMap", Name: "stale", Generation: 3, Mode: ModeOnce},
				{APIVersion: "v1", Kind: "ConfigMap", Name: "keep", Mode: ModeAlways},
			},
			consumed:         &Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "consumed", Generation: 5, Mode: ModeOnce},
			parentGeneration: 5,
			wantLen:          1, // only "keep" remains
			wantChanged:      true,
		},
		{
			name: "nothing to prune",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "a", Mode: ModeAlways},
			},
			consumed:         nil,
			parentGeneration: 99,
			wantLen:          1,
			wantChanged:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pruner.Prune(tt.approvals, tt.consumed, tt.parentGeneration)
			if len(result.Approvals) != tt.wantLen {
				t.Errorf("len(Approvals) = %d, want %d", len(result.Approvals), tt.wantLen)
			}
			if result.Changed != tt.wantChanged {
				t.Errorf("Changed = %v, want %v", result.Changed, tt.wantChanged)
			}
		})
	}
}
