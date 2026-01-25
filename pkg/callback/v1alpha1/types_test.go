package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestDriftReport_JSONRoundTrip(t *testing.T) {
	report := DriftReport{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       "DriftReport",
		},
		Spec: DriftReportSpec{
			ID:    "a1b2c3d4e5f67890",
			Phase: DriftReportPhaseDetected,
			Parent: ObjectReference{
				APIVersion:         "example.com/v1alpha1",
				Kind:               "EKSCluster",
				Namespace:          "infra",
				Name:               "prod",
				UID:                types.UID("parent-uid"),
				Generation:         5,
				ObservedGeneration: 5,
				ControllerManager:  "eks-controller",
				LifecyclePhase:     "Initialized",
			},
			Child: ObjectReference{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Namespace:  "infra",
				Name:       "cluster-config",
				UID:        types.UID("child-uid"),
				Generation: 3,
			},
			OldObject: &runtime.RawExtension{Raw: []byte(`{"data":{"key":"old"}}`)},
			NewObject: runtime.RawExtension{Raw: []byte(`{"data":{"key":"new"}}`)},
			Request: RequestContext{
				User:         "system:serviceaccount:infra:eks-controller",
				Groups:       []string{"system:serviceaccounts", "system:serviceaccounts:infra"},
				UID:          "request-uid-123",
				FieldManager: "eks-controller",
				Operation:    "UPDATE",
				DryRun:       true,
			},
		},
	}

	// Marshal to JSON
	data, err := json.Marshal(report)
	require.NoError(t, err)

	// Unmarshal back
	var decoded DriftReport
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	// Verify fields
	assert.Equal(t, report.TypeMeta, decoded.TypeMeta)
	assert.Equal(t, report.Spec.ID, decoded.Spec.ID)
	assert.Equal(t, report.Spec.Phase, decoded.Spec.Phase)
	assert.Equal(t, report.Spec.Parent, decoded.Spec.Parent)
	assert.Equal(t, report.Spec.Child, decoded.Spec.Child)
	assert.Equal(t, report.Spec.Request, decoded.Spec.Request)
}

func TestDriftReportResponse_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		response DriftReportResponse
	}{
		{
			name: "acknowledged",
			response: DriftReportResponse{
				TypeMeta: metav1.TypeMeta{
					APIVersion: GroupName + "/" + Version,
					Kind:       "DriftReportResponse",
				},
				Acknowledged: true,
			},
		},
		{
			name: "error",
			response: DriftReportResponse{
				TypeMeta: metav1.TypeMeta{
					APIVersion: GroupName + "/" + Version,
					Kind:       "DriftReportResponse",
				},
				Acknowledged: false,
				Error:        "failed to process drift report",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.response)
			require.NoError(t, err)

			var decoded DriftReportResponse
			err = json.Unmarshal(data, &decoded)
			require.NoError(t, err)

			assert.Equal(t, tt.response.TypeMeta, decoded.TypeMeta)
			assert.Equal(t, tt.response.Acknowledged, decoded.Acknowledged)
			assert.Equal(t, tt.response.Error, decoded.Error)
		})
	}
}

func TestDriftReportPhase_Values(t *testing.T) {
	assert.Equal(t, DriftReportPhase("Detected"), DriftReportPhaseDetected)
	assert.Equal(t, DriftReportPhase("Resolved"), DriftReportPhaseResolved)
}
