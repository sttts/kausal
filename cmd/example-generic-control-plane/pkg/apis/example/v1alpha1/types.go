package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Widget is a simple namespaced resource for demonstrating kausality.
type Widget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WidgetSpec `json:"spec,omitempty"`
}

// WidgetSpec defines the desired state of a Widget.
type WidgetSpec struct {
	// Color is the widget's color.
	Color string `json:"color,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// WidgetList contains a list of Widgets.
type WidgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Widget `json:"items"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// WidgetSet manages a set of Widgets (parent resource).
type WidgetSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WidgetSetSpec   `json:"spec,omitempty"`
	Status WidgetSetStatus `json:"status,omitempty"`
}

// WidgetSetSpec defines the desired state of a WidgetSet.
type WidgetSetSpec struct {
	// Replicas is the number of Widgets to maintain.
	Replicas int32 `json:"replicas,omitempty"`

	// Template defines the Widget template.
	Template WidgetSpec `json:"template,omitempty"`
}

// WidgetSetStatus defines the observed state of a WidgetSet.
type WidgetSetStatus struct {
	// ObservedGeneration is the generation observed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReadyWidgets is the number of ready Widgets.
	ReadyWidgets int32 `json:"readyWidgets,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// WidgetSetList contains a list of WidgetSets.
type WidgetSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WidgetSet `json:"items"`
}
