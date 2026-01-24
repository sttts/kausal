// Package admission provides admission handling for drift detection and tracing.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kausality-io/kausality/pkg/drift"
	"github.com/kausality-io/kausality/pkg/trace"
)

// Handler handles admission requests for drift detection and tracing.
type Handler struct {
	client     client.Client
	decoder    admission.Decoder
	detector   *drift.Detector
	propagator *trace.Propagator
	log        logr.Logger
}

// Config configures the admission handler.
type Config struct {
	Client client.Client
	Log    logr.Logger
}

// NewHandler creates a new admission Handler.
func NewHandler(cfg Config) *Handler {
	return &Handler{
		client:     cfg.Client,
		detector:   drift.NewDetector(cfg.Client),
		propagator: trace.NewPropagator(cfg.Client),
		log:        cfg.Log.WithName("kausality-admission"),
	}
}

// Handle processes an admission request for drift detection and tracing.
func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := h.log.WithValues(
		"operation", req.Operation,
		"kind", req.Kind.String(),
		"namespace", req.Namespace,
		"name", req.Name,
		"user", req.UserInfo.Username,
	)

	// Handle CREATE, UPDATE, and DELETE (DELETE just sets deletionTimestamp)
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update && req.Operation != admissionv1.Delete {
		return admission.Allowed("operation not relevant for tracing")
	}

	// For UPDATE, check if spec changed - ignore status/metadata-only changes
	// DELETE always traces (sets deletionTimestamp, which is significant even though it's metadata)
	if req.Operation == admissionv1.Update {
		specChanged, err := h.hasSpecChanged(req)
		if err != nil {
			log.Error(err, "failed to check spec change")
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to check spec change: %w", err))
		}
		if !specChanged {
			log.V(2).Info("no spec change, skipping")
			return admission.Allowed("no spec change")
		}
	}

	// Parse the object from the request
	obj, err := h.parseObject(req)
	if err != nil {
		log.Error(err, "failed to parse object from request")
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to parse object: %w", err))
	}

	// Detect drift
	driftResult, err := h.detector.Detect(ctx, obj)
	if err != nil {
		log.Error(err, "drift detection failed")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("drift detection failed: %w", err))
	}

	// Log drift detection result
	logFields := []interface{}{
		"driftDetected", driftResult.DriftDetected,
		"lifecyclePhase", driftResult.LifecyclePhase,
	}
	if driftResult.ParentRef != nil {
		logFields = append(logFields,
			"parentKind", driftResult.ParentRef.Kind,
			"parentName", driftResult.ParentRef.Name,
		)
	}

	if driftResult.DriftDetected {
		log.Info("DRIFT DETECTED - would be blocked in enforcement mode", logFields...)
	} else {
		log.V(1).Info("drift check passed", logFields...)
	}

	// Propagate trace
	traceResult, err := h.propagator.Propagate(ctx, obj, req.UserInfo.Username)
	if err != nil {
		log.Error(err, "trace propagation failed")
		// Don't fail the request on trace errors - just log and continue
		return admission.Allowed(driftResult.Reason)
	}

	// Log trace info
	if traceResult.IsOrigin {
		log.Info("trace: new origin", "traceLen", len(traceResult.Trace))
	} else {
		log.V(1).Info("trace: extended", "traceLen", len(traceResult.Trace), "parentTraceLen", len(traceResult.ParentTrace))
	}

	// For DELETE, we can't patch (no new object), just allow after logging
	if req.Operation == admissionv1.Delete {
		log.V(1).Info("delete operation traced", "trace", traceResult.Trace.String())
		return admission.Allowed(driftResult.Reason)
	}

	// Create patch to add/update trace annotation
	patch, err := createTracePatch(obj, traceResult.Trace)
	if err != nil {
		log.Error(err, "failed to create trace patch")
		return admission.Allowed(driftResult.Reason)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, patch)
}

