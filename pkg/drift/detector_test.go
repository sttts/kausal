package drift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kausality-io/kausality/pkg/controller"
)

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
			name: "observedGeneration with Ready=True - ready",
			state: &ParentState{
				HasObservedGeneration: true,
				ObservedGeneration:    1,
				Conditions: []metav1.Condition{
					{Type: "Ready", Status: metav1.ConditionTrue},
				},
			},
			expect: PhaseInitialized,
		},
		{
			name: "observedGeneration with Available=True - ready",
			state: &ParentState{
				HasObservedGeneration: true,
				ObservedGeneration:    1,
				Conditions: []metav1.Condition{
					{Type: "Available", Status: metav1.ConditionTrue},
				},
			},
			expect: PhaseInitialized,
		},
		{
			name: "observedGeneration with Initialized=True condition - ready",
			state: &ParentState{
				HasObservedGeneration: true,
				ObservedGeneration:    1,
				Conditions: []metav1.Condition{
					{Type: "Initialized", Status: metav1.ConditionTrue},
				},
			},
			expect: PhaseInitialized,
		},
		{
			name: "observedGeneration with Synced=True but Ready=False - initializing",
			state: &ParentState{
				HasObservedGeneration: true,
				ObservedGeneration:    1,
				Conditions: []metav1.Condition{
					{Type: "Synced", Status: metav1.ConditionTrue},
					{Type: "Ready", Status: metav1.ConditionFalse},
				},
			},
			expect: PhaseInitializing,
		},
		{
			name: "observedGeneration without healthy condition - initializing",
			state: &ParentState{
				HasObservedGeneration: true,
				ObservedGeneration:    1,
				// No Synced=True or Ready=True condition
			},
			expect: PhaseInitializing,
		},
		{
			name: "observedGeneration with Synced=False - initializing",
			state: &ParentState{
				HasObservedGeneration: true,
				ObservedGeneration:    1,
				Conditions: []metav1.Condition{
					{Type: "Synced", Status: metav1.ConditionFalse},
				},
			},
			expect: PhaseInitializing,
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
			isController, canDetermine := IsControllerByHash(tt.parentState, tt.username, tt.childUpdaters)
			assert.Equal(t, tt.wantController, isController, "isController")
			assert.Equal(t, tt.wantCanDetermine, canDetermine, "canDetermine")
		})
	}
}

func TestCheckGeneration(t *testing.T) {
	tests := []struct {
		name          string
		generation    int64
		obsGeneration int64
		wantDrift     bool
		wantAllowed   bool
	}{
		{
			name:          "gen != obsGen - expected change, no drift",
			generation:    5,
			obsGeneration: 4,
			wantDrift:     false,
			wantAllowed:   true,
		},
		{
			name:          "gen == obsGen - drift detected",
			generation:    5,
			obsGeneration: 5,
			wantDrift:     true,
			wantAllowed:   true, // Phase 1: logging only
		},
		{
			name:          "obsGen ahead of gen (edge case) - no drift",
			generation:    3,
			obsGeneration: 5,
			wantDrift:     false,
			wantAllowed:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parentState := &ParentState{
				Generation:         tt.generation,
				ObservedGeneration: tt.obsGeneration,
			}
			result := &DriftResult{
				ParentState: parentState,
			}

			got := checkGeneration(result, parentState)
			assert.Equal(t, tt.wantDrift, got.DriftDetected, "DriftDetected")
			assert.Equal(t, tt.wantAllowed, got.Allowed, "Allowed")
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
