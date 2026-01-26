package trace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTrace_Parse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int // number of hops
		wantErr bool
	}{
		{
			name:  "empty string",
			input: "",
			want:  0,
		},
		{
			name:  "empty array",
			input: "[]",
			want:  0,
		},
		{
			name: "single hop",
			input: `[{
				"apiVersion": "apps/v1",
				"kind": "Deployment",
				"name": "test",
				"generation": 5,
				"user": "hans@example.com",
				"timestamp": "2026-01-24T10:30:00Z"
			}]`,
			want: 1,
		},
		{
			name: "multiple hops",
			input: `[
				{"apiVersion": "apps/v1", "kind": "Deployment", "name": "d1", "generation": 1, "user": "u1", "timestamp": "2026-01-24T10:00:00Z"},
				{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "rs1", "generation": 2, "user": "sa1", "timestamp": "2026-01-24T10:01:00Z"}
			]`,
			want: 2,
		},
		{
			name:    "invalid json",
			input:   "not json",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trace, err := Parse(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, trace, tt.want)
		})
	}
}

func TestTrace_String(t *testing.T) {
	ts := metav1.Time{Time: time.Date(2026, 1, 24, 10, 30, 0, 0, time.UTC)}

	trace := Trace{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "test", Generation: 5, User: "hans@example.com", Timestamp: ts},
	}

	str := trace.String()

	// Parse it back
	parsed, err := Parse(str)
	require.NoError(t, err, "failed to parse trace string")
	require.Len(t, parsed, 1, "expected 1 hop")
	assert.Equal(t, "Deployment", parsed[0].Kind)
}

func TestTrace_Origin(t *testing.T) {
	tests := []struct {
		name  string
		trace Trace
		want  *Hop
	}{
		{
			name:  "empty trace",
			trace: nil,
			want:  nil,
		},
		{
			name:  "single hop",
			trace: Trace{{Kind: "Deployment", Name: "test"}},
			want:  &Hop{Kind: "Deployment", Name: "test"},
		},
		{
			name: "multiple hops",
			trace: Trace{
				{Kind: "Deployment", Name: "d1"},
				{Kind: "ReplicaSet", Name: "rs1"},
			},
			want: &Hop{Kind: "Deployment", Name: "d1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.trace.Origin()
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.want.Kind, got.Kind)
		})
	}
}

func TestTrace_Append(t *testing.T) {
	original := Trace{
		{Kind: "Deployment", Name: "d1"},
	}

	newHop := Hop{Kind: "ReplicaSet", Name: "rs1"}
	extended := original.Append(newHop)

	// Original should be unchanged
	assert.Len(t, original, 1, "original trace should be unchanged")

	// Extended should have 2 hops
	require.Len(t, extended, 2)
	assert.Equal(t, "ReplicaSet", extended[1].Kind)
}

func TestNewHop(t *testing.T) {
	hop := NewHop("apps/v1", "Deployment", "test", 5, "hans@example.com", "req-123")

	assert.Equal(t, "apps/v1", hop.APIVersion)
	assert.Equal(t, "Deployment", hop.Kind)
	assert.Equal(t, "test", hop.Name)
	assert.Equal(t, int64(5), hop.Generation)
	assert.Equal(t, "hans@example.com", hop.User)
	assert.Equal(t, "req-123", hop.RequestUID)
	assert.False(t, hop.Timestamp.IsZero(), "Timestamp should not be zero")
}

func TestTrace_MarshalJSON_Nil(t *testing.T) {
	var trace Trace = nil
	data, err := json.Marshal(trace)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(data))
}

func TestExtractTraceLabels(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        map[string]string
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			want:        nil,
		},
		{
			name:        "no trace labels",
			annotations: map[string]string{"foo": "bar"},
			want:        nil,
		},
		{
			name: "single trace label",
			annotations: map[string]string{
				"kausality.io/trace-ticket": "JIRA-123",
			},
			want: map[string]string{"ticket": "JIRA-123"},
		},
		{
			name: "multiple trace labels",
			annotations: map[string]string{
				"kausality.io/trace-ticket":     "JIRA-123",
				"kausality.io/trace-deployment": "deploy-42",
				"kausality.io/trace-env":        "prod",
			},
			want: map[string]string{
				"ticket":     "JIRA-123",
				"deployment": "deploy-42",
				"env":        "prod",
			},
		},
		{
			name: "mixed annotations",
			annotations: map[string]string{
				"kausality.io/trace-ticket": "JIRA-123",
				"kausality.io/trace":        "[...]", // main trace annotation, not a label
				"other/annotation":          "value",
			},
			want: map[string]string{"ticket": "JIRA-123"},
		},
		{
			name: "exact prefix match only",
			annotations: map[string]string{
				"kausality.io/trace-foo": "bar",
				"kausality.io/tracefoo":  "should not match",
				"kausality.io/trace-":    "empty key skipped",
				"other.io/trace-ticket":  "wrong prefix",
			},
			want: map[string]string{
				"foo": "bar",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractTraceLabels(tt.annotations)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ExtractTraceLabels() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewHopWithLabels(t *testing.T) {
	labels := map[string]string{"ticket": "JIRA-123", "env": "prod"}
	hop := NewHopWithLabels("apps/v1", "Deployment", "test", 5, "hans@example.com", "req-456", labels)

	assert.Equal(t, "apps/v1", hop.APIVersion)
	assert.Equal(t, "req-456", hop.RequestUID)
	assert.Len(t, hop.Labels, 2)
	assert.Equal(t, "JIRA-123", hop.Labels["ticket"])
}

func TestNewHopWithLabels_NilLabels(t *testing.T) {
	hop := NewHopWithLabels("apps/v1", "Deployment", "test", 5, "user", "req-789", nil)
	assert.Nil(t, hop.Labels, "Labels should be nil for nil input")
}

func TestNewHopWithLabels_EmptyLabels(t *testing.T) {
	hop := NewHopWithLabels("apps/v1", "Deployment", "test", 5, "user", "", map[string]string{})
	assert.Nil(t, hop.Labels, "Labels should be nil for empty input")
}

func TestHopWithLabels_JSON(t *testing.T) {
	ts := metav1.Time{Time: time.Date(2026, 1, 24, 10, 30, 0, 0, time.UTC)}
	hop := Hop{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "test",
		Generation: 5,
		User:       "user",
		Timestamp:  ts,
		Labels:     map[string]string{"ticket": "JIRA-123"},
	}

	data, err := json.Marshal(hop)
	require.NoError(t, err)

	// Verify labels are included
	var parsed Hop
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, "JIRA-123", parsed.Labels["ticket"])
}

func TestHopWithoutLabels_JSON(t *testing.T) {
	ts := metav1.Time{Time: time.Date(2026, 1, 24, 10, 30, 0, 0, time.UTC)}
	hop := Hop{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "test",
		Generation: 5,
		User:       "user",
		Timestamp:  ts,
	}

	data, err := json.Marshal(hop)
	require.NoError(t, err)

	// Verify labels field is omitted (omitempty)
	assert.False(t, strings.Contains(string(data), "labels"), "JSON should not contain 'labels' field when empty")
}