// createTracePatch creates a JSON patch to set the trace annotation.
func createTracePatch(obj client.Object, t trace.Trace) ([]byte, error) {
	// Get existing annotations
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// Set trace annotation
	annotations[trace.TraceAnnotation] = t.String()

	// Create patched object
	unstrObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("expected *unstructured.Unstructured, got %T", obj)
	}

	patched := unstrObj.DeepCopy()
	patched.SetAnnotations(annotations)

	// Marshal both to JSON
	original, err := json.Marshal(unstrObj.Object)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal original: %w", err)
	}

	modified, err := json.Marshal(patched.Object)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal patched: %w", err)
	}

	// Return the patched object (controller-runtime will compute the diff)
	_ = original // We return the full patched object, not a diff
	return modified, nil
}

// parseObject parses the object from the admission request.
func (h *Handler) parseObject(req admission.Request) (client.Object, error) {
	var rawObj []byte

	// For DELETE, use OldObject; for CREATE/UPDATE, use Object
	if req.Operation == admissionv1.Delete {
		rawObj = req.OldObject.Raw
	} else {
		rawObj = req.Object.Raw
	}

	if len(rawObj) == 0 {
		return nil, fmt.Errorf("no object data in request")
	}

	// Parse as unstructured
	obj := &unstructured.Unstructured{}
	if err := runtime.DecodeInto(unstructured.UnstructuredJSONScheme, rawObj, obj); err != nil {
		return nil, fmt.Errorf("failed to decode object: %w", err)
	}

	// Set GVK from request
	gvk := schema.GroupVersionKind{
		Group:   req.Kind.Group,
		Version: req.Kind.Version,
		Kind:    req.Kind.Kind,
	}
	obj.SetGroupVersionKind(gvk)

	// Set namespace if not set
	if obj.GetNamespace() == "" && req.Namespace != "" {
		obj.SetNamespace(req.Namespace)
	}

	return obj, nil
}

// InjectDecoder injects the decoder.
func (h *Handler) InjectDecoder(d admission.Decoder) error {
	h.decoder = d
	return nil
}

// hasSpecChanged checks if the spec field changed between old and new object.
func (h *Handler) hasSpecChanged(req admission.Request) (bool, error) {
	if len(req.OldObject.Raw) == 0 || len(req.Object.Raw) == 0 {
		return true, nil // can't compare, assume changed
	}

	oldObj := &unstructured.Unstructured{}
	if err := runtime.DecodeInto(unstructured.UnstructuredJSONScheme, req.OldObject.Raw, oldObj); err != nil {
		return false, fmt.Errorf("failed to decode old object: %w", err)
	}

	newObj := &unstructured.Unstructured{}
	if err := runtime.DecodeInto(unstructured.UnstructuredJSONScheme, req.Object.Raw, newObj); err != nil {
		return false, fmt.Errorf("failed to decode new object: %w", err)
	}

	oldSpec, _, _ := unstructured.NestedFieldCopy(oldObj.Object, "spec")
	newSpec, _, _ := unstructured.NestedFieldCopy(newObj.Object, "spec")

	return !equalSpec(oldSpec, newSpec), nil
}

// equalSpec compares two spec values for equality.
func equalSpec(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	// Use JSON encoding for deep comparison
	aJSON, err := runtime.Encode(unstructured.UnstructuredJSONScheme, &unstructured.Unstructured{Object: map[string]interface{}{"spec": a}})
	if err != nil {
		return false
	}
	bJSON, err := runtime.Encode(unstructured.UnstructuredJSONScheme, &unstructured.Unstructured{Object: map[string]interface{}{"spec": b}})
	if err != nil {
		return false
	}

	return string(aJSON) == string(bJSON)
}

// ValidatingWebhookFor creates a ValidatingAdmissionResponse for the given result.
func ValidatingWebhookFor(result *drift.DriftResult) admission.Response {
	if result.Allowed {
		return admission.Allowed(result.Reason)
	}

	return admission.Response{
		AdmissionResponse: admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Code:    http.StatusForbidden,
				Message: result.Reason,
				Reason:  metav1.StatusReasonForbidden,
			},
		},
	}
}
