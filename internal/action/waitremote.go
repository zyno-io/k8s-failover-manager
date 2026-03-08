package action

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

const (
	defaultConnectTimeout = 10 * time.Second
	defaultActionTimeout  = 300
)

// RemoteClientProvider abstracts remote client creation for testability.
type RemoteClientProvider interface {
	GetClient(ctx context.Context) (client.Client, error)
}

// WaitRemoteExecutor waits for a condition on the remote cluster's FailoverService.
type WaitRemoteExecutor struct {
	ClientProvider RemoteClientProvider
}

func (e *WaitRemoteExecutor) Execute(ctx context.Context, action *failoverv1alpha1.FailoverAction, namespace string, namePrefix string, actionIndex int) (*Result, error) {
	spec := action.WaitRemote
	if e.ClientProvider == nil {
		return &Result{Completed: true, Succeeded: false, Message: "remote client provider not configured"}, nil
	}

	connectTimeout := defaultConnectTimeout
	if spec.ConnectTimeoutSeconds != nil {
		connectTimeout = time.Duration(*spec.ConnectTimeoutSeconds) * time.Second
	}

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	remoteClient, err := e.ClientProvider.GetClient(connectCtx)
	if err != nil {
		// Return a soft failure so the controller-level timeout (actionTimeoutSeconds)
		// governs rather than retry-count exhaustion on transient connectivity errors.
		return &Result{Completed: false, Message: fmt.Sprintf("connecting to remote cluster: %v", err)}, nil
	}

	ownerRef := OwnerRefFromContext(ctx)
	if ownerRef == nil {
		return &Result{Completed: true, Succeeded: false, Message: "cannot determine FailoverService name for remote lookup"}, nil
	}

	remote := &failoverv1alpha1.FailoverService{}
	if err := remoteClient.Get(connectCtx, types.NamespacedName{
		Name:      ownerRef.Name,
		Namespace: namespace,
	}, remote); err != nil {
		// Soft failure — let the controller-level timeout handle this.
		return &Result{Completed: false, Message: fmt.Sprintf("getting remote FailoverService: %v", err)}, nil
	}

	// Get local transition start time for correlating with remote transitions
	localStartedAt, _ := TransitionStartedAtFromContext(ctx)
	localDirection, _ := TransitionDirectionFromContext(ctx)

	if spec.Phase != nil {
		return e.checkPhase(connectCtx, remoteClient, remote, namespace, ownerRef.Name, *spec.Phase, localStartedAt, localDirection), nil
	}

	if spec.ActionName != nil {
		return e.checkActionName(connectCtx, remoteClient, remote, namespace, ownerRef.Name, *spec.ActionName, localStartedAt, localDirection), nil
	}

	return &Result{Completed: true, Succeeded: false, Message: "neither phase nor actionName specified"}, nil
}

