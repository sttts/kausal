// Package admission provides a kausality admission plugin for k8s.io/apiserver.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-logr/logr"
	jsonpatch "gomodules.xyz/jsonpatch/v2"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crAdmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kausalityAdmission "github.com/kausality-io/kausality/pkg/admission"
	"github.com/kausality-io/kausality/pkg/policy"
)

// PluginName is the name of this admission plugin.
const PluginName = "Kausality"

// Register registers the kausality admission plugin.
func Register(plugins *admission.Plugins, c client.Client, log logr.Logger, resolver policy.Resolver) {
	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		return NewKausalityAdmission(c, log, resolver), nil
	})
}

// KausalityAdmission implements k8s.io/apiserver admission.MutationInterface.
// It wraps the kausality admission handler to provide drift detection and tracing.
type KausalityAdmission struct {
	handler *kausalityAdmission.Handler
	scheme  *runtime.Scheme
	log     logr.Logger
}

// NewKausalityAdmission creates a new kausality admission plugin.
func NewKausalityAdmission(c client.Client, log logr.Logger, resolver policy.Resolver) *KausalityAdmission {
	handler := kausalityAdmission.NewHandler(kausalityAdmission.Config{
		Client:         c,
		Log:            log,
		PolicyResolver: resolver,
	})
	return &KausalityAdmission{
		handler: handler,
		log:     log.WithName("kausality-admission"),
	}
}

// SetScheme sets the scheme for the admission plugin.
func (k *KausalityAdmission) SetScheme(scheme *runtime.Scheme) {
	k.scheme = scheme
}

// Handles returns true if this plugin handles the given operation.
func (k *KausalityAdmission) Handles(operation admission.Operation) bool {
	switch operation {
	case admission.Create, admission.Update, admission.Delete:
		return true
	default:
		return false
	}
}

// Admit performs the admission mutation.
func (k *KausalityAdmission) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	// Skip subresources except status
	if a.GetSubresource() != "" && a.GetSubresource() != "status" {
		return nil
	}

	// Convert admission.Attributes to controller-runtime admission.Request
	req, err := k.toAdmissionRequest(a)
	if err != nil {
		k.log.Error(err, "failed to convert admission attributes to request")
		return nil // Don't fail on conversion errors
	}

	// Call kausality handler
	resp := k.handler.Handle(ctx, req)

	// Handle denial
	if !resp.Allowed {
		return admission.NewForbidden(a, fmt.Errorf("%s", resp.Result.Message))
	}

	// Apply patches to the object
	if len(resp.Patches) > 0 {
		obj := a.GetObject()
		if err := k.applyPatches(obj, resp.Patches); err != nil {
			k.log.Error(err, "failed to apply patches")
			// Don't fail on patch errors - the handler allowed it
		}
	}

	// Log warnings
	for _, w := range resp.Warnings {
		k.log.Info("admission warning", "warning", w)
	}

	return nil
}

// toAdmissionRequest converts k8s.io/apiserver attributes to controller-runtime request.
func (k *KausalityAdmission) toAdmissionRequest(a admission.Attributes) (crAdmission.Request, error) {
	// Get GVK
	gvk := a.GetKind()

	// Serialize objects to JSON
	var objectRaw, oldObjectRaw []byte
	var err error

	if a.GetObject() != nil {
		objectRaw, err = json.Marshal(a.GetObject())
		if err != nil {
			return crAdmission.Request{}, fmt.Errorf("failed to marshal object: %w", err)
		}
	}

	if a.GetOldObject() != nil {
		oldObjectRaw, err = json.Marshal(a.GetOldObject())
		if err != nil {
			return crAdmission.Request{}, fmt.Errorf("failed to marshal old object: %w", err)
		}
	}

	// Map operation
	var op admissionv1.Operation
	switch a.GetOperation() {
	case admission.Create:
		op = admissionv1.Create
	case admission.Update:
		op = admissionv1.Update
	case admission.Delete:
		op = admissionv1.Delete
	default:
		op = admissionv1.Operation(string(a.GetOperation()))
	}

	// Get user info
	userInfo := a.GetUserInfo()

	return crAdmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "embedded-apiserver",
			Kind:      metav1.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind},
			Resource:  metav1.GroupVersionResource{Group: a.GetResource().Group, Version: a.GetResource().Version, Resource: a.GetResource().Resource},
			Namespace: a.GetNamespace(),
			Name:      a.GetName(),
			Operation: op,
			UserInfo: authenticationv1.UserInfo{
				Username: userInfo.GetName(),
				UID:      userInfo.GetUID(),
				Groups:   userInfo.GetGroups(),
			},
			Object:      runtime.RawExtension{Raw: objectRaw},
			OldObject:   runtime.RawExtension{Raw: oldObjectRaw},
			SubResource: a.GetSubresource(),
		},
	}, nil
}

