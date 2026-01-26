// Package trace re-exports types from api/v1alpha1 for backward compatibility.
package trace

import (
	"github.com/kausality-io/kausality/api/v1alpha1"
)

// Annotation keys - re-exported from api/v1alpha1.
const (
	TraceAnnotation     = v1alpha1.TraceAnnotation
	TraceMetadataPrefix = v1alpha1.TraceMetadataPrefix
)

// Types - re-exported from api/v1alpha1.
type (
	Trace = v1alpha1.Trace
	Hop   = v1alpha1.Hop
)

// Parse parses a trace from its JSON representation.
// Re-exported from api/v1alpha1.ParseTrace.
var Parse = v1alpha1.ParseTrace

// NewHop creates a new Hop with the current timestamp.
var NewHop = v1alpha1.NewHop

// NewHopWithLabels creates a new Hop with the current timestamp and custom labels.
var NewHopWithLabels = v1alpha1.NewHopWithLabels

// ExtractTraceLabels extracts trace metadata from annotations with the kausality.io/trace-* prefix.
var ExtractTraceLabels = v1alpha1.ExtractTraceLabels
