// Package admission provides admission handling for drift detection and tracing.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kausality-io/kausality/pkg/approval"
	"github.com/kausality-io/kausality/pkg/callback"
	"github.com/kausality-io/kausality/pkg/callback/v1alpha1"
	"github.com/kausality-io/kausality/pkg/config"
	"github.com/kausality-io/kausality/pkg/drift"
	"github.com/kausality-io/kausality/pkg/trace"
)

// Handler handles admission requests for drift detection and tracing.
type Handler struct {
	client          client.Client
	decoder         admission.Decoder
	detector        *drift.Detector
	propagator      *trace.Propagator
	approvalChecker *approval.Checker
	callbackSender  *callback.Sender
	config          *config.Config
	log             logr.Logger
}

// Config configures the admission handler.
type Config struct {
	Client client.Client
	Log    logr.Logger
	// DriftConfig provides per-resource drift detection configuration.
	// If nil, defaults to log mode for all resources.
	DriftConfig *config.Config
	// CallbackSender sends drift reports to webhook endpoints.
	// If nil, drift callbacks are disabled.
	CallbackSender *callback.Sender
}

// NewHandler creates a new admission Handler.
func NewHandler(cfg Config) *Handler {
	driftConfig := cfg.DriftConfig
	if driftConfig == nil {
		driftConfig = config.Default()
	}
	return &Handler{
		client:          cfg.Client,
		detector:        drift.NewDetector(cfg.Client),
		propagator:      trace.NewPropagator(cfg.Client),
		approvalChecker: approval.NewChecker(),
		callbackSender:  cfg.CallbackSender,
		config:          driftConfig,
		log:             cfg.Log.WithName("kausality-admission"),
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

	// Extract fieldManager for controller identification
	fieldManager := extractFieldManager(req)
	log = log.WithValues("fieldManager", fieldManager)

	// Detect drift
	driftResult, err := h.detector.DetectWithFieldManager(ctx, obj, fieldManager)
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

	// Check for freeze annotation on parent - blocks ALL mutations, not just drift
	if driftResult.ParentRef != nil {
		if frozen, reason := h.checkFreeze(ctx, driftResult.ParentRef, obj.GetNamespace(), log); frozen {
			freezeMsg := fmt.Sprintf("mutation blocked: parent is frozen (%s)", reason)
			log.Info("MUTATION FROZEN", logFields...)
			return admission.Denied(freezeMsg)
		}
	}

	// Track warnings to add to the response
	var warnings []string

	// Build resource context for mode matching
	gvk := obj.GetObjectKind().GroupVersionKind()
	resourceCtx := config.ResourceContext{
		GVK:          gvk,
		Namespace:    obj.GetNamespace(),
		ObjectLabels: obj.GetLabels(),
	}

	// Fetch namespace metadata if needed for selector matching and annotation resolution
	var nsAnnotations map[string]string
	if obj.GetNamespace() != "" {
		nsLabels, nsAnns, err := h.getNamespaceMetadata(ctx, obj.GetNamespace())
		if err != nil {
			log.V(1).Info("failed to get namespace metadata", "error", err)
			// Continue without namespace metadata - selectors won't match
		} else {
			resourceCtx.NamespaceLabels = nsLabels
			nsAnnotations = nsAnns
		}
	}

	// Determine enforce mode using annotation-based resolution
	// Precedence: object annotation > namespace annotation > config
	objAnnotations := obj.GetAnnotations()
	if objAnnotations == nil {
		objAnnotations = map[string]string{}
	}
	if nsAnnotations == nil {
		nsAnnotations = map[string]string{}
	}
	driftMode := h.config.ResolveModeWithAnnotations(objAnnotations, nsAnnotations, resourceCtx)
	enforceMode := driftMode == config.ModeEnforce

	if driftResult.DriftDetected {
		// Check for approvals when drift is detected
		approvalResult := h.checkApprovals(ctx, driftResult, obj, log)
		logFields = append(logFields,
			"approved", approvalResult.Approved,
			"rejected", approvalResult.Rejected,
			"driftMode", driftMode,
		)

		if approvalResult.Rejected {
			rejectMsg := fmt.Sprintf("drift rejected: %s", approvalResult.Reason)
			log.Info("DRIFT REJECTED", append(logFields, "rejectReason", approvalResult.Reason)...)
			if enforceMode {
				return admission.Denied(rejectMsg)
			}
			// Non-enforce mode: add warning but allow
			warnings = append(warnings, fmt.Sprintf("[kausality] %s (would be blocked in enforce mode)", rejectMsg))
		} else if approvalResult.Approved {
			log.Info("DRIFT APPROVED", append(logFields, "approvalReason", approvalResult.Reason)...)
			// Consume mode=once approvals and prune stale ones
			h.consumeApproval(ctx, approvalResult, log)
			// Send resolved notification
			h.sendDriftCallback(ctx, req, obj, driftResult, approvalResult.parent, v1alpha1.DriftReportPhaseResolved, log)
		} else {
			driftMsg := "drift detected: no approval found for this mutation"
			log.Info("DRIFT DETECTED - no approval found", logFields...)
			// Send drift detected notification
			h.sendDriftCallback(ctx, req, obj, driftResult, approvalResult.parent, v1alpha1.DriftReportPhaseDetected, log)
			if enforceMode {
				return admission.Denied(driftMsg)
			}
			// Non-enforce mode: add warning but allow
			warnings = append(warnings, fmt.Sprintf("[kausality] %s (would be blocked in enforce mode)", driftMsg))
		}
	} else {
		log.V(1).Info("drift check passed", logFields...)
	}

	// Propagate trace
	traceResult, err := h.propagator.PropagateWithFieldManager(ctx, obj, req.UserInfo.Username, fieldManager, string(req.UID))
	if err != nil {
		log.Error(err, "trace propagation failed")
		// Don't fail the request on trace errors - just log and continue
		return withWarnings(admission.Allowed(driftResult.Reason), warnings)
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
		return withWarnings(admission.Allowed(driftResult.Reason), warnings)
	}

	// Create patch to add/update trace annotation
	patch, err := createTracePatch(obj, traceResult.Trace)
	if err != nil {
		log.Error(err, "failed to create trace patch")
		return withWarnings(admission.Allowed(driftResult.Reason), warnings)
	}

	resp := admission.PatchResponseFromRaw(req.Object.Raw, patch)
	return withWarnings(resp, warnings)
}

// withWarnings adds warnings to an admission response.
func withWarnings(resp admission.Response, warnings []string) admission.Response {
	if len(warnings) > 0 {
		resp.Warnings = append(resp.Warnings, warnings...)
	}
	return resp
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

// approvalCheckResult extends approval.CheckResult with parent info for pruning.
type approvalCheckResult struct {
	approval.CheckResult
	parent           client.Object
	parentGeneration int64
}

// checkApprovals checks if the drift is approved or rejected.
func (h *Handler) checkApprovals(ctx context.Context, driftResult *drift.DriftResult, obj client.Object, log logr.Logger) approvalCheckResult {
	if driftResult.ParentRef == nil {
		return approvalCheckResult{CheckResult: approval.CheckResult{Reason: "no parent to check approvals on"}}
	}

	// Fetch parent object to read approval annotations
	parent, err := h.fetchParent(ctx, driftResult.ParentRef, obj.GetNamespace())
	if err != nil {
		log.Error(err, "failed to fetch parent for approval check")
		return approvalCheckResult{CheckResult: approval.CheckResult{Reason: "failed to fetch parent: " + err.Error()}}
	}

	// Build child reference
	gvk := obj.GetObjectKind().GroupVersionKind()
	childRef := approval.ChildRef{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       obj.GetName(),
	}

	// Check approvals on parent
	result := h.approvalChecker.Check(parent, childRef, parent.GetGeneration())
	return approvalCheckResult{
		CheckResult:      result,
		parent:           parent,
		parentGeneration: parent.GetGeneration(),
	}
}

// consumeApproval removes a mode=once approval and prunes stale approvals from the parent.
func (h *Handler) consumeApproval(ctx context.Context, result approvalCheckResult, log logr.Logger) {
	if result.parent == nil || result.MatchedApproval == nil {
		return
	}

	// Only consume mode=once approvals
	mode := result.MatchedApproval.Mode
	if mode == "" {
		mode = approval.ModeOnce
	}
	if mode != approval.ModeOnce {
		return
	}

	annotations := result.parent.GetAnnotations()
	if annotations == nil {
		return
	}

	approvalsStr := annotations[approval.ApprovalsAnnotation]
	if approvalsStr == "" {
		return
	}

	approvals, err := approval.ParseApprovals(approvalsStr)
	if err != nil {
		log.Error(err, "failed to parse approvals for pruning")
		return
	}

	// Prune the consumed approval and any stale ones
	pruner := approval.NewPruner()
	pruneResult := pruner.Prune(approvals, result.MatchedApproval, result.parentGeneration)

	if !pruneResult.Changed {
		return
	}

	// Update the parent's annotations
	newAnnotations := make(map[string]string)
	for k, v := range annotations {
		newAnnotations[k] = v
	}

	if len(pruneResult.Approvals) == 0 {
		delete(newAnnotations, approval.ApprovalsAnnotation)
	} else {
		newApprovalsStr, err := approval.MarshalApprovals(pruneResult.Approvals)
		if err != nil {
			log.Error(err, "failed to marshal pruned approvals")
			return
		}
		newAnnotations[approval.ApprovalsAnnotation] = newApprovalsStr
	}

	// Update the parent object
	parentCopy := result.parent.DeepCopyObject().(client.Object)
	parentCopy.SetAnnotations(newAnnotations)

	if err := h.client.Update(ctx, parentCopy); err != nil {
		log.Error(err, "failed to update parent with pruned approvals",
			"removedCount", pruneResult.RemovedCount)
		return
	}

	log.Info("pruned approvals from parent",
		"removedCount", pruneResult.RemovedCount,
		"remaining", len(pruneResult.Approvals))
}

// fetchParent fetches the parent object by reference.
func (h *Handler) fetchParent(ctx context.Context, ref *drift.ParentRef, childNamespace string) (client.Object, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid parent API version: %w", err)
	}

	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(gv.WithKind(ref.Kind))

	key := client.ObjectKey{
		Namespace: ref.Namespace,
		Name:      ref.Name,
	}
	// If parent namespace is empty but child has namespace, use child's namespace
	if key.Namespace == "" && childNamespace != "" {
		key.Namespace = childNamespace
	}

	if err := h.client.Get(ctx, key, parent); err != nil {
		return nil, err
	}

	return parent, nil
}

// checkFreeze checks if the parent has a freeze annotation.
// Freeze blocks ALL mutations, not just drift - it's an emergency lockdown.
func (h *Handler) checkFreeze(ctx context.Context, ref *drift.ParentRef, childNamespace string, log logr.Logger) (frozen bool, reason string) {
	parent, err := h.fetchParent(ctx, ref, childNamespace)
	if err != nil {
		log.V(1).Info("failed to fetch parent for freeze check", "error", err)
		return false, ""
	}

	annotations := parent.GetAnnotations()
	if annotations == nil {
		return false, ""
	}

	freezeValue, ok := annotations[approval.FreezeAnnotation]
	if !ok || freezeValue == "" {
		return false, ""
	}

	// Any non-empty value other than "false" is considered frozen
	if freezeValue == "false" {
		return false, ""
	}

	return true, freezeValue
}

// extractFieldManager extracts the fieldManager from admission request options.
func extractFieldManager(req admission.Request) string {
	if len(req.Options.Raw) == 0 {
		return ""
	}

	// Options is a RawExtension that can be CreateOptions, UpdateOptions, PatchOptions, or DeleteOptions
	// All of them (except DeleteOptions) have a FieldManager field
	// We'll parse as a generic map to extract fieldManager

	var opts map[string]interface{}
	if err := json.Unmarshal(req.Options.Raw, &opts); err != nil {
		return ""
	}

	if fm, ok := opts["fieldManager"].(string); ok {
		return fm
	}

	return ""
}

// sendDriftCallback sends a drift report to the configured webhook endpoint.
// If the parent has an active snooze annotation, the callback is suppressed.
func (h *Handler) sendDriftCallback(ctx context.Context, req admission.Request, obj client.Object, driftResult *drift.DriftResult, parent client.Object, phase v1alpha1.DriftReportPhase, log logr.Logger) {
	if h.callbackSender == nil || !h.callbackSender.IsEnabled() {
		return
	}

	// Check for snooze annotation on parent
	if parent != nil && h.isParentSnoozed(parent, log) {
		log.V(1).Info("drift callback suppressed (parent is snoozed)", "phase", phase)
		return
	}

	report := h.buildDriftReport(req, obj, driftResult, phase)
	if report == nil {
		return
	}

	// Send asynchronously to avoid blocking admission
	h.callbackSender.SendAsync(ctx, report)
	log.V(1).Info("drift callback sent", "phase", phase, "id", report.Spec.ID)
}

// isParentSnoozed checks if the parent has an active snooze annotation.
func (h *Handler) isParentSnoozed(parent client.Object, log logr.Logger) bool {
	if parent == nil {
		return false
	}

	annotations := parent.GetAnnotations()
	if annotations == nil {
		return false
	}

	snoozeUntil, ok := annotations[approval.SnoozeAnnotation]
	if !ok || snoozeUntil == "" {
		return false
	}

	// Parse the snooze timestamp
	snoozeTime, err := time.Parse(time.RFC3339, snoozeUntil)
	if err != nil {
		log.V(1).Info("invalid snooze-until timestamp", "value", snoozeUntil, "error", err)
		return false
	}

	// Check if snooze is still active
	if time.Now().Before(snoozeTime) {
		log.V(1).Info("parent is snoozed", "until", snoozeUntil)
		return true
	}

	return false
}

// buildDriftReport constructs a DriftReport from the admission context.
func (h *Handler) buildDriftReport(req admission.Request, obj client.Object, driftResult *drift.DriftResult, phase v1alpha1.DriftReportPhase) *v1alpha1.DriftReport {
	if driftResult.ParentRef == nil {
		return nil
	}

	gvk := obj.GetObjectKind().GroupVersionKind()

	// Build object references
	parentRef := v1alpha1.ObjectReference{
		APIVersion: driftResult.ParentRef.APIVersion,
		Kind:       driftResult.ParentRef.Kind,
		Namespace:  driftResult.ParentRef.Namespace,
		Name:       driftResult.ParentRef.Name,
	}

	// Include parent state info if available
	if driftResult.ParentState != nil {
		parentRef.Generation = driftResult.ParentState.Generation
		parentRef.ObservedGeneration = driftResult.ParentState.ObservedGeneration
		parentRef.ControllerManager = driftResult.ParentState.ControllerManager
	}
	parentRef.LifecyclePhase = string(driftResult.LifecyclePhase)

	childRef := v1alpha1.ObjectReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		UID:        obj.GetUID(),
		Generation: obj.GetGeneration(),
	}

	// Generate ID based on phase
	var id string
	if phase == v1alpha1.DriftReportPhaseDetected {
		// For detected phase, include spec diff in ID
		specDiff := computeSpecDiff(req)
		id = callback.GenerateDriftID(parentRef, childRef, specDiff)
	} else {
		// For resolved phase, use simpler ID
		id = callback.GenerateResolutionID(parentRef, childRef)
	}

	// Build request context
	reqCtx := v1alpha1.RequestContext{
		User:         req.UserInfo.Username,
		Groups:       req.UserInfo.Groups,
		UID:          string(req.UID),
		FieldManager: extractFieldManager(req),
		Operation:    string(req.Operation),
		DryRun:       req.DryRun != nil && *req.DryRun,
	}

	report := &v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:      id,
			Phase:   phase,
			Parent:  parentRef,
			Child:   childRef,
			Request: reqCtx,
		},
	}

	// Include objects in report
	report.Spec.NewObject = runtime.RawExtension{Raw: req.Object.Raw}
	if req.Operation == admissionv1.Update && len(req.OldObject.Raw) > 0 {
		report.Spec.OldObject = &runtime.RawExtension{Raw: req.OldObject.Raw}
	}

	return report
}

