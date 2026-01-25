package admission

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestHasSpecChanged(t *testing.T) {
	h := &Handler{}

	tests := []struct {
		name        string
		oldObj      map[string]interface{}
		newObj      map[string]interface{}
		wantChanged bool
	}{
		{
			name: "spec unchanged",
			oldObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
				"spec":       map[string]interface{}{"replicas": 3},
			},
			newObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test", "labels": map[string]interface{}{"foo": "bar"}},
				"spec":       map[string]interface{}{"replicas": 3},
			},
			wantChanged: false,
		},
		{
			name: "spec changed",
			oldObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
				"spec":       map[string]interface{}{"replicas": 3},
			},
			newObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
				"spec":       map[string]interface{}{"replicas": 5},
			},
			wantChanged: true,
		},
		{
			name: "status only changed",
			oldObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
				"spec":       map[string]interface{}{"replicas": 3},
				"status":     map[string]interface{}{"ready": false},
			},
			newObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
				"spec":       map[string]interface{}{"replicas": 3},
				"status":     map[string]interface{}{"ready": true},
			},
			wantChanged: false,
		},
		{
			name: "no spec in either",
			oldObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
			},
			newObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
			},
			wantChanged: false,
		},
		{
			name: "spec added",
			oldObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
			},
			newObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
				"spec":       map[string]interface{}{"replicas": 3},
			},
			wantChanged: true,
		},
		{
			name: "spec removed",
			oldObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
				"spec":       map[string]interface{}{"replicas": 3},
			},
			newObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": "test"},
			},
			wantChanged: true,
		},
		{
			name: "nested spec change",
			oldObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"image": "nginx:1.0"},
						},
					},
				},
			},
			newObj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{"image": "nginx:2.0"},
						},
					},
				},
			},
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldRaw, _ := json.Marshal(tt.oldObj)
			newRaw, _ := json.Marshal(tt.newObj)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					OldObject: runtime.RawExtension{Raw: oldRaw},
					Object:    runtime.RawExtension{Raw: newRaw},
				},
			}

			changed, err := h.hasSpecChanged(req)
			require.NoError(t, err)
			assert.Equal(t, tt.wantChanged, changed)
		})
	}
}

