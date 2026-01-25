package backend

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kausality-io/kausality/pkg/callback/v1alpha1"
)

func TestServer_Webhook_ReceivesDriftReport(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	report := v1alpha1.DriftReport{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kausality.io/v1alpha1",
			Kind:       "DriftReport",
		},
		Spec: v1alpha1.DriftReportSpec{
			ID:    "webhook-test-001",
			Phase: v1alpha1.DriftReportPhaseDetected,
			Parent: v1alpha1.ObjectReference{
				APIVersion:         "apps/v1",
				Kind:               "Deployment",
				Namespace:          "production",
				Name:               "api-server",
				ObservedGeneration: 5,
				ControllerManager:  "deployment-controller",
				LifecyclePhase:     "Initialized",
			},
			Child: v1alpha1.ObjectReference{
				APIVersion: "v1",
				Kind:       "Secret",
				Namespace:  "production",
				Name:       "api-credentials",
			},
			Request: v1alpha1.RequestContext{
				User:         "system:serviceaccount:kube-system:deployment-controller",
				Groups:       []string{"system:serviceaccounts", "system:authenticated"},
				UID:          "abc-123",
				Operation:    "UPDATE",
				FieldManager: "deployment-controller",
			},
		},
	}

	body, err := json.Marshal(report)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify response
	var response v1alpha1.DriftReportResponse
	err = json.Unmarshal(rec.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.True(t, response.Acknowledged)

	// Verify stored
	assert.Equal(t, 1, server.Store().Count())
	stored, ok := server.Store().Get("webhook-test-001")
	require.True(t, ok)
	assert.Equal(t, "Deployment", stored.Report.Spec.Parent.Kind)
	assert.Equal(t, "api-server", stored.Report.Spec.Parent.Name)
	assert.Equal(t, "Secret", stored.Report.Spec.Child.Kind)
	assert.Equal(t, "deployment-controller", stored.Report.Spec.Request.FieldManager)
}

func TestServer_Webhook_WithOldAndNewObject(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	oldObj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "app-config",
			"namespace": "default",
		},
		"data": map[string]interface{}{
			"key": "old-value",
		},
	}
	newObj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "app-config",
			"namespace": "default",
		},
		"data": map[string]interface{}{
			"key": "new-value",
		},
	}

	oldBytes, _ := json.Marshal(oldObj)
	newBytes, _ := json.Marshal(newObj)

	report := v1alpha1.DriftReport{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kausality.io/v1alpha1",
			Kind:       "DriftReport",
		},
		Spec: v1alpha1.DriftReportSpec{
			ID:    "drift-with-objects",
			Phase: v1alpha1.DriftReportPhaseDetected,
			Parent: v1alpha1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Namespace:  "default",
				Name:       "my-app",
			},
			Child: v1alpha1.ObjectReference{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Namespace:  "default",
				Name:       "app-config",
			},
			OldObject: &runtime.RawExtension{Raw: oldBytes},
			NewObject: runtime.RawExtension{Raw: newBytes},
			Request: v1alpha1.RequestContext{
				User:      "my-controller",
				Operation: "UPDATE",
			},
		},
	}

	body, _ := json.Marshal(report)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	stored, ok := server.Store().Get("drift-with-objects")
	require.True(t, ok)
	assert.NotNil(t, stored.Report.Spec.OldObject)
	assert.NotEmpty(t, stored.Report.Spec.OldObject.Raw)
	assert.NotEmpty(t, stored.Report.Spec.NewObject.Raw)
}

func TestServer_Webhook_Resolved_RemovesDrift(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	// First send detected
	detected := v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "resolve-test",
			Phase: v1alpha1.DriftReportPhaseDetected,
		},
	}
	body, _ := json.Marshal(detected)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, server.Store().Count())

	// Then send resolved
	resolved := v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "resolve-test",
			Phase: v1alpha1.DriftReportPhaseResolved,
		},
	}
	body, _ = json.Marshal(resolved)
	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 0, server.Store().Count())
}

