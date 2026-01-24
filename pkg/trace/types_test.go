package trace

import (
	"encoding/json"
	"testing"
	"time"
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
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(trace) != tt.want {
				t.Errorf("Parse() got %d hops, want %d", len(trace), tt.want)
			}
		})
	}
}

func TestTrace_String(t *testing.T) {
	ts := time.Date(2026, 1, 24, 10, 30, 0, 0, time.UTC)

	trace := Trace{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "test", Generation: 5, User: "hans@example.com", Timestamp: ts},
	}

	str := trace.String()

	// Parse it back
	parsed, err := Parse(str)
	if err != nil {
		t.Fatalf("failed to parse trace string: %v", err)
	}

	if len(parsed) != 1 {
		t.Fatalf("expected 1 hop, got %d", len(parsed))
	}

	if parsed[0].Kind != "Deployment" {
		t.Errorf("Kind = %q, want %q", parsed[0].Kind, "Deployment")
	}
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
			if tt.want == nil && got != nil {
				t.Errorf("Origin() = %v, want nil", got)
			}
			if tt.want != nil && got == nil {
				t.Errorf("Origin() = nil, want %v", tt.want)
			}
			if tt.want != nil && got != nil && got.Kind != tt.want.Kind {
				t.Errorf("Origin().Kind = %q, want %q", got.Kind, tt.want.Kind)
			}
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
	if len(original) != 1 {
		t.Errorf("original trace modified, got %d hops", len(original))
	}

	// Extended should have 2 hops
	if len(extended) != 2 {
		t.Errorf("extended trace has %d hops, want 2", len(extended))
	}

	if extended[1].Kind != "ReplicaSet" {
		t.Errorf("extended[1].Kind = %q, want %q", extended[1].Kind, "ReplicaSet")
	}
}

func TestNewHop(t *testing.T) {
	hop := NewHop("apps/v1", "Deployment", "test", 5, "hans@example.com")

	if hop.APIVersion != "apps/v1" {
		t.Errorf("APIVersion = %q, want %q", hop.APIVersion, "apps/v1")
	}
	if hop.Kind != "Deployment" {
		t.Errorf("Kind = %q, want %q", hop.Kind, "Deployment")
	}
	if hop.Name != "test" {
		t.Errorf("Name = %q, want %q", hop.Name, "test")
	}
	if hop.Generation != 5 {
		t.Errorf("Generation = %d, want %d", hop.Generation, 5)
	}
	if hop.User != "hans@example.com" {
		t.Errorf("User = %q, want %q", hop.User, "hans@example.com")
	}
	if hop.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
}

func TestTrace_MarshalJSON_Nil(t *testing.T) {
	var trace Trace = nil
	data, err := json.Marshal(trace)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("Marshal(nil) = %q, want %q", string(data), "[]")
	}
}