func (e *WaitRemoteExecutor) checkPhase(
	ctx context.Context,
	remoteClient client.Client,
	remote *failoverv1alpha1.FailoverService,
	namespace string,
	fsName string,
	targetPhase failoverv1alpha1.FailoverPhase,
	localStartedAt time.Time,
	expectedDirection string,
) *Result {
	remotePhase := failoverv1alpha1.PhaseIdle
	if remote.Status.Transition != nil {
		remotePhase = remote.Status.Transition.Phase
	}

	targetOrder := failoverv1alpha1.PhaseOrder(targetPhase)
	remoteOrder := failoverv1alpha1.PhaseOrder(remotePhase)

	// For idle target, remote must be idle AND have completed a transition after
	// the local one started. This prevents succeeding before the remote participates.
	if targetPhase == failoverv1alpha1.PhaseIdle {
		if remotePhase != failoverv1alpha1.PhaseIdle {
			if !transitionMatchesDirection(remote.Status.Transition, expectedDirection) {
				return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote %s transition to start (currently %s)", expectedDirection, remote.Status.Transition.TargetDirection)}
			}
			return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote to become idle (currently %s)", remotePhase)}
		}

		event, err := e.findRecentRemoteEvent(ctx, remoteClient, namespace, fsName, localStartedAt, expectedDirection)
		if err != nil {
			return &Result{Completed: false, Message: fmt.Sprintf("listing remote FailoverEvents: %v", err)}
		}
		if isTerminalRemoteEvent(event) {
			return &Result{Completed: true, Succeeded: true, Message: "Remote cluster is idle (completed matching transition after local transition started)"}
		}
		return &Result{Completed: false, Message: "Waiting for remote to participate in transition (currently idle)"}
	}

	// If remote is idle, it may have completed the transition or not started yet.
	// Correlate with the local transition start time to avoid false positives.
	if remotePhase == failoverv1alpha1.PhaseIdle {
		event, err := e.findRecentRemoteEvent(ctx, remoteClient, namespace, fsName, localStartedAt, expectedDirection)
		if err != nil {
			return &Result{Completed: false, Message: fmt.Sprintf("listing remote FailoverEvents: %v", err)}
		}
		if isTerminalRemoteEvent(event) {
			return &Result{Completed: true, Succeeded: true, Message: fmt.Sprintf("Remote cluster has completed matching transition (target was %s)", targetPhase)}
		}
		return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote to start transition (target phase: %s)", targetPhase)}
	}

	if !transitionMatchesDirection(remote.Status.Transition, expectedDirection) {
		return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote %s transition (currently %s)", expectedDirection, remote.Status.Transition.TargetDirection)}
	}

	// Remote is at or past target phase — but verify this transition started
	// after the local one to avoid matching a stale in-flight remote transition.
	if remoteOrder >= targetOrder {
		if e.remoteTransitionStartedAfter(remote, localStartedAt) {
			return &Result{Completed: true, Succeeded: true, Message: fmt.Sprintf("Remote cluster is at phase %s (target: %s)", remotePhase, targetPhase)}
		}
		return &Result{Completed: false, Message: fmt.Sprintf("Remote is at phase %s but transition predates local start; waiting for current transition", remotePhase)}
	}

	return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote to reach phase %s (currently %s)", targetPhase, remotePhase)}
}

// remoteTransitionStartedAfter checks whether the remote's current in-flight
// transition started after the local transition, to avoid matching a stale
// remote transition from before the local one began.
func (e *WaitRemoteExecutor) remoteTransitionStartedAfter(remote *failoverv1alpha1.FailoverService, localStartedAt time.Time) bool {
	if localStartedAt.IsZero() {
		return true // no local start time — optimistic
	}
	if remote.Status.Transition == nil || remote.Status.Transition.StartedAt == nil {
		return false
	}
	return !remote.Status.Transition.StartedAt.Time.Before(localStartedAt)
}

func (e *WaitRemoteExecutor) checkActionName(
	ctx context.Context,
	remoteClient client.Client,
	remote *failoverv1alpha1.FailoverService,
	namespace string,
	fsName string,
	actionName string,
	localStartedAt time.Time,
	expectedDirection string,
) *Result {
	if remote.Status.Transition == nil {
		// The in-flight status is cleared on completion, so use FailoverEvent as the
		// durable source of truth for completed remote actions.
		event, err := e.findRecentRemoteEvent(ctx, remoteClient, namespace, fsName, localStartedAt, expectedDirection)
		if err != nil {
			return &Result{Completed: false, Message: fmt.Sprintf("listing remote FailoverEvents: %v", err)}
		}

		if event == nil {
			return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote action %q (remote has no active transition)", actionName)}
		}

		if result, ok, ambiguous := findActionResult(event.Status.PreActionResults, event.Status.PostActionResults, actionName); ambiguous {
			return &Result{
				Completed: true,
				Succeeded: false,
				Message:   fmt.Sprintf("Remote action %q is ambiguous; action names must be unique", actionName),
			}
		} else if ok {
			return resultForAction(actionName, result)
		}

		// The remote transition already completed, and the named action was never
		// recorded in the durable event history. Waiting longer cannot satisfy this.
		if event.Status.Outcome != "" && event.Status.Outcome != failoverv1alpha1.EventOutcomeInProgress {
			return &Result{
				Completed: true,
				Succeeded: false,
				Message:   fmt.Sprintf("Remote transition completed without action %q", actionName),
			}
		}

		return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote action %q (remote event still in progress)", actionName)}
	}

	// Verify this remote transition correlates with the current local transition
	// to avoid matching action results from a stale remote transition.
	if !transitionMatchesDirection(remote.Status.Transition, expectedDirection) {
		return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote action %q on %s transition", actionName, expectedDirection)}
	}
	if !e.remoteTransitionStartedAfter(remote, localStartedAt) {
		return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote action %q (remote transition predates local start)", actionName)}
	}

	if result, ok, ambiguous := findActionResult(remote.Status.Transition.PreActionResults, remote.Status.Transition.PostActionResults, actionName); ambiguous {
		return &Result{
			Completed: true,
			Succeeded: false,
			Message:   fmt.Sprintf("Remote action %q is ambiguous; action names must be unique", actionName),
		}
	} else if ok {
		return resultForAction(actionName, result)
	}

	return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote action %q (not found in remote results)", actionName)}
}

