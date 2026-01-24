package approval

import (
	"testing"
)

func TestApproval_Matches(t *testing.T) {
	approval := Approval{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "test-cm",
	}

	tests := []struct {
		name  string
		child ChildRef
		want  bool
	}{
		{
			name:  "exact match",
			child: ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			want:  true,
		},
		{
			name:  "different name",
			child: ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "other-cm"},
			want:  false,
		},
		{
			name:  "different kind",
			child: ChildRef{APIVersion: "v1", Kind: "Secret", Name: "test-cm"},
			want:  false,
		},
		{
			name:  "different apiVersion",
			child: ChildRef{APIVersion: "v2", Kind: "ConfigMap", Name: "test-cm"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := approval.Matches(tt.child); got != tt.want {
				t.Errorf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApproval_IsValid(t *testing.T) {
	tests := []struct {
		name             string
		approval         Approval
		parentGeneration int64
		want             bool
	}{
		{
			name: "mode=always - always valid",
			approval: Approval{
				Mode: ModeAlways,
			},
			parentGeneration: 99,
			want:             true,
		},
		{
			name: "mode=once - matching generation",
			approval: Approval{
				Mode:       ModeOnce,
				Generation: 5,
			},
			parentGeneration: 5,
			want:             true,
		},
		{
			name: "mode=once - different generation",
			approval: Approval{
				Mode:       ModeOnce,
				Generation: 5,
			},
			parentGeneration: 6,
			want:             false,
		},
		{
			name: "mode=generation - matching",
			approval: Approval{
				Mode:       ModeGeneration,
				Generation: 10,
			},
			parentGeneration: 10,
			want:             true,
		},
		{
			name: "mode=generation - different",
			approval: Approval{
				Mode:       ModeGeneration,
				Generation: 10,
			},
			parentGeneration: 11,
			want:             false,
		},
		{
			name: "empty mode defaults to once - matching",
			approval: Approval{
				Mode:       "",
				Generation: 3,
			},
			parentGeneration: 3,
			want:             true,
		},
		{
			name: "empty mode defaults to once - different",
			approval: Approval{
				Mode:       "",
				Generation: 3,
			},
			parentGeneration: 4,
			want:             false,
		},
		{
			name: "unknown mode - invalid",
			approval: Approval{
				Mode:       "invalid",
				Generation: 5,
			},
			parentGeneration: 5,
			want:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.approval.IsValid(tt.parentGeneration); got != tt.want {
				t.Errorf("IsValid(%d) = %v, want %v", tt.parentGeneration, got, tt.want)
			}
		})
	}
}

func TestRejection_Matches(t *testing.T) {
	rejection := Rejection{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-deploy",
		Reason:     "Destructive change",
	}

	tests := []struct {
		name  string
		child ChildRef
		want  bool
	}{
		{
			name:  "exact match",
			child: ChildRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "my-deploy"},
			want:  true,
		},
		{
			name:  "different name",
			child: ChildRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "other"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rejection.Matches(tt.child); got != tt.want {
				t.Errorf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRejection_IsActive(t *testing.T) {
	tests := []struct {
		name             string
		rejection        Rejection
		parentGeneration int64
		want             bool
	}{
		{
			name: "no generation - always active",
			rejection: Rejection{
				Generation: 0,
			},
			parentGeneration: 99,
			want:             true,
		},
		{
			name: "matching generation - active",
			rejection: Rejection{
				Generation: 5,
			},
			parentGeneration: 5,
			want:             true,
		},
		{
			name: "different generation - inactive",
			rejection: Rejection{
				Generation: 5,
			},
			parentGeneration: 6,
			want:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rejection.IsActive(tt.parentGeneration); got != tt.want {
				t.Errorf("IsActive(%d) = %v, want %v", tt.parentGeneration, got, tt.want)
			}
		})
	}
}

func TestParseApprovals(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{
			name:    "empty string",
			input:   "",
			wantLen: 0,
			wantErr: false,
		},
		{
			name:    "empty array",
			input:   "[]",
			wantLen: 0,
			wantErr: false,
		},
		{
			name:    "single approval",
			input:   `[{"apiVersion":"v1","kind":"ConfigMap","name":"test","mode":"always"}]`,
			wantLen: 1,
			wantErr: false,
		},
		{
			name:    "multiple approvals",
			input:   `[{"apiVersion":"v1","kind":"ConfigMap","name":"a"},{"apiVersion":"v1","kind":"Secret","name":"b"}]`,
			wantLen: 2,
			wantErr: false,
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantLen: 0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseApprovals(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseApprovals() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("ParseApprovals() len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestParseRejections(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{
			name:    "empty string",
			input:   "",
			wantLen: 0,
			wantErr: false,
		},
		{
			name:    "single rejection",
			input:   `[{"apiVersion":"v1","kind":"Pod","name":"test","reason":"dangerous"}]`,
			wantLen: 1,
			wantErr: false,
		},
		{
			name:    "invalid json",
			input:   `{broken`,
			wantLen: 0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRejections(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRejections() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("ParseRejections() len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestMarshalApprovals(t *testing.T) {
	tests := []struct {
		name      string
		approvals []Approval
		wantEmpty bool
		wantErr   bool
	}{
		{
			name:      "nil slice",
			approvals: nil,
			wantEmpty: true,
		},
		{
			name:      "empty slice",
			approvals: []Approval{},
			wantEmpty: true,
		},
		{
			name: "single approval",
			approvals: []Approval{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "test", Mode: ModeAlways},
			},
			wantEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MarshalApprovals(tt.approvals)
			if (err != nil) != tt.wantErr {
				t.Errorf("MarshalApprovals() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if (got == "") != tt.wantEmpty {
				t.Errorf("MarshalApprovals() empty = %v, wantEmpty %v", got == "", tt.wantEmpty)
			}
		})
	}
}
