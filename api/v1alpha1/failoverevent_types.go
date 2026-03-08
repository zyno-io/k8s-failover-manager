/*
Copyright 2026 Signal24.

Licensed under the MIT License.
See LICENSE file in the project root for full license text.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FailoverEventSpec defines the specification for a FailoverEvent.
type FailoverEventSpec struct {
	// failoverServiceRef is the name of the FailoverService this event belongs to.
	// +required
	FailoverServiceRef string `json:"failoverServiceRef"`

	// direction is the transition direction: "failover" or "failback".
	// +required
	// +kubebuilder:validation:Enum=failover;failback
	Direction string `json:"direction"`

	// triggeredBy indicates what triggered this transition: "manual" or "remote-sync".
	// +required
	// +kubebuilder:validation:Enum=manual;remote-sync
	TriggeredBy string `json:"triggeredBy"`
}

const (
	// EventOutcomeSucceeded indicates the transition completed successfully.
	EventOutcomeSucceeded = "Succeeded"
	// EventOutcomeFailed indicates the transition failed.
	EventOutcomeFailed = "Failed"
	// EventOutcomeForceAdvanced indicates the transition was force-advanced.
	EventOutcomeForceAdvanced = "ForceAdvanced"
	// EventOutcomeInProgress indicates the transition is still in progress.
	EventOutcomeInProgress = "InProgress"
)

// FailoverEventStatus defines the observed state of a FailoverEvent.
type FailoverEventStatus struct {
	// startedAt is when the transition began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// completedAt is when the transition finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// outcome is the result of the transition: Succeeded, Failed, ForceAdvanced, or InProgress.
	// +optional
	Outcome string `json:"outcome,omitempty"`

	// preActionResults tracks the status of each pre-action.
	// +optional
	PreActionResults []ActionResult `json:"preActionResults,omitempty"`

	// postActionResults tracks the status of each post-action.
	// +optional
	PostActionResults []ActionResult `json:"postActionResults,omitempty"`

	// connectionKillResult tracks connection kill outcome.
	// +optional
	ConnectionKillResult *ConnectionKillStatus `json:"connectionKillResult,omitempty"`

	// activeGenerationBefore is the activeGeneration before the transition.
	// +optional
	ActiveGenerationBefore int64 `json:"activeGenerationBefore,omitempty"`

	// activeGenerationAfter is the activeGeneration after the transition.
	// +optional
	ActiveGenerationAfter int64 `json:"activeGenerationAfter,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.failoverServiceRef`
// +kubebuilder:printcolumn:name="Direction",type=string,JSONPath=`.spec.direction`
// +kubebuilder:printcolumn:name="Outcome",type=string,JSONPath=`.status.outcome`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FailoverEvent records a transition event for a FailoverService.
type FailoverEvent struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the event specification
	// +required
	Spec FailoverEventSpec `json:"spec"`

	// status defines the observed state of the event
	// +optional
	Status FailoverEventStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FailoverEventList contains a list of FailoverEvent.
type FailoverEventList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FailoverEvent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FailoverEvent{}, &FailoverEventList{})
}
