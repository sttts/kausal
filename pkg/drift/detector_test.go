package drift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kausality-io/kausality/pkg/controller"
)

func TestDetectFromState(t *testing.T) {
	detector := &Detector{
		lifecycleDetector: NewLifecycleDetector(),
	}

	tests := []struct {
		name              string
		state             *ParentState
		expectAllowed     bool
		expectDrift       bool
		expectPhase       LifecyclePhase
		expectReasonMatch string
	}{
		{
			name:          "nil state - allowed",
			state:         nil,
			expectAllowed: true,
			expectDrift:   false,
		},
		{
			name: "deleting - allowed without drift check",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    5,
				HasObservedGeneration: true,
				DeletionTimestamp:     &metav1.Time{Time: time.Now()},
			},
			expectAllowed: true,
			expectDrift:   false,
			expectPhase:   PhaseDeleting,
		},
		{
			name: "initializing (no observedGeneration) - allowed",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            1,
				HasObservedGeneration: false,
			},
			expectAllowed: true,
			expectDrift:   false,
			expectPhase:   PhaseInitializing,
		},
		{
			name: "initializing (has Initialized condition) - allowed as ready",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            1,
				HasObservedGeneration: false,
				Conditions: []metav1.Condition{
					{Type: "Initialized", Status: metav1.ConditionTrue},
				},
			},
			expectAllowed: true,
			expectDrift:   false,
			expectPhase:   PhaseInitialized,
		},
		{
			name: "expected change - gen != obsGen",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    4,
				HasObservedGeneration: true,
			},
			expectAllowed:     true,
			expectDrift:       false,
			expectPhase:       PhaseInitialized,
			expectReasonMatch: "expected change",
		},
		{
			name: "drift detected - gen == obsGen",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    5,
				HasObservedGeneration: true,
			},
			expectAllowed:     true, // Phase 1: always allow
			expectDrift:       true,
			expectPhase:       PhaseInitialized,
			expectReasonMatch: "drift detected",
		},
		{
			name: "marked initialized via annotation - drift check applies",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    5,
				HasObservedGeneration: true,
				IsInitialized:         true,
			},
			expectAllowed: true,
			expectDrift:   true,
			expectPhase:   PhaseInitialized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detector.DetectFromState(tt.state)
			assert.Equal(t, tt.expectAllowed, result.Allowed, "Allowed")
			assert.Equal(t, tt.expectDrift, result.DriftDetected, "DriftDetected")
			if tt.state != nil {
				assert.Equal(t, tt.expectPhase, result.LifecyclePhase, "LifecyclePhase")
			}
			if tt.expectReasonMatch != "" {
				assert.Contains(t, result.Reason, tt.expectReasonMatch, "Reason")
			}
		})
	}
}

func TestDetectFromStateWithFieldManager(t *testing.T) {
	detector := &Detector{
		lifecycleDetector: NewLifecycleDetector(),
	}

	tests := []struct {
		name              string
		state             *ParentState
		fieldManager      string
		expectDrift       bool
		expectReasonMatch string
	}{
		{
			name: "controller request - gen != obsGen - no drift",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    4,
				HasObservedGeneration: true,
				ControllerManager:     "my-controller",
			},
			fieldManager:      "my-controller",
			expectDrift:       false,
			expectReasonMatch: "expected change",
		},
		{
			name: "controller request - gen == obsGen - drift",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    5,
				HasObservedGeneration: true,
				ControllerManager:     "my-controller",
			},
			fieldManager:      "my-controller",
			expectDrift:       true,
			expectReasonMatch: "drift detected: parent generation",
		},
		{
			name: "different actor - not drift (new causal origin)",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    4, // Parent is reconciling
				HasObservedGeneration: true,
				ControllerManager:     "my-controller",
			},
			fieldManager:      "other-actor",
			expectDrift:       false,
			expectReasonMatch: "different actor",
		},
		{
			name: "unknown controller - fallback assumes controller",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    4,
				HasObservedGeneration: true,
				ControllerManager:     "", // Unknown
			},
			fieldManager:      "any-manager",
			expectDrift:       false,
			expectReasonMatch: "expected change",
		},
		{
			name: "empty fieldManager with known controller - treated as controller",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    4,
				HasObservedGeneration: true,
				ControllerManager:     "my-controller",
			},
			fieldManager:      "",
			expectDrift:       false,
			expectReasonMatch: "expected change",
		},
		{
			name: "non-empty fieldManager different from controller - different actor",
			state: &ParentState{
				Ref:                   ParentRef{Kind: "Deployment", Name: "test"},
				Generation:            5,
				ObservedGeneration:    5, // Parent is stable
				HasObservedGeneration: true,
				ControllerManager:     "my-controller",
			},
			fieldManager:      "kubectl-edit",
			expectDrift:       false,
			expectReasonMatch: "different actor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detector.DetectFromStateWithFieldManager(tt.state, tt.fieldManager)
			assert.Equal(t, tt.expectDrift, result.DriftDetected, "DriftDetected (reason: %s)", result.Reason)
			if tt.expectReasonMatch != "" {
				assert.Contains(t, result.Reason, tt.expectReasonMatch, "Reason")
			}
		})
	}
}

