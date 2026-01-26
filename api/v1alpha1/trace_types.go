package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	// RequestUID is the unique identifier of the admission request that caused this mutation.
	RequestUID string `json:"requestUID,omitempty"`
	// Timestamp of the mutation.
	Timestamp metav1.Time `json:"timestamp"`
	// Labels contains custom metadata from kausality.io/trace-* annotations.
	// For example, "kausality.io/trace-ticket=JIRA-123" becomes Labels["ticket"]="JIRA-123".
	// Each hop captures labels from its own object; labels are not inherited from parent.
	Labels map[string]string `json:"labels,omitempty"`
}

// ParseTrace parses a trace from its JSON representation.
func ParseTrace(data string) (Trace, error) {
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

// Append creates a new trace with the given hop appended.
func (t Trace) Append(hop Hop) Trace {
	result := make(Trace, len(t)+1)
	copy(result, t)
	result[len(t)] = hop
	return result
}

// NewHop creates a new Hop with the current timestamp.
func NewHop(apiVersion, kind, name string, generation int64, user, requestUID string) Hop {
	return Hop{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		Generation: generation,
		User:       user,
		RequestUID: requestUID,
		Timestamp:  metav1.Now(),
	}
}

// NewHopWithLabels creates a new Hop with the current timestamp and custom labels.
func NewHopWithLabels(apiVersion, kind, name string, generation int64, user, requestUID string, labels map[string]string) Hop {
	hop := NewHop(apiVersion, kind, name, generation, user, requestUID)
	if len(labels) > 0 {
		hop.Labels = labels
	}
	return hop
}

// ExtractTraceLabels extracts trace metadata from annotations with the kausality.io/trace-* prefix.
// For example, "kausality.io/trace-ticket=JIRA-123" returns map["ticket"]="JIRA-123".
// Annotations with empty suffix (exactly "kausality.io/trace-") are skipped.
func ExtractTraceLabels(annotations map[string]string) map[string]string {
	if annotations == nil {
		return nil
	}

	var labels map[string]string
	for key, value := range annotations {
		if len(key) > len(TraceMetadataPrefix) && key[:len(TraceMetadataPrefix)] == TraceMetadataPrefix {
			// Extract the key after the prefix
			labelKey := key[len(TraceMetadataPrefix):]
			if labelKey == "" {
				continue // Skip empty label keys
			}
			if labels == nil {
				labels = make(map[string]string)
			}
			labels[labelKey] = value
		}
	}
	return labels
}
