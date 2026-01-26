package approval

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApproval_Matches(t *testing.T) {
	tests := []struct {
		name     string
		approval Approval
		child    ChildRef
		want     bool
	}{
		{
			name:     "exact match",
			approval: Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			child:    ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			want:     true,
		},
		{
			name:     "different name",
			approval: Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			child:    ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "other-cm"},
			want:     false,
		},
		{
			name:     "different kind",
			approval: Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			child:    ChildRef{APIVersion: "v1", Kind: "Secret", Name: "test-cm"},
			want:     false,
		},
		{
			name:     "different apiVersion",
			approval: Approval{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			child:    ChildRef{APIVersion: "v2", Kind: "ConfigMap", Name: "test-cm"},
			want:     false,
		},
		{
			name:     "wildcard name matches any",
			approval: Approval{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "*"},
			child:    ChildRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "my-deploy-abc123"},
			want:     true,
		},
		{
			name:     "wildcard kind matches any",
			approval: Approval{APIVersion: "v1", Kind: "*", Name: "test"},
			child:    ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "test"},
			want:     true,
		},
		{
			name:     "wildcard apiVersion matches any",
			approval: Approval{APIVersion: "*", Kind: "Secret", Name: "creds"},
			child:    ChildRef{APIVersion: "v1", Kind: "Secret", Name: "creds"},
			want:     true,
		},
		{
			name:     "all wildcards match anything",
			approval: Approval{APIVersion: "*", Kind: "*", Name: "*"},
			child:    ChildRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "foo"},
			want:     true,
		},
		{
			name:     "wildcard name still requires kind match",
			approval: Approval{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "*"},
			child:    ChildRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "foo"},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.approval.Matches(tt.child))
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
			assert.Equal(t, tt.want, tt.approval.IsValid(tt.parentGeneration))
		})
	}
}

func TestRejection_Matches(t *testing.T) {
	tests := []struct {
		name      string
		rejection Rejection
		child     ChildRef
		want      bool
	}{
		{
			name:      "exact match",
			rejection: Rejection{APIVersion: "apps/v1", Kind: "Deployment", Name: "my-deploy", Reason: "test"},
			child:     ChildRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "my-deploy"},
			want:      true,
		},
		{
			name:      "different name",
			rejection: Rejection{APIVersion: "apps/v1", Kind: "Deployment", Name: "my-deploy", Reason: "test"},
			child:     ChildRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "other"},
			want:      false,
		},
		{
			name:      "wildcard name matches any",
			rejection: Rejection{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "*", Reason: "frozen"},
			child:     ChildRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs-xyz-12345"},
			want:      true,
		},
		{
			name:      "wildcard name still requires kind match",
			rejection: Rejection{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "*", Reason: "frozen"},
			child:     ChildRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "deploy-xyz"},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.rejection.Matches(tt.child))
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
			assert.Equal(t, tt.want, tt.rejection.IsActive(tt.parentGeneration))
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
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Len(t, got, tt.wantLen)
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
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Len(t, got, tt.wantLen)
		})
	}
}

func TestParseFreeze(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantNil  bool
		wantUser string
		wantErr  bool
	}{
		{
			name:    "empty string",
			input:   "",
			wantNil: true,
		},
		{
			name:     "legacy true format",
			input:    "true",
			wantNil:  false,
			wantUser: "", // no user in legacy format
		},
		{
			name:     "structured JSON",
			input:    `{"user":"admin@example.com","message":"incident #123","at":"2026-01-25T10:00:00Z"}`,
			wantNil:  false,
			wantUser: "admin@example.com",
		},
		{
			name:    "invalid JSON",
			input:   `{broken`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFreeze(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			assert.NotNil(t, got)
			assert.Equal(t, tt.wantUser, got.User)
		})
	}
}

func TestParseSnooze(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantNil    bool
		wantUser   string
		wantExpiry bool
		wantErr    bool
	}{
		{
			name:    "empty string",
			input:   "",
			wantNil: true,
		},
		{
			name:       "legacy RFC3339 format",
			input:      "2026-01-25T12:00:00Z",
			wantNil:    false,
			wantUser:   "", // no user in legacy format
			wantExpiry: true,
		},
		{
			name:       "structured JSON",
			input:      `{"expiry":"2026-01-25T12:00:00Z","user":"ops@example.com","message":"deploying hotfix"}`,
			wantNil:    false,
			wantUser:   "ops@example.com",
			wantExpiry: true,
		},
		{
			name:    "invalid JSON",
			input:   `{broken`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSnooze(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			assert.NotNil(t, got)
			assert.Equal(t, tt.wantUser, got.User)
			if tt.wantExpiry {
				assert.False(t, got.Expiry.IsZero())
			}
		})
	}
}

func TestSnooze_IsActive(t *testing.T) {
	tests := []struct {
		name   string
		snooze *Snooze
		want   bool
	}{
		{
			name:   "nil snooze",
			snooze: nil,
			want:   false,
		},
		{
			name:   "expired snooze",
			snooze: &Snooze{Expiry: metav1.Time{Time: time.Now().Add(-1 * time.Hour)}},
			want:   false,
		},
		{
			name:   "active snooze",
			snooze: &Snooze{Expiry: metav1.Time{Time: time.Now().Add(1 * time.Hour)}},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.snooze.IsActive())
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
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantEmpty, got == "", "empty check")
		})
	}
}