func TestLifecycleDetector_DetectPhase(t *testing.T) {
	detector := NewLifecycleDetector()

	tests := []struct {
		name   string
		state  *ParentState
		expect LifecyclePhase
	}{
		{
			name:   "nil state - ready",
			state:  nil,
			expect: PhaseInitialized,
		},
		{
			name: "deletionTimestamp set - deleting",
			state: &ParentState{
				DeletionTimestamp: &metav1.Time{Time: time.Now()},
			},
			expect: PhaseDeleting,
		},
		{
			name: "deletion takes precedence over initialized",
			state: &ParentState{
				DeletionTimestamp:     &metav1.Time{Time: time.Now()},
				HasObservedGeneration: true,
				IsInitialized:         true,
			},
			expect: PhaseDeleting,
		},
		{
			name: "annotation initialized - ready",
			state: &ParentState{
				IsInitialized: true,
			},
			expect: PhaseInitialized,
		},
		{
			name: "Initialized condition true - ready",
			state: &ParentState{
				Conditions: []metav1.Condition{
					{Type: "Initialized", Status: metav1.ConditionTrue},
				},
			},
			expect: PhaseInitialized,
		},
		{
			name: "Ready condition true - ready",
			state: &ParentState{
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionTrue},
				},
			},
			expect: PhaseInitialized,
		},
		{
			name: "observedGeneration exists - ready",
			state: &ParentState{
				HasObservedGeneration: true,
				ObservedGeneration:    1,
			},
			expect: PhaseInitialized,
		},
		{
			name: "no initialization signals - initializing",
			state: &ParentState{
				Generation: 1,
			},
			expect: PhaseInitializing,
		},
		{
			name: "Ready=False does not count as initialized",
			state: &ParentState{
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionFalse},
				},
			},
			expect: PhaseInitializing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase := detector.DetectPhase(tt.state)
			assert.Equal(t, tt.expect, phase)
		})
	}
}

func TestIsControllerByHash(t *testing.T) {
	detector := &Detector{
		lifecycleDetector: NewLifecycleDetector(),
	}

	// Generate some user hashes
	user1 := "system:serviceaccount:kube-system:deployment-controller"
	user2 := "admin@example.com"
	user3 := "kubectl-user"

	hash1 := controller.HashUsername(user1)
	hash2 := controller.HashUsername(user2)
	hash3 := controller.HashUsername(user3)

	tests := []struct {
		name             string
		parentState      *ParentState
		username         string
		childUpdaters    []string
		wantController   bool
		wantCanDetermine bool
	}{
		{
			name: "CREATE - first updater is controller",
			parentState: &ParentState{
				Ref:         ParentRef{Kind: "Deployment", Name: "test"},
				Controllers: nil, // No controllers annotation yet
			},
			username:         user1,
			childUpdaters:    nil, // No updaters yet (CREATE)
			wantController:   true,
			wantCanDetermine: true,
		},
		{
			name: "single updater matches - controller",
			parentState: &ParentState{
				Ref:         ParentRef{Kind: "Deployment", Name: "test"},
				Controllers: []string{hash1},
			},
			username:         user1,
			childUpdaters:    []string{hash1},
			wantController:   true,
			wantCanDetermine: true,
		},
		{
			name: "single updater doesn't match - not controller",
			parentState: &ParentState{
				Ref:         ParentRef{Kind: "Deployment", Name: "test"},
				Controllers: []string{hash1},
			},
			username:         user2,
			childUpdaters:    []string{hash1},
			wantController:   false,
			wantCanDetermine: true,
		},
		{
			name: "multiple updaters with parent controllers - intersection match",
			parentState: &ParentState{
				Ref:         ParentRef{Kind: "Deployment", Name: "test"},
				Controllers: []string{hash1, hash2},
			},
			username:         user1,
			childUpdaters:    []string{hash1, hash3},
			wantController:   true,
			wantCanDetermine: true,
		},
		{
			name: "multiple updaters with parent controllers - not in intersection",
			parentState: &ParentState{
				Ref:         ParentRef{Kind: "Deployment", Name: "test"},
				Controllers: []string{hash1},
			},
			username:         user3,
			childUpdaters:    []string{hash1, hash3},
			wantController:   false,
			wantCanDetermine: true,
		},
		{
			name: "multiple updaters no parent controllers - can't determine",
			parentState: &ParentState{
				Ref:         ParentRef{Kind: "Deployment", Name: "test"},
				Controllers: nil,
			},
			username:         user1,
			childUpdaters:    []string{hash1, hash2},
			wantController:   false,
			wantCanDetermine: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isController, canDetermine := detector.isControllerByHash(tt.parentState, tt.username, tt.childUpdaters)
			assert.Equal(t, tt.wantController, isController, "isController")
			assert.Equal(t, tt.wantCanDetermine, canDetermine, "canDetermine")
		})
	}
}

func TestParentRef_String(t *testing.T) {
	tests := []struct {
		name   string
		ref    ParentRef
		expect string
	}{
		{
			name: "cluster-scoped",
			ref: ParentRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "test",
			},
			expect: "apps/v1/Deployment:test",
		},
		{
			name: "namespaced",
			ref: ParentRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Namespace:  "default",
				Name:       "test",
			},
			expect: "apps/v1/Deployment:default/test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, tt.ref.String())
		})
	}
}
