package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
)

func TestRuleMatches(t *testing.T) {
	s := &Store{}

	tests := []struct {
		name string
		rule kausalityv1alpha1.ResourceRule
		gvr  schema.GroupVersionResource
		want bool
	}{
		{
			name: "exact match",
			rule: kausalityv1alpha1.ResourceRule{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
			},
			gvr:  schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			want: true,
		},
		{
			name: "wildcard resource",
			rule: kausalityv1alpha1.ResourceRule{
				APIGroups: []string{"apps"},
				Resources: []string{"*"},
			},
			gvr:  schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"},
			want: true,
		},
		{
			name: "wrong group",
			rule: kausalityv1alpha1.ResourceRule{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
			},
			gvr:  schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "deployments"},
			want: false,
		},
		{
			name: "wrong resource",
			rule: kausalityv1alpha1.ResourceRule{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
			},
			gvr:  schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"},
			want: false,
		},
		{
			name: "excluded resource",
			rule: kausalityv1alpha1.ResourceRule{
				APIGroups: []string{"apps"},
				Resources: []string{"*"},
				Excluded:  []string{"replicasets"},
			},
			gvr:  schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"},
			want: false,
		},
		{
			name: "core group",
			rule: kausalityv1alpha1.ResourceRule{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
			},
			gvr:  schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.ruleMatches(tt.rule, tt.gvr)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNamespacesMatch(t *testing.T) {
	s := &Store{}

	tests := []struct {
		name      string
		selector  *kausalityv1alpha1.NamespaceSelector
		namespace string
		nsLabels  map[string]string
		want      bool
	}{
		{
			name:      "nil selector matches all",
			selector:  nil,
			namespace: "production",
			want:      true,
		},
		{
			name: "explicit name match",
			selector: &kausalityv1alpha1.NamespaceSelector{
				Names: []string{"production", "staging"},
			},
			namespace: "production",
			want:      true,
		},
		{
			name: "explicit name no match",
			selector: &kausalityv1alpha1.NamespaceSelector{
				Names: []string{"production", "staging"},
			},
			namespace: "development",
			want:      false,
		},
		{
			name: "excluded namespace",
			selector: &kausalityv1alpha1.NamespaceSelector{
				Excluded: []string{"kube-system"},
			},
			namespace: "kube-system",
			want:      false,
		},
		{
			name: "excluded takes precedence over names",
			selector: &kausalityv1alpha1.NamespaceSelector{
				Names:    []string{"production", "kube-system"},
				Excluded: []string{"kube-system"},
			},
			namespace: "kube-system",
			want:      false,
		},
		{
			name: "label selector match",
			selector: &kausalityv1alpha1.NamespaceSelector{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"env": "production"},
				},
			},
			namespace: "my-namespace",
			nsLabels:  map[string]string{"env": "production"},
			want:      true,
		},
		{
			name: "label selector no match",
			selector: &kausalityv1alpha1.NamespaceSelector{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"env": "production"},
				},
			},
			namespace: "my-namespace",
			nsLabels:  map[string]string{"env": "staging"},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.namespacesMatch(tt.selector, tt.namespace, tt.nsLabels)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCalculateSpecificity(t *testing.T) {
	s := &Store{}
	ctx := ResourceContext{
		GVR:       schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Namespace: "production",
	}

	tests := []struct {
		name   string
		policy kausalityv1alpha1.Kausality
		want   int
	}{
		{
			name: "wildcard everything",
			policy: kausalityv1alpha1.Kausality{
				Spec: kausalityv1alpha1.KausalitySpec{
					Resources: []kausalityv1alpha1.ResourceRule{
						{APIGroups: []string{"apps"}, Resources: []string{"*"}},
					},
				},
			},
			want: 0,
		},
		{
			name: "explicit resource",
			policy: kausalityv1alpha1.Kausality{
				Spec: kausalityv1alpha1.KausalitySpec{
					Resources: []kausalityv1alpha1.ResourceRule{
						{APIGroups: []string{"apps"}, Resources: []string{"deployments"}},
					},
				},
			},
			want: 10,
		},
		{
			name: "explicit namespace",
			policy: kausalityv1alpha1.Kausality{
				Spec: kausalityv1alpha1.KausalitySpec{
					Resources: []kausalityv1alpha1.ResourceRule{
						{APIGroups: []string{"apps"}, Resources: []string{"*"}},
					},
					Namespaces: &kausalityv1alpha1.NamespaceSelector{
						Names: []string{"production"},
					},
				},
			},
			want: 100,
		},
		{
			name: "namespace selector",
			policy: kausalityv1alpha1.Kausality{
				Spec: kausalityv1alpha1.KausalitySpec{
					Resources: []kausalityv1alpha1.ResourceRule{
						{APIGroups: []string{"apps"}, Resources: []string{"*"}},
					},
					Namespaces: &kausalityv1alpha1.NamespaceSelector{
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"env": "production"},
						},
					},
				},
			},
			want: 50,
		},
		{
			name: "explicit resource + namespace",
			policy: kausalityv1alpha1.Kausality{
				Spec: kausalityv1alpha1.KausalitySpec{
					Resources: []kausalityv1alpha1.ResourceRule{
						{APIGroups: []string{"apps"}, Resources: []string{"deployments"}},
					},
					Namespaces: &kausalityv1alpha1.NamespaceSelector{
						Names: []string{"production"},
					},
				},
			},
			want: 110,
		},
		{
			name: "with object selector",
			policy: kausalityv1alpha1.Kausality{
				Spec: kausalityv1alpha1.KausalitySpec{
					Resources: []kausalityv1alpha1.ResourceRule{
						{APIGroups: []string{"apps"}, Resources: []string{"*"}},
					},
					ObjectSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"protected": "true"},
					},
				},
			},
			want: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.calculateSpecificity(&tt.policy, ctx)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOverrideMatches(t *testing.T) {
	s := &Store{}

	tests := []struct {
		name     string
		override kausalityv1alpha1.ModeOverride
		ctx      ResourceContext
		want     bool
	}{
		{
			name:     "empty override matches all",
			override: kausalityv1alpha1.ModeOverride{Mode: kausalityv1alpha1.ModeEnforce},
			ctx: ResourceContext{
				GVR:       schema.GroupVersionResource{Group: "apps", Resource: "deployments"},
				Namespace: "production",
			},
			want: true,
		},
		{
			name: "namespace match",
			override: kausalityv1alpha1.ModeOverride{
				Namespaces: []string{"production"},
				Mode:       kausalityv1alpha1.ModeEnforce,
			},
			ctx: ResourceContext{
				GVR:       schema.GroupVersionResource{Group: "apps", Resource: "deployments"},
				Namespace: "production",
			},
			want: true,
		},
		{
			name: "namespace no match",
			override: kausalityv1alpha1.ModeOverride{
				Namespaces: []string{"production"},
				Mode:       kausalityv1alpha1.ModeEnforce,
			},
			ctx: ResourceContext{
				GVR:       schema.GroupVersionResource{Group: "apps", Resource: "deployments"},
				Namespace: "staging",
			},
			want: false,
		},
		{
			name: "resource match",
			override: kausalityv1alpha1.ModeOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"statefulsets"},
				Mode:      kausalityv1alpha1.ModeEnforce,
			},
			ctx: ResourceContext{
				GVR:       schema.GroupVersionResource{Group: "apps", Resource: "statefulsets"},
				Namespace: "default",
			},
			want: true,
		},
		{
			name: "resource no match",
			override: kausalityv1alpha1.ModeOverride{
				APIGroups: []string{"apps"},
				Resources: []string{"statefulsets"},
				Mode:      kausalityv1alpha1.ModeEnforce,
			},
			ctx: ResourceContext{
				GVR:       schema.GroupVersionResource{Group: "apps", Resource: "deployments"},
				Namespace: "default",
			},
			want: false,
		},
		{
			name: "combined namespace + resource match",
			override: kausalityv1alpha1.ModeOverride{
				APIGroups:  []string{"apps"},
				Resources:  []string{"deployments"},
				Namespaces: []string{"production"},
				Mode:       kausalityv1alpha1.ModeEnforce,
			},
			ctx: ResourceContext{
				GVR:       schema.GroupVersionResource{Group: "apps", Resource: "deployments"},
				Namespace: "production",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.overrideMatches(tt.override, tt.ctx)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveMode_AnnotationPrecedence(t *testing.T) {
	s := &Store{}
	ctx := ResourceContext{
		GVR:       schema.GroupVersionResource{Group: "apps", Resource: "deployments"},
		Namespace: "default",
	}

	// Object annotation takes precedence
	mode := s.ResolveMode(ctx, map[string]string{ModeAnnotation: "enforce"}, nil)
	assert.Equal(t, kausalityv1alpha1.ModeEnforce, mode)

	// Namespace annotation second
	mode = s.ResolveMode(ctx, nil, map[string]string{ModeAnnotation: "enforce"})
	assert.Equal(t, kausalityv1alpha1.ModeEnforce, mode)

	// Object annotation wins over namespace
	mode = s.ResolveMode(ctx, map[string]string{ModeAnnotation: "log"}, map[string]string{ModeAnnotation: "enforce"})
	assert.Equal(t, kausalityv1alpha1.ModeLog, mode)

	// No annotations, no policies = default log
	mode = s.ResolveMode(ctx, nil, nil)
	assert.Equal(t, kausalityv1alpha1.ModeLog, mode)
}
