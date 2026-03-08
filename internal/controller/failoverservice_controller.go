/*
Copyright 2026 Signal24.

Licensed under the MIT License.
See LICENSE file in the project root for full license text.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
	"github.com/zyno-io/k8s-failover-manager/internal/action"
	"github.com/zyno-io/k8s-failover-manager/internal/connkill"
	"github.com/zyno-io/k8s-failover-manager/internal/metrics"
	"github.com/zyno-io/k8s-failover-manager/internal/remotecluster"
	svcmanager "github.com/zyno-io/k8s-failover-manager/internal/service"
)

const (
	finalizerName          = "k8s-failover.zyno.io/finalizer"
	forceAdvanceAnnotation = "k8s-failover.zyno.io/force-advance"
	antiEntropyInterval    = 60 * time.Second
	defaultBackoffMin      = 5 * time.Second
	defaultRequeueInterval = 5 * time.Second
	jobRequeueInterval     = 30 * time.Second
	defaultMaxRetries      = 5
	connKillPollInterval   = 1 * time.Second
	connKillTimeout        = 60 * time.Second

	directionFailover = "failover"
	directionFailback = "failback"
	rolePrimary       = "primary"
	roleFailover      = "failover"

	triggerManual     = "manual"
	triggerRemoteSync = "remote-sync"
)

// FailoverServiceReconciler reconciles a FailoverService object.
type FailoverServiceReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	ClusterRole    string
	ClusterID      string
	ServiceManager *svcmanager.Manager
	Syncer         RemoteSyncer
	ConnSignaler   *connkill.Signaler
	ActionExecutor *action.MultiExecutor
	RemoteWatcher  *remotecluster.RemoteWatcher
}

// RemoteSyncer is implemented by remotecluster.Syncer and test stubs.
type RemoteSyncer interface {
	Sync(ctx context.Context, local *failoverv1alpha1.FailoverService) (*remotecluster.SyncResult, error)
}

// +kubebuilder:rbac:groups=k8s-failover.zyno.io,resources=failoverservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=k8s-failover.zyno.io,resources=failoverservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k8s-failover.zyno.io,resources=failoverservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=k8s-failover.zyno.io,resources=failoverevents,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=k8s-failover.zyno.io,resources=failoverevents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments/scale;statefulsets/scale,verbs=get;update;patch
func (r *FailoverServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Phase 1: Fetch & Validate
	fs := &failoverv1alpha1.FailoverService{}
	if err := r.Get(ctx, req.NamespacedName, fs); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !fs.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(fs, finalizerName) {
			// Best effort: clean stale connection-kill keys scoped to this FailoverService.
			if r.ConnSignaler != nil {
				if err := r.ConnSignaler.CleanupSignals(ctx, fs.Namespace, fs.Name); err != nil {
					log.Error(err, "Failed to cleanup connection-kill signals during deletion")
				}
			}
			controllerutil.RemoveFinalizer(fs, finalizerName)
			if err := r.Update(ctx, fs); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(fs, finalizerName) {
		controllerutil.AddFinalizer(fs, finalizerName)
		if err := r.Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Ensure cluster ID annotation is persisted
	if fs.Annotations == nil || fs.Annotations["k8s-failover.zyno.io/cluster-id"] != r.ClusterID {
		remotecluster.SetClusterIDAnnotation(fs, r.ClusterID)
		if err := r.Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check force-advance annotation.
	// Apply status change first; only remove annotation after status is persisted.
	// This ensures force-advance is retried on transient status update failures.
	if fs.Annotations != nil && fs.Annotations[forceAdvanceAnnotation] != "" {
		forceAdvanceValue := fs.Annotations[forceAdvanceAnnotation]
		if r.handleForceAdvance(ctx, fs, forceAdvanceValue) {
			if err := r.Status().Update(ctx, fs); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Status persisted — now safe to remove annotation
		delete(fs.Annotations, forceAdvanceAnnotation)
		if err := r.Update(ctx, fs); err != nil {
			// Annotation removal failed but status was already applied.
			// Next reconcile will re-enter here, but handleForceAdvance
			// is idempotent (state already advanced).
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Dispatch based on transition phase
	phase := failoverv1alpha1.PhaseIdle
	if fs.Status.Transition != nil {
		phase = fs.Status.Transition.Phase
	}

	switch phase {
	case failoverv1alpha1.PhaseIdle:
		return r.reconcileIdle(ctx, fs)
	case failoverv1alpha1.PhaseExecutingPreActions:
		return r.executeActionPhase(ctx, fs, true)
	case failoverv1alpha1.PhaseUpdatingResources:
		return r.reconcileUpdatingResources(ctx, fs)
	case failoverv1alpha1.PhaseFlushingConnections:
		return r.reconcileFlushingConnections(ctx, fs)
	case failoverv1alpha1.PhaseExecutingPostActions:
		return r.executeActionPhase(ctx, fs, false)
	default:
		log.Error(nil, "Unknown transition phase", "phase", phase)
		meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
			Type:               failoverv1alpha1.ConditionActionFailed,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: fs.Generation,
			LastTransitionTime: metav1.Now(),
			Reason:             "UnknownPhase",
			Message:            fmt.Sprintf("Unknown transition phase %q; transition was reset", phase),
		})
		meta.RemoveStatusCondition(&fs.Status.Conditions, failoverv1alpha1.ConditionTransitionInProgress)
		fs.Status.Transition = nil
		r.updateClusterStatus(fs, failoverv1alpha1.PhaseIdle)
		if err := r.Status().Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
}

// reconcileIdle handles the normal idle state: detect transition first, then reconcile service.
// Transition detection MUST happen before service reconcile to prevent traffic switching
// before pre-actions have executed.
func (r *FailoverServiceReconciler) reconcileIdle(ctx context.Context, fs *failoverv1alpha1.FailoverService) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Detect transition BEFORE reconciling the service.
	// This ensures pre-actions run before the Service endpoints are updated.
	if fs.Status.LastKnownFailoverActive != nil && *fs.Status.LastKnownFailoverActive != fs.Spec.FailoverActive {
		log.Info("Failover transition detected")

		// Set specChangeTime on transition detection
		now := metav1.Now()
		fs.Status.SpecChangeTime = &now

		return r.beginTransition(ctx, fs, triggerManual)
	}

	// No transition pending — reconcile Service to ensure it matches current state.
	if err := r.ServiceManager.Reconcile(ctx, fs); err != nil {
		log.Error(err, "Failed to reconcile service")
		metrics.ReconcileErrorsTotal.WithLabelValues(fs.Name, fs.Namespace, "service").Inc()
		meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
			Type:               failoverv1alpha1.ConditionServiceReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: fs.Generation,
			LastTransitionTime: metav1.Now(),
			Reason:             "ReconcileFailed",
			Message:            err.Error(),
		})
		if statusErr := r.Status().Update(ctx, fs); statusErr != nil {
			log.Error(statusErr, "Failed to update status after service reconcile error")
		}
		return ctrl.Result{RequeueAfter: defaultBackoffMin}, nil
	}

	meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
		Type:               failoverv1alpha1.ConditionServiceReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: fs.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             "Reconciled",
		Message:            "Service is correctly configured",
	})

	if fs.Status.LastKnownFailoverActive == nil {
		// Initial reconcile — just set the baseline, no transition pipeline
		return r.reconcileSteadyState(ctx, fs)
	}

	// No transition — do remote sync and status update
	return r.reconcileSteadyState(ctx, fs)
}

// beginTransition initializes the transition state machine.
func (r *FailoverServiceReconciler) beginTransition(ctx context.Context, fs *failoverv1alpha1.FailoverService, trigger string) (ctrl.Result, error) {
	direction := directionFailover
	if !fs.Spec.FailoverActive {
		direction = directionFailback
	}

	// Increment transition counter for all trigger paths (manual and remote-sync).
	metricDirection := "to-failover"
	if direction == directionFailback {
		metricDirection = "to-primary"
	}
	metrics.FailoverTransitionsTotal.WithLabelValues(fs.Name, fs.Namespace, metricDirection).Inc()

	oldAddress, newAddress := r.getTransitionAddresses(fs, direction)

	hooks := r.getTransitionHooks(fs, direction)
	now := metav1.Now()
	transitionID := fmt.Sprintf("%d", now.UnixNano())

	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    direction,
		OldAddressSnapshot: oldAddress,
		NewAddressSnapshot: newAddress,
		TransitionID:       transitionID,
		CurrentActionIndex: 0,
		StartedAt:          &now,
	}

	// Initialize pre-action results
	if hooks != nil {
		transition.PreActionResults = makeActionResults(hooks.PreActions)
		transition.PostActionResults = makeActionResults(hooks.PostActions)
	}

	// Determine starting phase
	if hooks != nil && len(hooks.PreActions) > 0 {
		transition.Phase = failoverv1alpha1.PhaseExecutingPreActions
	} else {
		transition.Phase = failoverv1alpha1.PhaseUpdatingResources
	}

	// Compute event name and set it on the transition BEFORE persisting.
	// The event itself is created after the status write succeeds, so a failed
	// status write cannot leave orphaned FailoverEvent objects.
	transition.EventName = r.failoverEventName(fs, direction, &now)

	fs.Status.Transition = transition

	meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
		Type:               failoverv1alpha1.ConditionTransitionInProgress,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: fs.Generation,
		LastTransitionTime: now,
		Reason:             "TransitionStarted",
		Message:            fmt.Sprintf("Transition to %s started", direction),
	})

	r.updateClusterStatus(fs, transition.Phase)
	if err := r.Status().Update(ctx, fs); err != nil {
		return ctrl.Result{}, err
	}

	// Create the FailoverEvent now that the transition is persisted.
	r.createFailoverEvent(ctx, fs, direction, trigger, &now)

	return ctrl.Result{Requeue: true}, nil
}

// executeActionPhase runs actions sequentially (either pre or post).
func (r *FailoverServiceReconciler) executeActionPhase(ctx context.Context, fs *failoverv1alpha1.FailoverService, isPrePhase bool) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	transition := fs.Status.Transition

	hooks := r.getTransitionHooks(fs, transition.TargetDirection)
	var actions []failoverv1alpha1.FailoverAction
	var results []failoverv1alpha1.ActionResult
	if isPrePhase {
		if hooks != nil {
			actions = hooks.PreActions
		}
		results = transition.PreActionResults
	} else {
		if hooks != nil {
			actions = hooks.PostActions
		}
		results = transition.PostActionResults
	}

	idx := transition.CurrentActionIndex

	// Guard: negative index (shouldn't happen, but protect against corrupt status)
	if idx < 0 {
		log.Error(nil, "Negative action index detected, resetting to 0", "actionIndex", idx)
		transition.CurrentActionIndex = 0
		idx = 0
	}

	// Guard: if spec was edited mid-transition and results/actions length diverged, abort the phase
	if idx < len(actions) && idx >= len(results) {
		log.Error(nil, "Action results length mismatch — spec may have been edited mid-transition, skipping remaining actions",
			"actionIndex", idx, "actionsLen", len(actions), "resultsLen", len(results))
		if isPrePhase {
			transition.Phase = failoverv1alpha1.PhaseUpdatingResources
		} else {
			return r.completeTransition(ctx, fs)
		}
		transition.CurrentActionIndex = 0
		r.updateClusterStatus(fs, transition.Phase)
		if err := r.Status().Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// All actions done → advance to next phase
	if idx >= len(actions) {
		if isPrePhase {
			transition.Phase = failoverv1alpha1.PhaseUpdatingResources
		} else {
			// Post-actions complete → transition done
			return r.completeTransition(ctx, fs)
		}
		transition.CurrentActionIndex = 0
		r.updateClusterStatus(fs, transition.Phase)
		if err := r.Status().Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	currentAction := &actions[idx]
	result := &results[idx]

	// Handle terminal/pending states before execution
	if handled, res, err := r.handleActionState(ctx, fs, currentAction, result, transition); handled {
		return res, err
	}

	return r.runAction(ctx, fs, currentAction, result, transition)
}

// handleActionState handles pre-execution states (succeeded, skipped, failed, pending).
// Returns true if the state was handled and no execution is needed.
func (r *FailoverServiceReconciler) handleActionState(
	ctx context.Context,
	fs *failoverv1alpha1.FailoverService,
	currentAction *failoverv1alpha1.FailoverAction,
	result *failoverv1alpha1.ActionResult,
	transition *failoverv1alpha1.TransitionStatus,
) (bool, ctrl.Result, error) {
	// Check if already succeeded/skipped → advance
	if result.Status == failoverv1alpha1.ActionStatusSucceeded || result.Status == failoverv1alpha1.ActionStatusSkipped {
		transition.CurrentActionIndex++
		if err := r.Status().Update(ctx, fs); err != nil {
			return true, ctrl.Result{}, err
		}
		return true, ctrl.Result{Requeue: true}, nil
	}

	// Check if previously failed
	if result.Status == failoverv1alpha1.ActionStatusFailed {
		if currentAction.IgnoreFailure {
			result.Status = failoverv1alpha1.ActionStatusSkipped
			now := metav1.Now()
			result.CompletedAt = &now
			transition.CurrentActionIndex++
			if err := r.Status().Update(ctx, fs); err != nil {
				return true, ctrl.Result{}, err
			}
			return true, ctrl.Result{Requeue: true}, nil
		}
		// Halt — set condition, update event, and wait for force-advance
		r.updateFailoverEvent(ctx, fs, failoverv1alpha1.EventOutcomeFailed)
		meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
			Type:               failoverv1alpha1.ConditionActionFailed,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: fs.Generation,
			LastTransitionTime: metav1.Now(),
			Reason:             "ActionFailed",
			Message:            fmt.Sprintf("Action %q failed: %s", currentAction.Name, result.Message),
		})
		if err := r.Status().Update(ctx, fs); err != nil {
			return true, ctrl.Result{}, err
		}
		return true, ctrl.Result{}, nil // halt — no requeue
	}

	// Mark as running and persist BEFORE executing side effects (crash-safety)
	if result.Status == failoverv1alpha1.ActionStatusPending {
		result.Status = failoverv1alpha1.ActionStatusRunning
		now := metav1.Now()
		result.StartedAt = &now
		if err := r.Status().Update(ctx, fs); err != nil {
			return true, ctrl.Result{}, err
		}
		return true, ctrl.Result{Requeue: true}, nil
	}

	// Check WaitReady timeout at controller level
	if currentAction.WaitReady != nil && result.StartedAt != nil {
		timeout := int64(300)
		if currentAction.WaitReady.TimeoutSeconds != nil {
			timeout = *currentAction.WaitReady.TimeoutSeconds
		}
		if action.CheckTimeout(result.StartedAt, timeout) {
			result.Status = failoverv1alpha1.ActionStatusFailed
			now := metav1.Now()
			result.CompletedAt = &now
			result.Message = fmt.Sprintf("Timed out after %ds", timeout)
			actionType := r.actionType(currentAction)
			metrics.ActionExecutionsTotal.WithLabelValues(fs.Name, fs.Namespace, currentAction.Name, actionType, "failure").Inc()
			if err := r.Status().Update(ctx, fs); err != nil {
				return true, ctrl.Result{}, err
			}
			return true, ctrl.Result{Requeue: true}, nil
		}
	}

	// Check Scale+waitReady timeout at controller level
	if currentAction.Scale != nil && currentAction.Scale.WaitReady && result.StartedAt != nil {
		timeout := int64(300)
		if currentAction.Scale.TimeoutSeconds != nil {
			timeout = *currentAction.Scale.TimeoutSeconds
		}
		if action.CheckTimeout(result.StartedAt, timeout) {
			result.Status = failoverv1alpha1.ActionStatusFailed
			now := metav1.Now()
			result.CompletedAt = &now
			result.Message = fmt.Sprintf("Timed out waiting for readiness after %ds", timeout)
			actionType := r.actionType(currentAction)
			metrics.ActionExecutionsTotal.WithLabelValues(fs.Name, fs.Namespace, currentAction.Name, actionType, "failure").Inc()
			if err := r.Status().Update(ctx, fs); err != nil {
				return true, ctrl.Result{}, err
			}
			return true, ctrl.Result{Requeue: true}, nil
		}
	}

	// Check WaitRemote timeout at controller level
	if currentAction.WaitRemote != nil && result.StartedAt != nil {
		timeout := int64(300)
		if currentAction.WaitRemote.ActionTimeoutSeconds != nil {
			timeout = *currentAction.WaitRemote.ActionTimeoutSeconds
		}
		if action.CheckTimeout(result.StartedAt, timeout) {
			result.Status = failoverv1alpha1.ActionStatusFailed
			now := metav1.Now()
			result.CompletedAt = &now
			result.Message = fmt.Sprintf("Timed out waiting for remote after %ds", timeout)
			actionType := r.actionType(currentAction)
			metrics.ActionExecutionsTotal.WithLabelValues(fs.Name, fs.Namespace, currentAction.Name, actionType, "failure").Inc()
			if err := r.Status().Update(ctx, fs); err != nil {
				return true, ctrl.Result{}, err
			}
			return true, ctrl.Result{Requeue: true}, nil
		}
	}

	return false, ctrl.Result{}, nil
}

// actionRequeueInterval returns the appropriate requeue interval for this action type.
func (r *FailoverServiceReconciler) actionRequeueInterval(currentAction *failoverv1alpha1.FailoverAction) time.Duration {
	// Per-action override
	if currentAction.RetryIntervalSeconds != nil {
		return time.Duration(*currentAction.RetryIntervalSeconds) * time.Second
	}
	// Jobs have a longer default since the controller Owns Jobs (status changes trigger reconciles)
	if currentAction.Job != nil {
		return jobRequeueInterval
	}
	return defaultRequeueInterval
}

// actionMaxRetries returns the max retries for this action.
func (r *FailoverServiceReconciler) actionMaxRetries(currentAction *failoverv1alpha1.FailoverAction) int {
	if currentAction.MaxRetries != nil {
		return int(*currentAction.MaxRetries)
	}
	return defaultMaxRetries
}

// runAction executes an action and records the result.
func (r *FailoverServiceReconciler) runAction(
	ctx context.Context,
	fs *failoverv1alpha1.FailoverService,
	currentAction *failoverv1alpha1.FailoverAction,
	result *failoverv1alpha1.ActionResult,
	transition *failoverv1alpha1.TransitionStatus,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Execute the action — include transitionID in prefix to prevent stale Job name collisions
	namePrefix := fmt.Sprintf("%s-%s-%s", fs.Name, transition.TargetDirection, transition.TransitionID)
	// Pass owner reference and transition start time via context
	ownerRef := metav1.NewControllerRef(fs, failoverv1alpha1.GroupVersion.WithKind("FailoverService"))
	actionCtx := action.ContextWithOwnerRef(ctx, ownerRef)
	if transition.StartedAt != nil {
		actionCtx = action.ContextWithTransitionStartedAt(actionCtx, transition.StartedAt.Time)
	}
	actionCtx = action.ContextWithTransitionDirection(actionCtx, transition.TargetDirection)
	execResult, err := r.ActionExecutor.Execute(actionCtx, currentAction, fs.Namespace, namePrefix, transition.CurrentActionIndex)
	if err != nil {
		result.RetryCount++
		maxRetries := r.actionMaxRetries(currentAction)
		requeueInterval := r.actionRequeueInterval(currentAction)
		if result.RetryCount > maxRetries {
			// Max retries exceeded — mark as failed
			log.Error(err, "Action execution error (max retries exceeded)", "action", currentAction.Name, "retries", result.RetryCount)
			result.Status = failoverv1alpha1.ActionStatusFailed
			now := metav1.Now()
			result.CompletedAt = &now
			result.Message = fmt.Sprintf("failed after %d retries: %v", result.RetryCount, err)
			actionType := r.actionType(currentAction)
			metrics.ActionExecutionsTotal.WithLabelValues(fs.Name, fs.Namespace, currentAction.Name, actionType, "failure").Inc()
			if err := r.Status().Update(ctx, fs); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		// Transient error — keep action as Running and retry
		log.Error(err, "Action execution error (will retry)", "action", currentAction.Name, "retryCount", result.RetryCount)
		result.Message = fmt.Sprintf("transient error (retry %d/%d): %v", result.RetryCount, maxRetries, err)
		if err := r.Status().Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	result.JobName = execResult.JobName
	result.Message = execResult.Message

	if !execResult.Completed {
		// Still running — update status and requeue
		requeueInterval := r.actionRequeueInterval(currentAction)
		if err := r.Status().Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// Action completed
	now := metav1.Now()
	result.CompletedAt = &now
	if execResult.Succeeded {
		result.Status = failoverv1alpha1.ActionStatusSucceeded
		// Record metrics
		if result.StartedAt != nil {
			duration := now.Time.Sub(result.StartedAt.Time).Seconds()
			actionType := r.actionType(currentAction)
			metrics.ActionExecutionDuration.WithLabelValues(fs.Name, fs.Namespace, currentAction.Name, actionType).Observe(duration)
			metrics.ActionExecutionsTotal.WithLabelValues(fs.Name, fs.Namespace, currentAction.Name, actionType, "success").Inc()
		}
		transition.CurrentActionIndex++
	} else {
		result.Status = failoverv1alpha1.ActionStatusFailed
		if result.StartedAt != nil {
			actionType := r.actionType(currentAction)
			metrics.ActionExecutionsTotal.WithLabelValues(fs.Name, fs.Namespace, currentAction.Name, actionType, "failure").Inc()
		}
	}

	if err := r.Status().Update(ctx, fs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileUpdatingResources updates the Service and advances to flushing.
func (r *FailoverServiceReconciler) reconcileUpdatingResources(ctx context.Context, fs *failoverv1alpha1.FailoverService) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Use the snapshotted target address, not the live spec, to prevent
	// mid-transition spec edits from changing the in-flight destination.
	targetAddress := fs.Status.Transition.NewAddressSnapshot
	if targetAddress == "" {
		// Backward compatibility for transitions created before address snapshotting.
		_, targetAddress = r.getTransitionAddresses(fs, fs.Status.Transition.TargetDirection)
	}
	if err := r.ServiceManager.ReconcileWithAddress(ctx, fs, targetAddress); err != nil {
		log.Error(err, "Failed to reconcile service during transition")
		metrics.ReconcileErrorsTotal.WithLabelValues(fs.Name, fs.Namespace, "service").Inc()
		return ctrl.Result{RequeueAfter: defaultBackoffMin}, nil
	}

	meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
		Type:               failoverv1alpha1.ConditionServiceReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: fs.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             "Reconciled",
		Message:            "Service is correctly configured",
	})

	fs.Status.Transition.Phase = failoverv1alpha1.PhaseFlushingConnections
	fs.Status.Transition.CurrentActionIndex = 0

	r.updateClusterStatus(fs, failoverv1alpha1.PhaseFlushingConnections)
	if err := r.Status().Update(ctx, fs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileFlushingConnections kills connections and waits for ACKs before advancing.
func (r *FailoverServiceReconciler) reconcileFlushingConnections(ctx context.Context, fs *failoverv1alpha1.FailoverService) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	transition := fs.Status.Transition

	connKillApplied := false
	if r.ConnSignaler != nil {
		oldAddress := r.getOldTargetAddress(fs)
		if oldAddress != "" && net.ParseIP(oldAddress) != nil {
			// Step 1: Send signal if not yet sent
			if transition.ConnectionKill == nil || !transition.ConnectionKill.SignalSent {
				killToken := fmt.Sprintf("%s-%d", transition.TransitionID, time.Now().UnixNano())
				nodeNames, err := r.ConnSignaler.SignalKill(ctx, oldAddress, killToken, fs.Namespace, fs.Name)
				if err != nil {
					log.Error(err, "Failed to signal connection kill")
					metrics.ReconcileErrorsTotal.WithLabelValues(fs.Name, fs.Namespace, "connkill").Inc()
					meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
						Type:               failoverv1alpha1.ConditionConnectionsKilled,
						Status:             metav1.ConditionFalse,
						ObservedGeneration: fs.Generation,
						LastTransitionTime: metav1.Now(),
						Reason:             "SignalFailed",
						Message:            err.Error(),
					})
					if statusErr := r.Status().Update(ctx, fs); statusErr != nil {
						log.Error(statusErr, "Failed to update status after connection kill signal error")
					}
					return ctrl.Result{RequeueAfter: defaultBackoffMin}, nil
				}

				if len(nodeNames) == 0 {
					log.Info("No ready connection-killer agents available; skipping connection kill ACK wait")
					transition.ConnectionKill = nil
					connKillApplied = true
					meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
						Type:               failoverv1alpha1.ConditionConnectionsKilled,
						Status:             metav1.ConditionTrue,
						ObservedGeneration: fs.Generation,
						LastTransitionTime: metav1.Now(),
						Reason:             "Skipped",
						Message:            "No ready connection-killer agents available; connection kill skipped",
					})
				} else {
					now := metav1.Now()
					transition.ConnectionKill = &failoverv1alpha1.ConnectionKillStatus{
						SignalSent:    true,
						KillToken:     killToken,
						ExpectedNodes: nodeNames,
						SignaledAt:    &now,
					}

					meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
						Type:               failoverv1alpha1.ConditionConnectionsKilled,
						Status:             metav1.ConditionFalse,
						ObservedGeneration: fs.Generation,
						LastTransitionTime: now,
						Reason:             "WaitingForAcks",
						Message:            fmt.Sprintf("Kill signal sent for %s, waiting for %d node ACKs", oldAddress, len(nodeNames)),
					})

					r.updateClusterStatus(fs, failoverv1alpha1.PhaseFlushingConnections)
					if err := r.Status().Update(ctx, fs); err != nil {
						log.Error(err, "Failed to update status after signaling connection kill")
						return ctrl.Result{RequeueAfter: connKillPollInterval}, nil
					}
					return ctrl.Result{RequeueAfter: connKillPollInterval}, nil
				}
			}

			if transition.ConnectionKill != nil && transition.ConnectionKill.SignalSent {
				// Step 2: Poll for ACKs
				ckStatus := transition.ConnectionKill
				ackStatus, err := r.ConnSignaler.CheckAcks(ctx, ckStatus.KillToken, ckStatus.ExpectedNodes, fs.Namespace, fs.Name)
				if err != nil {
					log.Error(err, "Failed to check connection kill ACKs")
					// Check timeout even on error to avoid infinite retry loop
					if ckStatus.SignaledAt != nil && time.Since(ckStatus.SignaledAt.Time) > connKillTimeout {
						log.Error(nil, "Connection kill ACK check error with timeout exceeded, advancing")
						meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
							Type:               failoverv1alpha1.ConditionConnectionsKilled,
							Status:             metav1.ConditionFalse,
							ObservedGeneration: fs.Generation,
							LastTransitionTime: metav1.Now(),
							Reason:             "AckTimeout",
							Message:            fmt.Sprintf("Timed out checking ACKs: %v", err),
						})
						if statusErr := r.Status().Update(ctx, fs); statusErr != nil {
							return ctrl.Result{RequeueAfter: connKillPollInterval}, nil
						}
						return ctrl.Result{}, nil // halt — wait for force-advance
					}
					return ctrl.Result{RequeueAfter: defaultBackoffMin}, nil
				}

				ckStatus.AckedNodes = ackStatus.AckedNodes

				if !ackStatus.AllAcked {
					// Check timeout
					if ckStatus.SignaledAt != nil && time.Since(ckStatus.SignaledAt.Time) > connKillTimeout {
						log.Error(nil, "Connection kill ACK timeout",
							"pending", ackStatus.PendingNodes,
							"errors", ackStatus.ErrorNodes)
						meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
							Type:               failoverv1alpha1.ConditionConnectionsKilled,
							Status:             metav1.ConditionFalse,
							ObservedGeneration: fs.Generation,
							LastTransitionTime: metav1.Now(),
							Reason:             "AckTimeout",
							Message:            fmt.Sprintf("Timed out waiting for ACKs from nodes: %v", ackStatus.PendingNodes),
						})
						if err := r.Status().Update(ctx, fs); err != nil {
							log.Error(err, "Failed to update status after ACK timeout")
							return ctrl.Result{RequeueAfter: connKillPollInterval}, nil
						}
						// Halt — wait for force-advance
						return ctrl.Result{}, nil
					}

					log.Info("Waiting for connection kill ACKs",
						"acked", len(ackStatus.AckedNodes),
						"pending", len(ackStatus.PendingNodes),
						"errors", len(ackStatus.ErrorNodes))
					if err := r.Status().Update(ctx, fs); err != nil {
						log.Error(err, "Failed to update status while waiting for ACKs")
					}
					return ctrl.Result{RequeueAfter: connKillPollInterval}, nil
				}

				// All ACKed
				metrics.ConnectionsKilledTotal.Inc()
				connKillApplied = true
				meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
					Type:               failoverv1alpha1.ConditionConnectionsKilled,
					Status:             metav1.ConditionTrue,
					ObservedGeneration: fs.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             "AllNodesAcked",
					Message:            fmt.Sprintf("All %d nodes confirmed connection kill for %s", len(ckStatus.ExpectedNodes), oldAddress),
				})
			}
		}
	}

	// If connection kill was not applicable (no signaler, non-IP address), mark as skipped
	if !connKillApplied {
		meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
			Type:               failoverv1alpha1.ConditionConnectionsKilled,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: fs.Generation,
			LastTransitionTime: metav1.Now(),
			Reason:             "Skipped",
			Message:            "Connection kill not applicable for this transition",
		})
	}

	// Advance to post-actions or complete
	hooks := r.getTransitionHooks(fs, fs.Status.Transition.TargetDirection)
	if hooks != nil && len(hooks.PostActions) > 0 {
		fs.Status.Transition.Phase = failoverv1alpha1.PhaseExecutingPostActions
		fs.Status.Transition.CurrentActionIndex = 0
		r.updateClusterStatus(fs, failoverv1alpha1.PhaseExecutingPostActions)
	} else {
		return r.completeTransition(ctx, fs)
	}

	if err := r.Status().Update(ctx, fs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// completeTransition finalizes a transition and returns to idle.
func (r *FailoverServiceReconciler) completeTransition(ctx context.Context, fs *failoverv1alpha1.FailoverService) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Record transition duration
	if fs.Status.Transition != nil && fs.Status.Transition.StartedAt != nil {
		duration := time.Since(fs.Status.Transition.StartedAt.Time).Seconds()
		metrics.TransitionDuration.WithLabelValues(fs.Name, fs.Namespace, fs.Status.Transition.TargetDirection).Observe(duration)
	}

	// Clean up any jobs created during the transition
	if fs.Status.Transition != nil {
		r.cleanupTransitionJobs(ctx, fs)
		if r.ConnSignaler != nil {
			if err := r.ConnSignaler.CleanupSignals(ctx, fs.Namespace, fs.Name); err != nil {
				log.Error(err, "Failed to cleanup connection-kill signals after transition")
			}
		}
	}

	// Save transition data before clearing — needed for event update after FS status write.
	savedTransition := fs.Status.Transition

	// Derive the resulting failover state from the snapshotted transition direction,
	// not the live spec. This ensures that if spec.failoverActive was flipped again
	// during the transition, the next reconcile will correctly detect it as a new
	// transition rather than thinking nothing changed.
	transitionedToFailover := fs.Status.Transition.TargetDirection == directionFailover
	fs.Status.LastKnownFailoverActive = &transitionedToFailover

	// Update failover_active gauge based on the completed transition
	val := float64(0)
	if transitionedToFailover {
		val = 1
	}
	metrics.FailoverActive.WithLabelValues(fs.Name, fs.Namespace).Set(val)
	now := metav1.Now()
	fs.Status.LastTransitionTime = &now
	fs.Status.ObservedGeneration = fs.Generation
	fs.Status.Transition = nil
	fs.Status.ActiveGeneration++
	fs.Status.ActiveCluster = r.ClusterID

	// Update per-cluster status
	r.updateClusterStatus(fs, failoverv1alpha1.PhaseIdle)

	meta.RemoveStatusCondition(&fs.Status.Conditions, failoverv1alpha1.ConditionTransitionInProgress)
	meta.RemoveStatusCondition(&fs.Status.Conditions, failoverv1alpha1.ConditionActionFailed)

	// Remote sync
	if r.Syncer != nil {
		syncResult, err := r.Syncer.Sync(ctx, fs)
		if err != nil {
			log.Error(err, "Failed to sync with remote cluster")
			metrics.ReconcileErrorsTotal.WithLabelValues(fs.Name, fs.Namespace, "remote-sync").Inc()
			meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
				Type:               failoverv1alpha1.ConditionRemoteSynced,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: fs.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "SyncFailed",
				Message:            err.Error(),
			})
		} else {
			fs.Status.RemoteState = syncResult.RemoteState

			if syncResult.RemoteWins {
				log.Info("Remote cluster wins conflict, adopting remote state",
					"remoteActiveGeneration", syncResult.RemoteState.ActiveGeneration,
					"remoteActiveCluster", syncResult.RemoteState.ActiveCluster)
				fs.Spec.FailoverActive = syncResult.RemoteState.FailoverActive
				if err := r.Update(ctx, fs); err != nil {
					return ctrl.Result{}, err
				}
				r.applyRemoteWinnerState(fs, syncResult.RemoteState, transitionedToFailover)
				fs.Status.Transition = nil
				meta.RemoveStatusCondition(&fs.Status.Conditions, failoverv1alpha1.ConditionTransitionInProgress)
				meta.RemoveStatusCondition(&fs.Status.Conditions, failoverv1alpha1.ConditionActionFailed)
				r.updateClusterStatus(fs, failoverv1alpha1.PhaseIdle)
				if err := r.Status().Update(ctx, fs); err != nil {
					return ctrl.Result{}, err
				}
				fs.Status.Transition = savedTransition
				r.updateFailoverEvent(ctx, fs, failoverv1alpha1.EventOutcomeSucceeded)
				fs.Status.Transition = nil
				// If the remote winner changed the spec, start the transition directly
				// with the correct trigger instead of relying on reconcileIdle (which
				// always uses triggerManual).
				if transitionedToFailover != fs.Spec.FailoverActive {
					return r.beginTransition(ctx, fs, triggerRemoteSync)
				}
				return ctrl.Result{Requeue: true}, nil
			}

			meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
				Type:               failoverv1alpha1.ConditionRemoteSynced,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: fs.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "Synced",
				Message:            "Remote cluster state is in sync",
			})
		}
	}

	if err := r.Status().Update(ctx, fs); err != nil {
		return ctrl.Result{}, err
	}

	// Update FailoverEvent AFTER FS status is persisted to avoid divergence
	// where the event says "succeeded" but FS is still mid-transition.
	fs.Status.Transition = savedTransition
	r.updateFailoverEvent(ctx, fs, failoverv1alpha1.EventOutcomeSucceeded)
	fs.Status.Transition = nil

	log.Info("Transition completed",
		"failoverActive", fs.Spec.FailoverActive,
		"activeGeneration", fs.Status.ActiveGeneration,
		"activeCluster", fs.Status.ActiveCluster)
	return ctrl.Result{RequeueAfter: antiEntropyInterval}, nil
}

// reconcileSteadyState handles normal steady-state reconciliation (no transition).
func (r *FailoverServiceReconciler) reconcileSteadyState(ctx context.Context, fs *failoverv1alpha1.FailoverService) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	currentFailoverActive := fs.Spec.FailoverActive

	// Remote Sync
	if r.Syncer != nil {
		syncResult, err := r.Syncer.Sync(ctx, fs)
		if err != nil {
			log.Error(err, "Failed to sync with remote cluster")
			metrics.ReconcileErrorsTotal.WithLabelValues(fs.Name, fs.Namespace, "remote-sync").Inc()
			meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
				Type:               failoverv1alpha1.ConditionRemoteSynced,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: fs.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "SyncFailed",
				Message:            err.Error(),
			})
		} else {
			fs.Status.RemoteState = syncResult.RemoteState

			if syncResult.RemoteWins {
				log.Info("Remote cluster wins conflict, adopting remote state",
					"remoteActiveGeneration", syncResult.RemoteState.ActiveGeneration,
					"remoteActiveCluster", syncResult.RemoteState.ActiveCluster)
				fs.Spec.FailoverActive = syncResult.RemoteState.FailoverActive
				if err := r.Update(ctx, fs); err != nil {
					return ctrl.Result{}, err
				}
				r.applyRemoteWinnerState(fs, syncResult.RemoteState, currentFailoverActive)
				r.updateClusterStatus(fs, failoverv1alpha1.PhaseIdle)
				if err := r.Status().Update(ctx, fs); err != nil {
					return ctrl.Result{}, err
				}
				// If the spec actually changed, start the transition directly with the
				// correct trigger instead of relying on reconcileIdle (which always uses triggerManual).
				if currentFailoverActive != fs.Spec.FailoverActive {
					return r.beginTransition(ctx, fs, triggerRemoteSync)
				}
				return ctrl.Result{Requeue: true}, nil
			}

			meta.SetStatusCondition(&fs.Status.Conditions, metav1.Condition{
				Type:               failoverv1alpha1.ConditionRemoteSynced,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: fs.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "Synced",
				Message:            "Remote cluster state is in sync",
			})
		}
	}

	// Update Status
	fs.Status.ObservedGeneration = fs.Generation
	failoverActive := fs.Spec.FailoverActive
	fs.Status.LastKnownFailoverActive = &failoverActive

	// Update per-cluster status for observability
	r.updateClusterStatus(fs, failoverv1alpha1.PhaseIdle)

	// Update failover_active gauge
	val := float64(0)
	if fs.Spec.FailoverActive {
		val = 1
	}
	metrics.FailoverActive.WithLabelValues(fs.Name, fs.Namespace).Set(val)

	if err := r.Status().Update(ctx, fs); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: antiEntropyInterval}, nil
}

// applyRemoteWinnerState updates local status to reflect a remote winner while preserving
// transition detection by retaining the previous local failover state in LastKnownFailoverActive.
func (r *FailoverServiceReconciler) applyRemoteWinnerState(
	fs *failoverv1alpha1.FailoverService,
	remoteState *failoverv1alpha1.RemoteState,
	previousFailoverActive bool,
) {
	fs.Status.LastKnownFailoverActive = &previousFailoverActive
	fs.Status.ObservedGeneration = fs.Generation
	fs.Status.RemoteState = remoteState
	if remoteState == nil {
		return
	}
	fs.Status.SpecChangeTime = remoteState.SpecChangeTime

	// Adopt the remote authoritative revision to avoid stale generation skew.
	fs.Status.ActiveGeneration = remoteState.ActiveGeneration
	if remoteState.ActiveCluster != "" {
		fs.Status.ActiveCluster = remoteState.ActiveCluster
	}
}

// handleForceAdvance processes a force-advance request.
// The annotation has already been removed before this is called.
// Returns true if status was modified and needs persisting.
func (r *FailoverServiceReconciler) handleForceAdvance(ctx context.Context, fs *failoverv1alpha1.FailoverService, _ string) bool {
	if fs.Status.Transition == nil {
		return false
	}

	log := logf.FromContext(ctx)
	log.Info("Force-advance triggered", "phase", fs.Status.Transition.Phase)

	// Update FailoverEvent for force-advance
	r.updateFailoverEvent(ctx, fs, failoverv1alpha1.EventOutcomeForceAdvanced)

	transition := fs.Status.Transition

	// Force-advance never completes the transition inline — it skips the current
	// action/phase and lets the normal reconcile loop handle completion via
	// completeTransition, which properly bumps revision, cleans up jobs, and records metrics.
	switch transition.Phase {
	case failoverv1alpha1.PhaseExecutingPreActions:
		r.forceAdvanceActionPhase(transition, true)
	case failoverv1alpha1.PhaseExecutingPostActions:
		r.forceAdvanceActionPhase(transition, false)
		// Don't complete inline — the next reconcile of executeActionPhase
		// will see idx >= len(actions) and route to completeTransition.
	case failoverv1alpha1.PhaseUpdatingResources:
		transition.Phase = failoverv1alpha1.PhaseFlushingConnections
		transition.CurrentActionIndex = 0
	case failoverv1alpha1.PhaseFlushingConnections:
		// Advance to post-actions phase; if there are no post-actions,
		// executeActionPhase will immediately call completeTransition.
		transition.Phase = failoverv1alpha1.PhaseExecutingPostActions
		transition.CurrentActionIndex = 0
	}

	// Clear action failed condition
	meta.RemoveStatusCondition(&fs.Status.Conditions, failoverv1alpha1.ConditionActionFailed)

	return true
}

func (r *FailoverServiceReconciler) forceAdvanceActionPhase(transition *failoverv1alpha1.TransitionStatus, isPrePhase bool) {
	var results []failoverv1alpha1.ActionResult
	if isPrePhase {
		results = transition.PreActionResults
	} else {
		results = transition.PostActionResults
	}

	idx := transition.CurrentActionIndex
	if idx >= 0 && idx < len(results) {
		results[idx].Status = failoverv1alpha1.ActionStatusSkipped
		now := metav1.Now()
		results[idx].CompletedAt = &now
		results[idx].Message = "Force-advanced by operator"
		transition.CurrentActionIndex++
	}
}

// createFailoverEvent creates a FailoverEvent resource for this transition.
// failoverEventName computes a deterministic name for a FailoverEvent.
func (r *FailoverServiceReconciler) failoverEventName(fs *failoverv1alpha1.FailoverService, direction string, startedAt *metav1.Time) string {
	raw := fmt.Sprintf("%s-%s-%d", fs.Name, direction, startedAt.UnixNano())
	if len(raw) <= 63 {
		return raw
	}
	h := sha256.Sum256([]byte(raw))
	suffix := hex.EncodeToString(h[:4])
	maxPrefix := 63 - 1 - len(suffix) // 1 for dash separator
	return raw[:maxPrefix] + "-" + suffix
}

func (r *FailoverServiceReconciler) createFailoverEvent(ctx context.Context, fs *failoverv1alpha1.FailoverService, direction, trigger string, startedAt *metav1.Time) {
	log := logf.FromContext(ctx)
	eventName := fs.Status.Transition.EventName

	event := &failoverv1alpha1.FailoverEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventName,
			Namespace: fs.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(fs, failoverv1alpha1.GroupVersion.WithKind("FailoverService")),
			},
		},
		Spec: failoverv1alpha1.FailoverEventSpec{
			FailoverServiceRef: fs.Name,
			Direction:          direction,
			TriggeredBy:        trigger,
		},
	}

	if err := r.Create(ctx, event); err != nil {
		log.Error(err, "Failed to create FailoverEvent", "event", eventName)
		return
	}

	// Update status
	event.Status = failoverv1alpha1.FailoverEventStatus{
		StartedAt:              startedAt,
		Outcome:                failoverv1alpha1.EventOutcomeInProgress,
		ActiveGenerationBefore: fs.Status.ActiveGeneration,
	}
	if err := r.Status().Update(ctx, event); err != nil {
		log.Error(err, "Failed to update FailoverEvent status", "event", eventName)
	}
}

// updateFailoverEvent updates the FailoverEvent with the final outcome.
// This is idempotent: once an event reaches a terminal state (anything other than
// InProgress), subsequent calls are no-ops to avoid reconcile churn from Owns() watches.
func (r *FailoverServiceReconciler) updateFailoverEvent(ctx context.Context, fs *failoverv1alpha1.FailoverService, outcome string) {
	if fs.Status.Transition == nil || fs.Status.Transition.EventName == "" {
		return
	}
	log := logf.FromContext(ctx)
	eventName := fs.Status.Transition.EventName

	event := &failoverv1alpha1.FailoverEvent{}
	if err := r.Get(ctx, client.ObjectKey{Name: eventName, Namespace: fs.Namespace}, event); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get FailoverEvent for update", "event", eventName)
		}
		return
	}

	// Prevent no-op rewrites that cause reconcile churn from Owns() watches.
	// Allowed transitions:
	//   InProgress -> any
	//   Failed -> ForceAdvanced  (force-advance overrides failure)
	//   ForceAdvanced -> Failed  (action fails again after force-advance)
	//   ForceAdvanced -> Succeeded  (completion after force-advance; outcome stays ForceAdvanced)
	// All other terminal->terminal transitions are no-ops.
	currentOutcome := event.Status.Outcome
	if currentOutcome != "" && currentOutcome != failoverv1alpha1.EventOutcomeInProgress {
		switch {
		case currentOutcome == failoverv1alpha1.EventOutcomeFailed && outcome == failoverv1alpha1.EventOutcomeForceAdvanced:
			// Allow: force-advance overrides a failed state
		case currentOutcome == failoverv1alpha1.EventOutcomeForceAdvanced && outcome == failoverv1alpha1.EventOutcomeFailed:
			// Allow: action fails again after force-advance
		case currentOutcome == failoverv1alpha1.EventOutcomeForceAdvanced && outcome == failoverv1alpha1.EventOutcomeSucceeded:
			// Allow: completion after force-advance (outcome stays ForceAdvanced, but results are updated below)
		default:
			return
		}
	}

	event.Status.PreActionResults = fs.Status.Transition.PreActionResults
	event.Status.PostActionResults = fs.Status.Transition.PostActionResults
	event.Status.ConnectionKillResult = fs.Status.Transition.ConnectionKill

	// Don't downgrade ForceAdvanced to Succeeded — ForceAdvanced indicates
	// the transition required manual intervention even though it completed.
	if outcome == failoverv1alpha1.EventOutcomeSucceeded && currentOutcome == failoverv1alpha1.EventOutcomeForceAdvanced {
		// Keep ForceAdvanced but update results and generation.
		// ActiveGeneration was already incremented in completeTransition.
		event.Status.ActiveGenerationAfter = fs.Status.ActiveGeneration
	} else {
		event.Status.Outcome = outcome
		if outcome == failoverv1alpha1.EventOutcomeFailed {
			// Failed transitions don't increment generation
			event.Status.ActiveGenerationAfter = fs.Status.ActiveGeneration
		} else {
			// Succeeded: ActiveGeneration was already incremented in completeTransition.
			// ForceAdvanced: generation not yet incremented (transition still in-flight).
			event.Status.ActiveGenerationAfter = fs.Status.ActiveGeneration
		}
	}

	// Only stamp CompletedAt for terminal outcomes. ForceAdvanced is not terminal —
	// the transition continues via the normal reconcile loop after force-advance.
	if outcome != failoverv1alpha1.EventOutcomeForceAdvanced {
		now := metav1.Now()
		event.Status.CompletedAt = &now
	}

	if err := r.Status().Update(ctx, event); err != nil {
		log.Error(err, "Failed to update FailoverEvent", "event", eventName)
	}
}

// getTransitionHooks returns the transition actions for the current cluster role and direction.
func (r *FailoverServiceReconciler) getTransitionHooks(fs *failoverv1alpha1.FailoverService, direction string) *failoverv1alpha1.TransitionActions {
	var target *failoverv1alpha1.ClusterTarget
	if r.ClusterRole == rolePrimary {
		target = &fs.Spec.PrimaryCluster
	} else {
		target = &fs.Spec.FailoverCluster
	}

	if direction == directionFailover {
		return target.OnFailover
	}
	return target.OnFailback
}

// getTransitionAddresses returns the old and new addresses for a transition direction
// on the local cluster role target.
func (r *FailoverServiceReconciler) getTransitionAddresses(fs *failoverv1alpha1.FailoverService, direction string) (string, string) {
	var target *failoverv1alpha1.ClusterTarget
	if r.ClusterRole == rolePrimary {
		target = &fs.Spec.PrimaryCluster
	} else {
		target = &fs.Spec.FailoverCluster
	}

	if direction == directionFailover {
		return target.PrimaryModeAddress, target.FailoverModeAddress
	}
	return target.FailoverModeAddress, target.PrimaryModeAddress
}

// getOldTargetAddress returns the address that was active before the transition.
// Prefers the explicit transition snapshot and falls back to direction-based derivation
// for backward compatibility with older TransitionStatus payloads.
func (r *FailoverServiceReconciler) getOldTargetAddress(fs *failoverv1alpha1.FailoverService) string {
	if fs.Status.Transition != nil && fs.Status.Transition.OldAddressSnapshot != "" {
		return fs.Status.Transition.OldAddressSnapshot
	}

	var target *failoverv1alpha1.ClusterTarget
	if r.ClusterRole == rolePrimary {
		target = &fs.Spec.PrimaryCluster
	} else {
		target = &fs.Spec.FailoverCluster
	}

	// Use the transition direction snapshot, not live spec, to determine old address
	if fs.Status.Transition != nil && fs.Status.Transition.TargetDirection == directionFailover {
		// Transitioning to failover → old was primary mode
		return target.PrimaryModeAddress
	}
	// Transitioning to primary (failback) → old was failover mode
	return target.FailoverModeAddress
}

// cleanupTransitionJobs deletes any Jobs created during the transition.
func (r *FailoverServiceReconciler) cleanupTransitionJobs(ctx context.Context, fs *failoverv1alpha1.FailoverService) {
	log := logf.FromContext(ctx)
	if r.ActionExecutor == nil {
		return
	}

	allResults := append(fs.Status.Transition.PreActionResults, fs.Status.Transition.PostActionResults...)
	for _, result := range allResults {
		if result.JobName != "" {
			if err := r.ActionExecutor.Cleanup(ctx, result.JobName, fs.Namespace); err != nil {
				log.Error(err, "Failed to cleanup job", "job", result.JobName)
			}
		}
	}
}

// updateClusterStatus updates or inserts the per-cluster status entry for this cluster.
func (r *FailoverServiceReconciler) updateClusterStatus(fs *failoverv1alpha1.FailoverService, phase failoverv1alpha1.FailoverPhase) {
	now := metav1.Now()
	for i := range fs.Status.Clusters {
		if fs.Status.Clusters[i].ClusterID == r.ClusterID {
			fs.Status.Clusters[i].AppliedGeneration = fs.Status.ActiveGeneration
			fs.Status.Clusters[i].LastAppliedTime = &now
			fs.Status.Clusters[i].Phase = phase
			return
		}
	}
	fs.Status.Clusters = append(fs.Status.Clusters, failoverv1alpha1.ClusterStatus{
		ClusterID:         r.ClusterID,
		AppliedGeneration: fs.Status.ActiveGeneration,
		LastAppliedTime:   &now,
		Phase:             phase,
	})
}

func (r *FailoverServiceReconciler) actionType(a *failoverv1alpha1.FailoverAction) string {
	switch {
	case a.HTTP != nil:
		return "http"
	case a.Job != nil:
		return "job"
	case a.Scale != nil:
		return "scale"
	case a.WaitReady != nil:
		return "waitReady"
	case a.WaitRemote != nil:
		return "waitRemote"
	default:
		return "unknown"
	}
}

// makeActionResults creates pending ActionResult entries for a list of actions.
func makeActionResults(actions []failoverv1alpha1.FailoverAction) []failoverv1alpha1.ActionResult {
	results := make([]failoverv1alpha1.ActionResult, len(actions))
	for i, a := range actions {
		results[i] = failoverv1alpha1.ActionResult{
			Name:   a.Name,
			Status: failoverv1alpha1.ActionStatusPending,
		}
	}
	return results
}

// SetupWithManager sets up the controller with the Manager.
func (r *FailoverServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&failoverv1alpha1.FailoverService{}).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Owns(&batchv1.Job{}).
		Owns(&failoverv1alpha1.FailoverEvent{}).
		Named("failoverservice")

	if r.RemoteWatcher != nil {
		builder = builder.WatchesRawSource(r.RemoteWatcher)
	}

	return builder.Complete(r)
}

// NewReconcilerFromEnv creates a FailoverServiceReconciler with settings from environment variables.
func NewReconcilerFromEnv(c client.Client, scheme *runtime.Scheme) (*FailoverServiceReconciler, error) {
	clusterRole := os.Getenv("CLUSTER_ROLE")
	if clusterRole == "" {
		clusterRole = rolePrimary
	}
	if clusterRole != rolePrimary && clusterRole != roleFailover {
		return nil, fmt.Errorf("CLUSTER_ROLE must be 'primary' or 'failover', got %q", clusterRole)
	}

	clusterID := os.Getenv("CLUSTER_ID")
	if clusterID == "" {
		return nil, fmt.Errorf("CLUSTER_ID is required")
	}

	secretName := os.Getenv("REMOTE_KUBECONFIG_SECRET")
	if secretName == "" {
		return nil, fmt.Errorf("REMOTE_KUBECONFIG_SECRET is required")
	}

	secretNamespace := os.Getenv("REMOTE_KUBECONFIG_NAMESPACE")
	if secretNamespace == "" {
		return nil, fmt.Errorf("REMOTE_KUBECONFIG_NAMESPACE is required")
	}

	controllerNamespace := os.Getenv("CONTROLLER_NAMESPACE")
	if controllerNamespace == "" {
		controllerNamespace = "k8s-failover-manager-system"
	}

	svcMgr := &svcmanager.Manager{
		Client:      c,
		ClusterRole: clusterRole,
	}

	waitReadyExec := &action.WaitReadyExecutor{Client: c}

	provider := &remotecluster.ClientProvider{
		LocalClient:     c,
		Scheme:          scheme,
		SecretName:      secretName,
		SecretNamespace: secretNamespace,
	}

	actionExecutor := &action.MultiExecutor{
		HTTP: &action.HTTPExecutor{},
		Job:  &action.JobExecutor{Client: c},
		Scale: &action.ScaleExecutor{
			Client:    c,
			WaitReady: waitReadyExec,
		},
		WaitReady: waitReadyExec,
		WaitRemote: &action.WaitRemoteExecutor{
			ClientProvider: provider,
		},
	}

	syncer := &remotecluster.Syncer{
		ClientProvider: provider,
		ClusterID:      clusterID,
	}

	remoteWatcher := &remotecluster.RemoteWatcher{
		ClientProvider: provider,
	}

	reconciler := &FailoverServiceReconciler{
		Client:         c,
		Scheme:         scheme,
		ClusterRole:    clusterRole,
		ClusterID:      clusterID,
		ServiceManager: svcMgr,
		Syncer:         syncer,
		ActionExecutor: actionExecutor,
		RemoteWatcher:  remoteWatcher,
	}

	// Set up connection kill signaler
	reconciler.ConnSignaler = &connkill.Signaler{
		Client:    c,
		Namespace: controllerNamespace,
	}

	return reconciler, nil
}