func TestComputeAnnotationsForController(t *testing.T) {
	tests := []struct {
		name        string
		old         map[string]string
		new         map[string]string
		specChanged bool
		newTrace    string
		newUpdaters string
		want        map[string]string
	}{
		{
			name:        "no spec change: preserve all kausality annotations",
			old:         map[string]string{"kausality.io/trace": "old-trace", "kausality.io/updaters": "abc12", "other": "value"},
			new:         map[string]string{"kausality.io/trace": "stale-trace", "kausality.io/updaters": "stale", "other": "value"},
			specChanged: false,
			newTrace:    "", // unused when specChanged=false
			newUpdaters: "",
			want:        map[string]string{"kausality.io/trace": "old-trace", "kausality.io/updaters": "abc12", "other": "value"},
		},
		{
			name:        "no spec change: preserve user annotations from old",
			old:         map[string]string{"kausality.io/approvals": "[...]", "kausality.io/freeze": "true"},
			new:         map[string]string{},
			specChanged: false,
			newTrace:    "",
			newUpdaters: "",
			want:        map[string]string{"kausality.io/approvals": "[...]", "kausality.io/freeze": "true"},
		},
		{
			name:        "spec changed: use computed system, preserve user annotations",
			old:         map[string]string{"kausality.io/trace": "old", "kausality.io/updaters": "abc", "kausality.io/approvals": "[...]"},
			new:         map[string]string{"kausality.io/trace": "stale"},
			specChanged: true,
			newTrace:    "computed-trace",
			newUpdaters: "computed-updaters",
			want:        map[string]string{"kausality.io/trace": "computed-trace", "kausality.io/updaters": "computed-updaters", "kausality.io/approvals": "[...]"},
		},
		{
			name:        "spec changed: preserve controllers (child can also be parent)",
			old:         map[string]string{"kausality.io/trace": "old", "kausality.io/updaters": "abc", "kausality.io/controllers": "def"},
			new:         map[string]string{},
			specChanged: true,
			newTrace:    "new-trace",
			newUpdaters: "new-updaters",
			want:        map[string]string{"kausality.io/trace": "new-trace", "kausality.io/updaters": "new-updaters", "kausality.io/controllers": "def"},
		},
		{
			name:        "non-kausality annotations pass through",
			old:         map[string]string{"other.io/annotation": "old"},
			new:         map[string]string{"other.io/annotation": "new"},
			specChanged: false,
			newTrace:    "",
			newUpdaters: "",
			want:        map[string]string{"other.io/annotation": "new"},
		},
		{
			name:        "nil old annotations",
			old:         nil,
			new:         map[string]string{"other": "value"},
			specChanged: true,
			newTrace:    "trace",
			newUpdaters: "updaters",
			want:        map[string]string{"other": "value", "kausality.io/trace": "trace", "kausality.io/updaters": "updaters"},
		},
		{
			name:        "preserve trace-* user annotations on spec change",
			old:         map[string]string{"kausality.io/trace-ticket": "JIRA-123", "kausality.io/trace": "old"},
			new:         map[string]string{},
			specChanged: true,
			newTrace:    "new-trace",
			newUpdaters: "new-updaters",
			want:        map[string]string{"kausality.io/trace": "new-trace", "kausality.io/updaters": "new-updaters", "kausality.io/trace-ticket": "JIRA-123"},
		},
		{
			name:        "preserve freeze annotation on spec change",
			old:         map[string]string{"kausality.io/freeze": `{"user":"admin"}`, "kausality.io/trace": "old"},
			new:         map[string]string{},
			specChanged: true,
			newTrace:    "new",
			newUpdaters: "upd",
			want:        map[string]string{"kausality.io/trace": "new", "kausality.io/updaters": "upd", "kausality.io/freeze": `{"user":"admin"}`},
		},
		{
			name:        "child can be parent: preserve controllers on spec change",
			old:         map[string]string{"kausality.io/trace": "old", "kausality.io/updaters": "abc", "kausality.io/controllers": "pod-ctrl", "kausality.io/approvals": "[...]"},
			new:         map[string]string{"kausality.io/trace": "stale"},
			specChanged: true,
			newTrace:    "new-trace",
			newUpdaters: "new-updaters",
			want:        map[string]string{"kausality.io/trace": "new-trace", "kausality.io/updaters": "new-updaters", "kausality.io/controllers": "pod-ctrl", "kausality.io/approvals": "[...]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeAnnotationsForController(tt.old, tt.new, tt.specChanged, tt.newTrace, tt.newUpdaters)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestComputeAnnotationsForUser(t *testing.T) {
	tests := []struct {
		name        string
		old         map[string]string
		new         map[string]string
		specChanged bool
		newTrace    string
		newUpdaters string
		want        map[string]string
	}{
		{
			name:        "no spec change: preserve all kausality annotations",
			old:         map[string]string{"kausality.io/trace": "old-trace", "kausality.io/updaters": "abc12"},
			new:         map[string]string{"kausality.io/trace": "stale", "kausality.io/updaters": "stale"},
			specChanged: false,
			newTrace:    "", // unused when specChanged=false
			newUpdaters: "",
			want:        map[string]string{"kausality.io/trace": "old-trace", "kausality.io/updaters": "abc12"},
		},
		{
			name:        "spec changed: new origin with computed system annotations",
			old:         map[string]string{"kausality.io/trace": "old", "kausality.io/approvals": "[...]"},
			new:         map[string]string{},
			specChanged: true,
			newTrace:    "new-origin-trace",
			newUpdaters: "user-hash",
			want:        map[string]string{"kausality.io/trace": "new-origin-trace", "kausality.io/updaters": "user-hash"},
		},
		{
			name:        "spec changed: user annotations NOT preserved (new origin)",
			old:         map[string]string{"kausality.io/freeze": "true", "kausality.io/approvals": "[...]"},
			new:         map[string]string{},
			specChanged: true,
			newTrace:    "trace",
			newUpdaters: "upd",
			want:        map[string]string{"kausality.io/trace": "trace", "kausality.io/updaters": "upd"},
		},
		{
			name:        "no spec change: preserve user annotations too",
			old:         map[string]string{"kausality.io/trace": "t", "kausality.io/freeze": "true"},
			new:         map[string]string{},
			specChanged: false,
			newTrace:    "",
			newUpdaters: "",
			want:        map[string]string{"kausality.io/trace": "t", "kausality.io/freeze": "true"},
		},
		{
			name:        "nil old annotations with spec change",
			old:         nil,
			new:         map[string]string{"other": "value"},
			specChanged: true,
			newTrace:    "trace",
			newUpdaters: "upd",
			want:        map[string]string{"other": "value", "kausality.io/trace": "trace", "kausality.io/updaters": "upd"},
		},
		{
			name:        "non-kausality annotations pass through",
			old:         map[string]string{"other.io/ann": "old"},
			new:         map[string]string{"other.io/ann": "new"},
			specChanged: true,
			newTrace:    "t",
			newUpdaters: "u",
			want:        map[string]string{"other.io/ann": "new", "kausality.io/trace": "t", "kausality.io/updaters": "u"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeAnnotationsForUser(tt.old, tt.new, tt.specChanged, tt.newTrace, tt.newUpdaters)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestComputeAnnotationsForStatusUpdate(t *testing.T) {
	tests := []struct {
		name     string
		old      map[string]string
		new      map[string]string
		userHash string
		want     map[string]string
	}{
		{
			name:     "preserve all kausality annotations and add controller hash",
			old:      map[string]string{"kausality.io/trace": "old-trace", "kausality.io/updaters": "abc12", "kausality.io/controllers": "def34"},
			new:      map[string]string{"kausality.io/trace": "stale", "kausality.io/updaters": "stale"},
			userHash: "new12",
			want:     map[string]string{"kausality.io/trace": "old-trace", "kausality.io/updaters": "abc12", "kausality.io/controllers": "def34,new12"},
		},
		{
			name:     "first controller hash when none exists",
			old:      map[string]string{"kausality.io/trace": "trace"},
			new:      map[string]string{},
			userHash: "first",
			want:     map[string]string{"kausality.io/trace": "trace", "kausality.io/controllers": "first"},
		},
		{
			name:     "preserve user annotations from old",
			old:      map[string]string{"kausality.io/approvals": "[...]", "kausality.io/freeze": "true"},
			new:      map[string]string{},
			userHash: "ctrl1",
			want:     map[string]string{"kausality.io/approvals": "[...]", "kausality.io/freeze": "true", "kausality.io/controllers": "ctrl1"},
		},
		{
			name:     "non-kausality annotations pass through from new",
			old:      map[string]string{"other.io/ann": "old", "kausality.io/trace": "old-trace"},
			new:      map[string]string{"other.io/ann": "new"},
			userHash: "ctrl1",
			want:     map[string]string{"other.io/ann": "new", "kausality.io/trace": "old-trace", "kausality.io/controllers": "ctrl1"},
		},
		{
			name:     "nil old annotations",
			old:      nil,
			new:      map[string]string{"other": "value"},
			userHash: "ctrl1",
			want:     map[string]string{"other": "value", "kausality.io/controllers": "ctrl1"},
		},
		{
			name:     "duplicate hash not added",
			old:      map[string]string{"kausality.io/controllers": "ctrl1"},
			new:      map[string]string{},
			userHash: "ctrl1",
			want:     map[string]string{"kausality.io/controllers": "ctrl1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeAnnotationsForStatusUpdate(tt.old, tt.new, tt.userHash)
			assert.Equal(t, tt.want, got)
		})
	}
}
