package v1alpha1

import (
	"github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/apis/example"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: example.GroupName, Version: "v1alpha1"}

// SchemeGroupVersion is an alias for GroupVersion (used by code generators).
var SchemeGroupVersion = GroupVersion

// Resource returns a GroupResource for the given resource.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}

var (
	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes adds the types in this group-version to the scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&Widget{},
		&WidgetList{},
		&WidgetSet{},
		&WidgetSetList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