// computeSpecDiff computes a hash-able representation of the spec change.
func computeSpecDiff(req admission.Request) []byte {
	if req.Operation != admissionv1.Update {
		return req.Object.Raw
	}

	// For updates, extract just the spec fields for comparison
	oldObj := &unstructured.Unstructured{}
	newObj := &unstructured.Unstructured{}

	if err := runtime.DecodeInto(unstructured.UnstructuredJSONScheme, req.OldObject.Raw, oldObj); err != nil {
		return req.Object.Raw
	}
	if err := runtime.DecodeInto(unstructured.UnstructuredJSONScheme, req.Object.Raw, newObj); err != nil {
		return req.Object.Raw
	}

	oldSpec, _, _ := unstructured.NestedFieldCopy(oldObj.Object, "spec")
	newSpec, _, _ := unstructured.NestedFieldCopy(newObj.Object, "spec")

	// Create a diff representation
	diff := map[string]interface{}{
		"old": oldSpec,
		"new": newSpec,
	}
	diffBytes, err := json.Marshal(diff)
	if err != nil {
		return req.Object.Raw
	}
	return diffBytes
}

// getNamespaceMetadata fetches labels and annotations from a namespace.
func (h *Handler) getNamespaceMetadata(ctx context.Context, namespace string) (labels, annotations map[string]string, err error) {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	if err := h.client.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		return nil, nil, err
	}
	return ns.GetLabels(), ns.GetAnnotations(), nil
}