func (e *WaitRemoteExecutor) findRecentRemoteEvent(
	ctx context.Context,
	remoteClient client.Client,
	namespace string,
	fsName string,
	localStartedAt time.Time,
	expectedDirection string,
) (*failoverv1alpha1.FailoverEvent, error) {
	events := &failoverv1alpha1.FailoverEventList{}
	if err := remoteClient.List(ctx, events, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	var best *failoverv1alpha1.FailoverEvent
	var bestTime time.Time
	for i := range events.Items {
		event := &events.Items[i]
		if event.Spec.FailoverServiceRef != fsName {
			continue
		}
		if !eventMatchesTransition(event, localStartedAt, expectedDirection) {
			continue
		}

		eventTime := eventTimestamp(event)
		if best == nil || eventTime.After(bestTime) {
			best = event.DeepCopy()
			bestTime = eventTime
		}
	}

	return best, nil
}

func eventMatchesTransition(event *failoverv1alpha1.FailoverEvent, localStartedAt time.Time, expectedDirection string) bool {
	if expectedDirection != "" && event.Spec.Direction != expectedDirection {
		return false
	}
	if localStartedAt.IsZero() {
		return true
	}
	if event.Status.StartedAt != nil {
		return !event.Status.StartedAt.Time.Before(localStartedAt)
	}
	if event.Status.CompletedAt != nil {
		return !event.Status.CompletedAt.Time.Before(localStartedAt)
	}
	return !event.CreationTimestamp.Time.Before(localStartedAt)
}

func isTerminalRemoteEvent(event *failoverv1alpha1.FailoverEvent) bool {
	return event != nil && event.Status.Outcome != "" && event.Status.Outcome != failoverv1alpha1.EventOutcomeInProgress
}

func transitionMatchesDirection(transition *failoverv1alpha1.TransitionStatus, expectedDirection string) bool {
	return expectedDirection == "" || (transition != nil && transition.TargetDirection == expectedDirection)
}

func eventTimestamp(event *failoverv1alpha1.FailoverEvent) time.Time {
	if event.Status.CompletedAt != nil {
		return event.Status.CompletedAt.Time
	}
	if event.Status.StartedAt != nil {
		return event.Status.StartedAt.Time
	}
	return event.CreationTimestamp.Time
}

func findActionResult(
	preResults []failoverv1alpha1.ActionResult,
	postResults []failoverv1alpha1.ActionResult,
	actionName string,
) (*failoverv1alpha1.ActionResult, bool, bool) {
	var match *failoverv1alpha1.ActionResult
	for i := range preResults {
		if preResults[i].Name == actionName {
			if match != nil {
				return nil, false, true
			}
			match = &preResults[i]
		}
	}
	for i := range postResults {
		if postResults[i].Name == actionName {
			if match != nil {
				return nil, false, true
			}
			match = &postResults[i]
		}
	}
	if match == nil {
		return nil, false, false
	}
	return match, true, false
}

func resultForAction(actionName string, actionResult *failoverv1alpha1.ActionResult) *Result {
	switch actionResult.Status {
	case failoverv1alpha1.ActionStatusSucceeded:
		return &Result{Completed: true, Succeeded: true, Message: fmt.Sprintf("Remote action %q succeeded", actionName)}
	case failoverv1alpha1.ActionStatusFailed:
		return &Result{Completed: true, Succeeded: false, Message: fmt.Sprintf("Remote action %q failed: %s", actionName, actionResult.Message)}
	case failoverv1alpha1.ActionStatusSkipped:
		return &Result{Completed: true, Succeeded: true, Message: fmt.Sprintf("Remote action %q was skipped", actionName)}
	default:
		return &Result{Completed: false, Message: fmt.Sprintf("Waiting for remote action %q (status: %s)", actionName, actionResult.Status)}
	}
}
