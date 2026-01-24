// Package trace provides request tracing through Kubernetes resource hierarchies.
package trace

import (
	"encoding/json"
	"time"
)

// TraceAnnotation is the annotation key for the trace.
const TraceAnnotation = "kausality.io/trace"

// Trace represents the causal chain of mutations through a resource hierarchy.
// It is stored as a JSON array in the kausality.io/trace annotation.
type Trace []Hop

// Hop represents a single hop in the trace - a resource that was mutated.
type Hop struct {
	// APIVersion of the resource (e.g., "apps/v1").
	APIVersion string `json:"apiVersion"`
	// Kind of the resource (e.g., "Deployment").
	Kind string `json:"kind"`
	// Name of the resource.
	Name string `json:"name"`
	// Generation of the resource at mutation time.
	Generation int64 `json:"generation"`
	// User who made the mutation (human/CI at origin, service account for controllers).
	User string `json:"user"`
	// Timestamp of the mutation.
	Timestamp time.Time `json:"timestamp"`
}

// Parse parses a trace from its JSON representation.
func Parse(data string) (Trace, error) {
	if data == "" {
		return nil, nil
	}

	var trace Trace
	if err := json.Unmarshal([]byte(data), &trace); err != nil {
		return nil, err
	}
	return trace, nil
}

// String returns the JSON representation of the trace.
func (t Trace) String() string {
	if len(t) == 0 {
		return "[]"
	}

	data, err := json.Marshal(t)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// MarshalJSON implements json.Marshaler.
func (t Trace) MarshalJSON() ([]byte, error) {
	if t == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Hop(t))
}

// Origin returns the first hop (the initiator), or nil if empty.
func (t Trace) Origin() *Hop {
	if len(t) == 0 {
		return nil
	}
	return &t[0]
}

// LastHop returns the most recent hop, or nil if empty.
func (t Trace) LastHop() *Hop {
	if len(t) == 0 {
		return nil
	}
	return &t[len(t)-1]
}

// Append creates a new trace with the given hop appended.
func (t Trace) Append(hop Hop) Trace {
	result := make(Trace, len(t)+1)
	copy(result, t)
	result[len(t)] = hop
	return result
}

// NewHop creates a new Hop with the current timestamp.
func NewHop(apiVersion, kind, name string, generation int64, user string) Hop {
	return Hop{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Generation: generation,
		User:       user,
		Timestamp:  time.Now().UTC(),
	}
}
