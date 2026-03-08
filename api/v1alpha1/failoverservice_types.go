/*
Copyright 2026 Signal24.

Licensed under the MIT License.
See LICENSE file in the project root for full license text.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FailoverAction defines a single action to execute during a transition.
// Exactly one of http, job, scale, waitReady, or waitRemote must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.http) ? 1 : 0) + (has(self.job) ? 1 : 0) + (has(self.scale) ? 1 : 0) + (has(self.waitReady) ? 1 : 0) + (has(self.waitRemote) ? 1 : 0) == 1",message="exactly one of http, job, scale, waitReady, or waitRemote must be set"
type FailoverAction struct {
	// name is a human-readable identifier for this action.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// http defines an HTTP request action.
	// +optional
	HTTP *HTTPAction `json:"http,omitempty"`

	// job defines a Kubernetes Job action.
	// +optional
	Job *JobAction `json:"job,omitempty"`

	// scale defines a scale action for a Deployment or StatefulSet.
	// +optional
	Scale *ScaleAction `json:"scale,omitempty"`

	// waitReady waits for a Deployment or StatefulSet to become fully ready.
	// +optional
	WaitReady *WaitReadyAction `json:"waitReady,omitempty"`

	// waitRemote waits for the remote cluster to reach a specific phase or complete a named action.
	// +optional
	WaitRemote *WaitRemoteAction `json:"waitRemote,omitempty"`

	// ignoreFailure allows the pipeline to continue even if this action fails.
	// +optional
	IgnoreFailure bool `json:"ignoreFailure,omitempty"`

	// maxRetries overrides the default number of transient-error retries for this action. Defaults to 5.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	MaxRetries *int32 `json:"maxRetries,omitempty"`

	// retryIntervalSeconds overrides the default retry/poll interval for this action. Defaults to 5.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	RetryIntervalSeconds *int64 `json:"retryIntervalSeconds,omitempty"`
}

// HTTPAction defines an HTTP request to execute.
type HTTPAction struct {
	// url is the target URL.
	// +required
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// method is the HTTP method. Defaults to GET.
	// +optional
	// +kubebuilder:default=GET
	Method string `json:"method,omitempty"`

	// headers are additional HTTP headers to include.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// body is the request body.
	// +optional
	Body *string `json:"body,omitempty"`

	// timeoutSeconds is the HTTP request timeout. Defaults to 30.
	// +optional
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
}

// JobAction defines a Kubernetes Job to run.
type JobAction struct {
	// image is the container image to use.
	// +required
	Image string `json:"image"`

	// command overrides the container entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// script wraps the provided script in /bin/sh -c.
	// +optional
	Script *string `json:"script,omitempty"`

	// env defines environment variables for the Job container.
	// +optional
	Env []EnvVar `json:"env,omitempty"`

	// serviceAccountName is the service account for the Job pod.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// activeDeadlineSeconds is the Job timeout. Defaults to 300.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// maxRetries maps to Job backoffLimit. Defaults to 0 (no retry).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxRetries *int32 `json:"maxRetries,omitempty"`
}

// ScaleAction defines a scale operation on a Deployment or StatefulSet.
type ScaleAction struct {
	// kind is the resource kind: "Deployment" or "StatefulSet".
	// +required
	// +kubebuilder:validation:Enum=Deployment;StatefulSet
	Kind string `json:"kind"`

	// name is the resource name.
	// +required
	Name string `json:"name"`

	// replicas is the desired replica count.
	// +required
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// waitReady causes the action to poll until the resource is fully ready after scaling.
	// +optional
	WaitReady bool `json:"waitReady,omitempty"`

	// timeoutSeconds is how long to wait for readiness when waitReady is true. Defaults to 300.
	// +optional
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
}

// WaitReadyAction waits for a Deployment or StatefulSet to have all replicas ready.
type WaitReadyAction struct {
	// kind is the resource kind: "Deployment" or "StatefulSet".
	// +required
	// +kubebuilder:validation:Enum=Deployment;StatefulSet
	Kind string `json:"kind"`

	// name is the resource name.
	// +required
	Name string `json:"name"`

	// timeoutSeconds is how long to wait before failing. Defaults to 300.
	// +optional
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
}

// WaitRemoteAction waits for the remote cluster to reach a specific phase or complete a named action.
// Exactly one of phase or actionName must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.phase) ? 1 : 0) + (has(self.actionName) ? 1 : 0) == 1",message="exactly one of phase or actionName must be set"
type WaitRemoteAction struct {
	// phase waits for the remote cluster's transition to reach or pass this phase.
	// An empty string means idle (no transition in progress).
	// +optional
	Phase *FailoverPhase `json:"phase,omitempty"`

	// actionName waits for a named action on the remote cluster to complete.
	// +optional
	ActionName *string `json:"actionName,omitempty"`

	// connectTimeoutSeconds is the timeout for connecting to the remote cluster. Defaults to 10.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ConnectTimeoutSeconds *int64 `json:"connectTimeoutSeconds,omitempty"`

	// actionTimeoutSeconds is the timeout for waiting for the condition. Defaults to 300.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ActionTimeoutSeconds *int64 `json:"actionTimeoutSeconds,omitempty"`
}

// EnvVar defines an environment variable for a Job container.
type EnvVar struct {
	// name is the environment variable name.
	// +required
	Name string `json:"name"`

	// value is the environment variable value.
	// +required
	Value string `json:"value"`
}

// TransitionActions defines pre/post actions for a specific transition event.
// Action names should be unique across preActions and postActions when they are
// referenced by waitRemote.actionName.
type TransitionActions struct {
	// preActions are executed before the Service update.
	// +optional
	PreActions []FailoverAction `json:"preActions,omitempty"`

	// postActions are executed after connection flushing.
	// +optional
	PostActions []FailoverAction `json:"postActions,omitempty"`
}

// ClusterTarget defines a target address and transition hooks for a cluster.
type ClusterTarget struct {
	// primaryModeAddress is the address the local Service gets set to in primary mode.
	// +required
	// +kubebuilder:validation:MinLength=1
	PrimaryModeAddress string `json:"primaryModeAddress"`

	// failoverModeAddress is the address the local Service gets set to in failover mode.
	// +required
	// +kubebuilder:validation:MinLength=1
	FailoverModeAddress string `json:"failoverModeAddress"`

	// onFailover defines actions to run when transitioning to failover.
	// +optional
	OnFailover *TransitionActions `json:"onFailover,omitempty"`

	// onFailback defines actions to run when transitioning back to primary.
	// +optional
	OnFailback *TransitionActions `json:"onFailback,omitempty"`
}

// Port defines a port mapping for the failover service.
type Port struct {
	// name is the name of the port (must be an IANA service name).
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Name string `json:"name"`

	// port is the port number.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// protocol is the protocol for the port. Defaults to TCP.
	// +optional
	// +kubebuilder:default=TCP
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	Protocol string `json:"protocol,omitempty"`
}

// RemoteState holds the last known state of the remote cluster's FailoverService.
type RemoteState struct {
	// failoverActive indicates whether failover is active on the remote cluster.
	// +optional
	FailoverActive bool `json:"failoverActive,omitempty"`

	// activeCluster is the cluster ID that last completed a transition on the remote.
	// +optional
	ActiveCluster string `json:"activeCluster,omitempty"`

	// activeGeneration is the remote cluster's system-managed generation counter.
	// +optional
	ActiveGeneration int64 `json:"activeGeneration,omitempty"`

	// lastSyncTime is the last time the remote state was synced.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// clusterID is the unique identifier of the remote cluster.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// specChangeTime is the timestamp when the remote cluster's spec.failoverActive last changed.
	// +optional
	SpecChangeTime *metav1.Time `json:"specChangeTime,omitempty"`
}

// FailoverPhase represents the current phase of a transition.
// +kubebuilder:validation:Enum="";ExecutingPreActions;UpdatingResources;FlushingConnections;ExecutingPostActions
type FailoverPhase string

const (
	PhaseIdle                 FailoverPhase = ""
	PhaseExecutingPreActions  FailoverPhase = "ExecutingPreActions"
	PhaseUpdatingResources    FailoverPhase = "UpdatingResources"
	PhaseFlushingConnections  FailoverPhase = "FlushingConnections"
	PhaseExecutingPostActions FailoverPhase = "ExecutingPostActions"
)

// PhaseOrder returns a numeric ordering for phases (higher = further along).
func PhaseOrder(p FailoverPhase) int {
	switch p {
	case PhaseIdle:
		return 0
	case PhaseExecutingPreActions:
		return 1
	case PhaseUpdatingResources:
		return 2
	case PhaseFlushingConnections:
		return 3
	case PhaseExecutingPostActions:
		return 4
	default:
		return -1
	}
}

// TransitionStatus tracks the progress of a failover transition.
type TransitionStatus struct {
	// phase is the current phase of the transition.
	// +optional
	Phase FailoverPhase `json:"phase,omitempty"`

	// targetDirection is either "failover" or "failback".
	// +optional
	TargetDirection string `json:"targetDirection,omitempty"`

	// oldAddressSnapshot is the address that was active when this transition started.
	// Used by connection flushing to avoid spec-edit races mid-transition.
	// +optional
	OldAddressSnapshot string `json:"oldAddressSnapshot,omitempty"`

	// newAddressSnapshot is the address selected for this transition at start time.
	// Used for service updates to avoid spec-edit races mid-transition.
	// +optional
	NewAddressSnapshot string `json:"newAddressSnapshot,omitempty"`

	// transitionID is a unique identifier for this transition, used to prevent
	// Job name collisions across transitions. Derived from the transition start time.
	// +optional
	TransitionID string `json:"transitionID,omitempty"`

	// currentActionIndex is the index of the current action being executed.
	// +optional
	CurrentActionIndex int `json:"currentActionIndex"`

	// preActionResults tracks the status of each pre-action.
	// +optional
	PreActionResults []ActionResult `json:"preActionResults,omitempty"`

	// postActionResults tracks the status of each post-action.
	// +optional
	PostActionResults []ActionResult `json:"postActionResults,omitempty"`

	// startedAt is when the transition began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// connectionKill tracks connection kill progress during the FlushingConnections phase.
	// +optional
	ConnectionKill *ConnectionKillStatus `json:"connectionKill,omitempty"`

	// eventName links this transition to its FailoverEvent resource.
	// +optional
	EventName string `json:"eventName,omitempty"`
}

// ConnectionKillStatus tracks the progress of connection killing during a transition.
type ConnectionKillStatus struct {
	// signalSent indicates whether the kill signal has been sent.
	// +optional
	SignalSent bool `json:"signalSent,omitempty"`

	// killToken is the unique token for this kill operation, used to match ACKs.
	// +optional
	KillToken string `json:"killToken,omitempty"`

	// expectedNodes is the list of node names that should ACK the kill.
	// +optional
	ExpectedNodes []string `json:"expectedNodes,omitempty"`

	// ackedNodes is the list of nodes that have successfully ACKed.
	// +optional
	AckedNodes []string `json:"ackedNodes,omitempty"`

	// signaledAt is when the kill signal was sent.
	// +optional
	SignaledAt *metav1.Time `json:"signaledAt,omitempty"`
}

// ActionResult tracks the status of a single action execution.
type ActionResult struct {
	// name is the action name.
	// +required
	Name string `json:"name"`

	// status is one of: Pending, Running, Succeeded, Failed, Skipped.
	// +required
	Status string `json:"status"`

	// message contains additional details.
	// +optional
	Message string `json:"message,omitempty"`

	// startedAt is when this action started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// completedAt is when this action finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// jobName is the name of the Job created for job-type actions.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// retryCount tracks how many transient errors have occurred for this action.
	// +optional
	RetryCount int `json:"retryCount,omitempty"`
}

const (
	ActionStatusPending   = "Pending"
	ActionStatusRunning   = "Running"
	ActionStatusSucceeded = "Succeeded"
	ActionStatusFailed    = "Failed"
	ActionStatusSkipped   = "Skipped"
)

// FailoverServiceSpec defines the desired state of FailoverService.
type FailoverServiceSpec struct {
	// serviceName is the name of the Kubernetes Service to create/manage.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceName is immutable"
	ServiceName string `json:"serviceName"`

	// ports defines the ports for the managed Service.
	// +required
	// +kubebuilder:validation:MinItems=1
	Ports []Port `json:"ports"`

	// failoverActive indicates whether the failover target should be active.
	// When false, traffic routes to the primary target; when true, to the failover target.
	// +optional
	FailoverActive bool `json:"failoverActive,omitempty"`

	// primaryCluster defines the primary cluster target.
	// +required
	PrimaryCluster ClusterTarget `json:"primaryCluster"`

	// failoverCluster defines the failover cluster target.
	// +required
	FailoverCluster ClusterTarget `json:"failoverCluster"`
}

// ClusterStatus tracks the observed state of a single cluster.
type ClusterStatus struct {
	// clusterID is the unique identifier for this cluster.
	// +required
	ClusterID string `json:"clusterID"`

	// appliedGeneration is the activeGeneration that this cluster has fully applied.
	// +optional
	AppliedGeneration int64 `json:"appliedGeneration,omitempty"`

	// lastAppliedTime is when this cluster last completed a transition.
	// +optional
	LastAppliedTime *metav1.Time `json:"lastAppliedTime,omitempty"`

	// phase is the current phase of this cluster's transition (empty when idle).
	// +optional
	Phase FailoverPhase `json:"phase,omitempty"`
}

// FailoverServiceStatus defines the observed state of FailoverService.
type FailoverServiceStatus struct {
	// observedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// activeCluster is the cluster ID that last completed a transition.
	// System-managed; used for conflict resolution.
	// +optional
	ActiveCluster string `json:"activeCluster,omitempty"`

	// activeGeneration is a monotonic counter incremented on each completed transition.
	// System-managed; used for conflict resolution.
	// +optional
	ActiveGeneration int64 `json:"activeGeneration,omitempty"`

	// lastTransitionTime is the last time the failover state transitioned.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// lastKnownFailoverActive is the last known value of spec.failoverActive,
	// used to detect transitions.
	// +optional
	LastKnownFailoverActive *bool `json:"lastKnownFailoverActive,omitempty"`

	// specChangeTime records when spec.failoverActive last changed value.
	// Used for timestamp-based conflict resolution tiebreaking.
	// +optional
	SpecChangeTime *metav1.Time `json:"specChangeTime,omitempty"`

	// clusters tracks per-cluster observed state for observability.
	// +listType=map
	// +listMapKey=clusterID
	// +optional
	Clusters []ClusterStatus `json:"clusters,omitempty"`

	// remoteState holds the last known state from the remote cluster.
	// +optional
	RemoteState *RemoteState `json:"remoteState,omitempty"`

	// transition tracks the progress of an in-flight transition.
	// +optional
	Transition *TransitionStatus `json:"transition,omitempty"`

	// conditions represent the current state of the FailoverService resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	// ConditionServiceReady indicates whether the managed Service is correctly configured.
	ConditionServiceReady = "ServiceReady"

	// ConditionRemoteSynced indicates whether the remote cluster is synced.
	ConditionRemoteSynced = "RemoteSynced"

	// ConditionConnectionsKilled indicates whether stale connections were killed during transition.
	ConditionConnectionsKilled = "ConnectionsKilled"

	// ConditionTransitionInProgress indicates a transition is currently in progress.
	ConditionTransitionInProgress = "TransitionInProgress"

	// ConditionActionFailed indicates an action has failed and the pipeline is halted.
	ConditionActionFailed = "ActionFailed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceName`
// +kubebuilder:printcolumn:name="Failover Active",type=boolean,JSONPath=`.spec.failoverActive`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.transition.phase`
// +kubebuilder:printcolumn:name="Active Cluster",type=string,JSONPath=`.status.activeCluster`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.activeGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FailoverService is the Schema for the failoverservices API.
type FailoverService struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of FailoverService
	// +required
	Spec FailoverServiceSpec `json:"spec"`

	// status defines the observed state of FailoverService
	// +optional
	Status FailoverServiceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FailoverServiceList contains a list of FailoverService.
type FailoverServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FailoverService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FailoverService{}, &FailoverServiceList{})
}