func TestServer_Webhook_InvalidJSON(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServer_ListDrifts(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	// Add some drifts via webhook
	reports := []v1alpha1.DriftReport{
		{
			Spec: v1alpha1.DriftReportSpec{
				ID:    "list-test-1",
				Phase: v1alpha1.DriftReportPhaseDetected,
				Parent: v1alpha1.ObjectReference{
					Kind: "Deployment",
					Name: "app-1",
				},
			},
		},
		{
			Spec: v1alpha1.DriftReportSpec{
				ID:    "list-test-2",
				Phase: v1alpha1.DriftReportPhaseDetected,
				Parent: v1alpha1.ObjectReference{
					Kind: "StatefulSet",
					Name: "db-1",
				},
			},
		},
	}

	for _, r := range reports {
		body, _ := json.Marshal(r)
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	// List drifts
	req := httptest.NewRequest(http.MethodGet, "/api/v1/drifts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var result struct {
		Items []*StoredReport `json:"items"`
		Count int             `json:"count"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &result)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Count)
	assert.Len(t, result.Items, 2)
}

func TestServer_GetDrift(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	// Add a drift
	report := v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "get-test",
			Phase: v1alpha1.DriftReportPhaseDetected,
			Parent: v1alpha1.ObjectReference{
				Kind:      "Deployment",
				Name:      "specific-app",
				Namespace: "prod",
			},
		},
	}
	body, _ := json.Marshal(report)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Get specific drift
	req = httptest.NewRequest(http.MethodGet, "/api/v1/drifts/get-test", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var stored StoredReport
	err := json.Unmarshal(rec.Body.Bytes(), &stored)
	require.NoError(t, err)
	assert.Equal(t, "get-test", stored.Report.Spec.ID)
	assert.Equal(t, "specific-app", stored.Report.Spec.Parent.Name)
}

func TestServer_GetDrift_NotFound(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/drifts/non-existent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_DeleteDrift(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	// Add a drift
	report := v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "delete-test",
			Phase: v1alpha1.DriftReportPhaseDetected,
		},
	}
	body, _ := json.Marshal(report)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, 1, server.Store().Count())

	// Delete it
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/drifts/delete-test", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 0, server.Store().Count())
}

func TestServer_Health(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	// Add a drift
	server.Store().Add(&v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "health-test",
			Phase: v1alpha1.DriftReportPhaseDetected,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var health struct {
		Status     string `json:"status"`
		DriftCount int    `json:"driftCount"`
		Time       string `json:"time"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &health)
	require.NoError(t, err)
	assert.Equal(t, "ok", health.Status)
	assert.Equal(t, 1, health.DriftCount)
	assert.NotEmpty(t, health.Time)
}

func TestServer_FullWorkflow(t *testing.T) {
	server := NewServer()
	handler := server.Handler()

	// 1. Receive drift detection
	detected := v1alpha1.DriftReport{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kausality.io/v1alpha1",
			Kind:       "DriftReport",
		},
		Spec: v1alpha1.DriftReportSpec{
			ID:    "workflow-test",
			Phase: v1alpha1.DriftReportPhaseDetected,
			Parent: v1alpha1.ObjectReference{
				APIVersion:         "apps/v1",
				Kind:               "Deployment",
				Namespace:          "default",
				Name:               "web-app",
				Generation:         10,
				ObservedGeneration: 10,
				ControllerManager:  "web-controller",
				LifecyclePhase:     "Initialized",
			},
			Child: v1alpha1.ObjectReference{
				APIVersion: "v1",
				Kind:       "Service",
				Namespace:  "default",
				Name:       "web-app-svc",
			},
			Request: v1alpha1.RequestContext{
				User:         "system:serviceaccount:default:web-controller",
				UID:          "req-123",
				Operation:    "UPDATE",
				FieldManager: "web-controller",
			},
		},
	}

	body, _ := json.Marshal(detected)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// 2. Verify it's listed
	req = httptest.NewRequest(http.MethodGet, "/api/v1/drifts", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var listResult struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResult)
	assert.Equal(t, 1, listResult.Count)

	// 3. Get details
	req = httptest.NewRequest(http.MethodGet, "/api/v1/drifts/workflow-test", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var detail StoredReport
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	assert.Equal(t, "web-controller", detail.Report.Spec.Parent.ControllerManager)

	// 4. Receive resolution
	resolved := v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "workflow-test",
			Phase: v1alpha1.DriftReportPhaseResolved,
		},
	}
	body, _ = json.Marshal(resolved)
	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 5. Verify it's gone
	req = httptest.NewRequest(http.MethodGet, "/api/v1/drifts", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body, _ = io.ReadAll(rec.Body)
	_ = json.Unmarshal(body, &listResult)
	assert.Equal(t, 0, listResult.Count)
}