// applyPatches applies JSON patches to the object.
func (k *KausalityAdmission) applyPatches(obj runtime.Object, patches []jsonpatch.JsonPatchOperation) error {
	// Serialize to JSON
	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("failed to marshal object: %w", err)
	}

	// Apply patches manually by modifying the map
	var objMap map[string]interface{}
	if err := json.Unmarshal(data, &objMap); err != nil {
		return fmt.Errorf("failed to unmarshal object: %w", err)
	}

	for _, patch := range patches {
		if err := applyPatch(objMap, patch); err != nil {
			return fmt.Errorf("failed to apply patch %v: %w", patch, err)
		}
	}

	// Serialize back
	data, err = json.Marshal(objMap)
	if err != nil {
		return fmt.Errorf("failed to marshal patched object: %w", err)
	}

	// Unmarshal back into the original object
	return json.Unmarshal(data, obj)
}

// applyPatch applies a single JSON patch operation.
func applyPatch(obj map[string]interface{}, patch jsonpatch.JsonPatchOperation) error {
	// Parse path (e.g., "/metadata/annotations/kausality.io~1trace")
	parts := parsePath(patch.Path)
	if len(parts) == 0 {
		return fmt.Errorf("empty path")
	}

	// Navigate to parent
	current := obj
	for i := 0; i < len(parts)-1; i++ {
		key := parts[i]
		next, ok := current[key]
		if !ok {
			if patch.Operation == "add" {
				// Create intermediate objects
				newMap := make(map[string]interface{})
				current[key] = newMap
				current = newMap
				continue
			}
			return fmt.Errorf("path not found: %s", key)
		}
		nextMap, ok := next.(map[string]interface{})
		if !ok {
			return fmt.Errorf("path component %s is not an object", key)
		}
		current = nextMap
	}

	// Apply operation on last key
	lastKey := parts[len(parts)-1]
	switch patch.Operation {
	case "add", "replace":
		current[lastKey] = patch.Value
	case "remove":
		delete(current, lastKey)
	default:
		return fmt.Errorf("unsupported operation: %s", patch.Operation)
	}

	return nil
}

// parsePath parses a JSON pointer path into segments.
func parsePath(path string) []string {
	if path == "" || path == "/" {
		return nil
	}
	// Remove leading /
	if path[0] == '/' {
		path = path[1:]
	}
	// Split by /
	parts := []string{}
	current := ""
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			parts = append(parts, unescapePathSegment(current))
			current = ""
		} else {
			current += string(path[i])
		}
	}
	if current != "" {
		parts = append(parts, unescapePathSegment(current))
	}
	return parts
}

// unescapePathSegment unescapes JSON pointer escape sequences.
func unescapePathSegment(s string) string {
	// ~1 -> /
	// ~0 -> ~
	result := ""
	for i := 0; i < len(s); i++ {
		if i < len(s)-1 && s[i] == '~' {
			switch s[i+1] {
			case '1':
				result += "/"
				i++
				continue
			case '0':
				result += "~"
				i++
				continue
			}
		}
		result += string(s[i])
	}
	return result
}

// HandleDirect calls the underlying kausality handler directly.
// This is useful for testing without going through the k8s.io/apiserver wrapper.
func (k *KausalityAdmission) HandleDirect(ctx context.Context, req crAdmission.Request) crAdmission.Response {
	return k.handler.Handle(ctx, req)
}

// Ensure KausalityAdmission implements admission.MutationInterface.
var _ admission.MutationInterface = &KausalityAdmission{}

// AdmissionResponse wraps an admission response with HTTP helpers.
type AdmissionResponse struct {
	Allowed  bool
	Message  string
	Warnings []string
}

// WriteResponse writes the admission response to the HTTP response writer.
func (r *AdmissionResponse) WriteResponse(w http.ResponseWriter) {
	resp := &admissionv1.AdmissionResponse{
		Allowed: r.Allowed,
	}
	if !r.Allowed && r.Message != "" {
		resp.Result = &metav1.Status{
			Message: r.Message,
			Code:    http.StatusForbidden,
		}
	}
	resp.Warnings = r.Warnings

	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionv1.SchemeGroupVersion.String(),
			Kind:       "AdmissionReview",
		},
		Response: resp,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(review); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// DefaultGVK returns the default GVK for unknown resources.
func DefaultGVK(resource schema.GroupVersionResource) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   resource.Group,
		Version: resource.Version,
		Kind:    resource.Resource, // Best guess
	}
}
