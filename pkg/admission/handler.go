// Package admission provides admission handling for drift detection and tracing.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	jsonpatch "gomodules.xyz/jsonpatch/v2"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kausality-io/kausality/pkg/approval"
	"github.com/kausality-io/kausality/pkg/callback"
	"github.com/kausality-io/kausality/pkg/callback/v1alpha1"
	"github.com/kausality-io/kausality/pkg/config"
	"github.com/kausality-io/kausality/pkg/controller"
	"github.com/kausality-io/kausality/pkg/drift"
	"github.com/kausality-io/kausality/pkg/trace"
)

// Handler handles admission requests for drift detection and tracing.
type Handler struct {
	client            client.Client
	decoder           admission.Decoder
	detector          *drift.Detector
	propagator        *trace.Propagator
	approvalChecker   *approval.Checker
	callbackSender    callback.ReportSender
	controllerTracker *controller.Tracker
	config            *config.Config
	log               logr.Logger
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
	CallbackSender callback.ReportSender
}

// NewHandler creates a new admission Handler.
func NewHandler(cfg Config) *Handler {
	driftConfig := cfg.DriftConfig
	if driftConfig == nil {
		driftConfig = config.Default()
	}
	log := cfg.Log.WithName("kausality-admission")
	return &Handler{
		client:            cfg.Client,
		detector:          drift.NewDetector(cfg.Client),
		propagator:        trace.NewPropagator(cfg.Client),
		approvalChecker:   approval.NewChecker(),
		callbackSender:    cfg.CallbackSender,
		controllerTracker: controller.NewTracker(cfg.Client, log),
		config:            driftConfig,
		log:               log,
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
		"subresource", req.SubResource,
	)

	// Handle CREATE, UPDATE, and DELETE (DELETE just sets deletionTimestamp)
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update && req.Operation != admissionv1.Delete {
		return admission.Allowed("operation not relevant for tracing")
	}

	// Handle status subresource updates - record controller identity
	if req.SubResource == "status" {
		return h.handleStatusUpdate(ctx, req, log)
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
			// No spec change: preserve all kausality annotations (regardless of actor)
			var oldObj, newObj unstructured.Unstructured
			if err := json.Unmarshal(req.OldObject.Raw, &oldObj); err == nil {
				if err := json.Unmarshal(req.Object.Raw, &newObj); err == nil {
					// specChanged=false means newTrace/newUpdaters are unused
					merged := computeAnnotationsForUser(oldObj.GetAnnotations(), newObj.GetAnnotations(), false, "", "")
					newObj.SetAnnotations(merged)
					if modified, err := json.Marshal(newObj.Object); err == nil {
						log.V(1).Info("no spec change, preserving annotations")
						return admission.PatchResponseFromRaw(req.Object.Raw, modified)
					}
				}
			}
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

	// Get existing updaters from OldObject (for UPDATE) or empty (for CREATE)
	var childUpdaters []string
	if req.Operation == admissionv1.Update && len(req.OldObject.Raw) > 0 {
		oldObj := &unstructured.Unstructured{}
		if err := runtime.DecodeInto(unstructured.UnstructuredJSONScheme, req.OldObject.Raw, oldObj); err == nil {
			childUpdaters = drift.ParseUpdaterHashes(oldObj)
		}
	}

	// Get user identifier (username if available, UID as fallback)
	userID := controller.UserIdentifier(req.UserInfo.Username, req.UserInfo.UID)

	// Add user hash for logging
	userHash := controller.HashUsername(userID)
	log = log.WithValues("userHash", userHash)

	// Detect drift using user hash tracking
	driftResult, err := h.detector.Detect(ctx, obj, userID, childUpdaters)
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
		if frozen, freeze := h.checkFreeze(ctx, driftResult.ParentRef, obj.GetNamespace(), log); frozen {
			freezeMsg := fmt.Sprintf("mutation blocked: parent %s", freeze.String())
			log.Info("MUTATION FROZEN", append(logFields, "freezeUser", freeze.User, "freezeMessage", freeze.Message)...)
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
	traceResult, err := h.propagator.Propagate(ctx, obj, userID, childUpdaters, string(req.UID))
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

	// Build annotations with trace and updater
	unstrObj := obj.(*unstructured.Unstructured)
	annotations := unstrObj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// On CREATE, wipe ALL kausality annotations copied from parent (e.g., deployment controller
	// copies Deployment annotations to ReplicaSet). We set fresh values based on our computation.
	if req.Operation == admissionv1.Create {
		for key := range annotations {
			if strings.HasPrefix(key, "kausality.io/") {
				delete(annotations, key)
			}
		}
	}

	newTrace := traceResult.Trace.String()
	newUpdaters := addHash(annotations[controller.UpdatersAnnotation], userHash)

	// Build patches - need to handle case where annotations don't exist
	var patches []jsonpatch.JsonPatchOperation

	// Check if the original object has annotations
	originalAnnotations, _, _ := unstructured.NestedStringMap(unstrObj.Object, "metadata", "annotations")
	if len(originalAnnotations) == 0 {
		// No annotations exist - add the whole annotations object
		patches = append(patches, jsonpatch.JsonPatchOperation{
			Operation: "add",
			Path:      "/metadata/annotations",
			Value: map[string]string{
				trace.TraceAnnotation:         newTrace,
				controller.UpdatersAnnotation: newUpdaters,
			},
		})
	} else {
		// Annotations exist - use replace for existing keys, add for new ones
		tracePath := "/metadata/annotations/" + strings.ReplaceAll(trace.TraceAnnotation, "/", "~1")
		updatersPath := "/metadata/annotations/" + strings.ReplaceAll(controller.UpdatersAnnotation, "/", "~1")

		// Check if keys exist to decide add vs replace
		traceOp := "add"
		if _, exists := originalAnnotations[trace.TraceAnnotation]; exists {
			traceOp = "replace"
		}
		updatersOp := "add"
		if _, exists := originalAnnotations[controller.UpdatersAnnotation]; exists {
			updatersOp = "replace"
		}

		patches = append(patches, jsonpatch.JsonPatchOperation{
			Operation: traceOp,
			Path:      tracePath,
			Value:     newTrace,
		})
		patches = append(patches, jsonpatch.JsonPatchOperation{
			Operation: updatersOp,
			Path:      updatersPath,
			Value:     newUpdaters,
		})
	}

	// Build response manually to ensure patch is serialized correctly
	patchType := admissionv1.PatchTypeJSONPatch
	resp := admission.Response{
		Patches: patches,
		AdmissionResponse: admissionv1.AdmissionResponse{
			Allowed:   true,
			PatchType: &patchType,
		},
	}

	return withWarnings(resp, warnings)
}

// handleStatusUpdate handles status subresource updates to record controller identity.
// It also protects our annotations from being overwritten by stale controller caches.
func (h *Handler) handleStatusUpdate(ctx context.Context, req admission.Request, log logr.Logger) admission.Response {
	if req.Operation != admissionv1.Update {
		return admission.Allowed("status subresource: only UPDATE is relevant")
	}

	// Parse the object for controller tracking
	obj, err := h.parseObject(req)
	if err != nil {
		log.Error(err, "failed to parse object from status update request")
		return admission.Allowed("failed to parse object")
	}

	// Get user identifier (username if available, UID as fallback)
	userID := controller.UserIdentifier(req.UserInfo.Username, req.UserInfo.UID)
	userHash := controller.HashUsername(userID)
	log.V(1).Info("status update", "userHash", userHash)

	// Record controller asynchronously as backup (in case sync patch fails)
	h.controllerTracker.RecordControllerAsync(ctx, obj, userID)

	// Compute annotations: preserve kausality annotations and add user to controllers
	var oldObj, newObj unstructured.Unstructured
	if err := json.Unmarshal(req.OldObject.Raw, &oldObj); err == nil {
		if err := json.Unmarshal(req.Object.Raw, &newObj); err == nil {
			merged := computeAnnotationsForStatusUpdate(oldObj.GetAnnotations(), newObj.GetAnnotations(), userHash)
			newObj.SetAnnotations(merged)
			if modified, err := json.Marshal(newObj.Object); err == nil {
				log.V(1).Info("status update, added controller hash and preserved annotations")
				return admission.PatchResponseFromRaw(req.Object.Raw, modified)
			}
		}
	}

	return admission.Allowed("status update recorded")
}

// withWarnings adds warnings to an admission response.
func withWarnings(resp admission.Response, warnings []string) admission.Response {
	if len(warnings) > 0 {
		resp.Warnings = append(resp.Warnings, warnings...)
	}
	return resp
}

// addHash adds a hash to a comma-separated string if not already present.
func addHash(existing, hash string) string {
	hashes := controller.ParseHashes(existing)
	for _, h := range hashes {
		if h == hash {
			return existing
		}
	}
	hashes = append(hashes, hash)
	if len(hashes) > controller.MaxHashes {
		hashes = hashes[len(hashes)-controller.MaxHashes:]
	}
	return strings.Join(hashes, ",")
}

// kausalityPrefix is the prefix for all kausality annotations.
const kausalityPrefix = "kausality.io/"

// systemAnnotations are annotations with special handling (recomputed on spec change).
var systemAnnotations = map[string]bool{
	trace.TraceAnnotation:            true,
	controller.UpdatersAnnotation:    true,
	controller.ControllersAnnotation: true,
}

// isSystemAnnotation returns true for annotations that get special handling.
func isSystemAnnotation(key string) bool {
	return systemAnnotations[key]
}

// isKausalityAnnotation returns true for any kausality.io/* annotation.
func isKausalityAnnotation(key string) bool {
	return strings.HasPrefix(key, kausalityPrefix)
}

// computeAnnotationsForController computes annotations for controller updates.
// - No spec change: preserve ALL kausality annotations from old
// - Spec change: set system annotations to computed values, preserve user annotations from old
func computeAnnotationsForController(old, new map[string]string, specChanged bool, newTrace, newUpdaters string) map[string]string {
	result := copyAnnotations(new)

	if specChanged {
		// Set system annotations to computed values
		result[trace.TraceAnnotation] = newTrace
		result[controller.UpdatersAnnotation] = newUpdaters
		// Preserve controllers annotation from old (not recomputed on child spec updates).
		// A child can also be a parent (e.g., ReplicaSet is parent to Pods).
		if oldControllers, ok := old[controller.ControllersAnnotation]; ok {
			result[controller.ControllersAnnotation] = oldControllers
		}
		// Preserve user annotations from old
		for key, oldVal := range old {
			if isKausalityAnnotation(key) && !isSystemAnnotation(key) {
				result[key] = oldVal
			}
		}
	} else {
		// No spec change: preserve ALL kausality annotations from old
		for key, oldVal := range old {
			if isKausalityAnnotation(key) {
				result[key] = oldVal
			}
		}
	}
	return result
}

// computeAnnotationsForUser computes annotations for user updates.
// - No spec change: preserve ALL kausality annotations from old
// - Spec change: set system annotations to computed values (new origin, no preservation)
func computeAnnotationsForUser(old, new map[string]string, specChanged bool, newTrace, newUpdaters string) map[string]string {
	result := copyAnnotations(new)

	if specChanged {
		// New origin: set system annotations, no preservation from old
		result[trace.TraceAnnotation] = newTrace
		result[controller.UpdatersAnnotation] = newUpdaters
	} else {
		// No spec change: preserve ALL kausality annotations from old
		for key, oldVal := range old {
			if isKausalityAnnotation(key) {
				result[key] = oldVal
			}
		}
	}
	return result
}

// computeAnnotationsForStatusUpdate computes annotations for status subresource updates.
// Preserves all kausality annotations and adds the user hash to the controllers annotation.
func computeAnnotationsForStatusUpdate(old, new map[string]string, userHash string) map[string]string {
	result := copyAnnotations(new)
	// Preserve all kausality annotations from old
	for key, oldVal := range old {
		if isKausalityAnnotation(key) {
			result[key] = oldVal
		}
	}
	// Add user to controllers annotation (status updater = controller)
	oldControllers := result[controller.ControllersAnnotation]
	result[controller.ControllersAnnotation] = addHash(oldControllers, userHash)
	return result
}

// copyAnnotations creates a copy of the annotations map.
func copyAnnotations(m map[string]string) map[string]string {
	result := make(map[string]string, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
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

	// Parse as unstructured - GVK is already in the raw JSON
	obj := &unstructured.Unstructured{}
	if err := runtime.DecodeInto(unstructured.UnstructuredJSONScheme, rawObj, obj); err != nil {
		return nil, fmt.Errorf("failed to decode object: %w", err)
	}

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
// Returns the parsed Freeze struct with user/message/timestamp info.
func (h *Handler) checkFreeze(ctx context.Context, ref *drift.ParentRef, childNamespace string, log logr.Logger) (frozen bool, freeze *approval.Freeze) {
	parent, err := h.fetchParent(ctx, ref, childNamespace)
	if err != nil {
		log.V(1).Info("failed to fetch parent for freeze check", "error", err)
		return false, nil
	}

	annotations := parent.GetAnnotations()
	if annotations == nil {
		return false, nil
	}

	freezeValue, ok := annotations[approval.FreezeAnnotation]
	if !ok || freezeValue == "" {
		return false, nil
	}

	// Support "false" to explicitly disable
	if freezeValue == "false" {
		return false, nil
	}

	// Parse the structured freeze annotation
	freeze, err = approval.ParseFreeze(freezeValue)
	if err != nil {
		log.V(1).Info("invalid freeze annotation", "value", freezeValue, "error", err)
		// Treat invalid JSON as frozen (fail closed) with no metadata
		return true, &approval.Freeze{}
	}

	return true, freeze
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
	if parent != nil {
		if snooze := h.isParentSnoozed(parent, log); snooze != nil {
			log.V(1).Info("drift callback suppressed", "phase", phase, "snooze", snooze.String())
			return
		}
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
// Returns the parsed Snooze struct if active, nil otherwise.
func (h *Handler) isParentSnoozed(parent client.Object, log logr.Logger) *approval.Snooze {
	if parent == nil {
		return nil
	}

	annotations := parent.GetAnnotations()
	if annotations == nil {
		return nil
	}

	snoozeValue, ok := annotations[approval.SnoozeAnnotation]
	if !ok || snoozeValue == "" {
		return nil
	}

	// Parse the structured snooze annotation
	snooze, err := approval.ParseSnooze(snoozeValue)
	if err != nil {
		log.V(1).Info("invalid snooze annotation", "value", snoozeValue, "error", err)
		return nil
	}

	// Check if snooze is still active
	if snooze.IsActive() {
		log.V(1).Info("parent is snoozed", "snooze", snooze.String())
		return snooze
	}

	return nil
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
