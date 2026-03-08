package action

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

type ownerRefKey struct{}
type transitionStartedAtKey struct{}
type transitionDirectionKey struct{}

// ContextWithOwnerRef attaches an OwnerReference to a context for use by action executors.
func ContextWithOwnerRef(ctx context.Context, ref *metav1.OwnerReference) context.Context {
	return context.WithValue(ctx, ownerRefKey{}, ref)
}

// OwnerRefFromContext retrieves the OwnerReference from a context, if present.
func OwnerRefFromContext(ctx context.Context) *metav1.OwnerReference {
	ref, _ := ctx.Value(ownerRefKey{}).(*metav1.OwnerReference)
	return ref
}

// ContextWithTransitionStartedAt attaches the local transition's start time to a context.
func ContextWithTransitionStartedAt(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, transitionStartedAtKey{}, t)
}

// TransitionStartedAtFromContext retrieves the local transition's start time from a context.
func TransitionStartedAtFromContext(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(transitionStartedAtKey{}).(time.Time)
	return t, ok
}

// ContextWithTransitionDirection attaches the local transition direction to a context.
func ContextWithTransitionDirection(ctx context.Context, direction string) context.Context {
	return context.WithValue(ctx, transitionDirectionKey{}, direction)
}

// TransitionDirectionFromContext retrieves the local transition direction from a context.
func TransitionDirectionFromContext(ctx context.Context) (string, bool) {
	direction, ok := ctx.Value(transitionDirectionKey{}).(string)
	return direction, ok
}

// Result represents the outcome of executing an action.
type Result struct {
	// Completed is true when the action is done (success or failure).
	// false means still running (requeue).
	Completed bool
	// Succeeded is true if the action completed successfully.
	Succeeded bool
	// Message contains additional details.
	Message string
	// JobName is the name of the Job created, if applicable.
	JobName string
}

// Executor defines the interface for executing a single action type.
type Executor interface {
	Execute(ctx context.Context, action *failoverv1alpha1.FailoverAction, namespace string, namePrefix string, actionIndex int) (*Result, error)
}

// CleanupExecutor defines the interface for cleaning up resources created by an action.
type CleanupExecutor interface {
	Cleanup(ctx context.Context, jobName string, namespace string) error
}

// MultiExecutor dispatches action execution to the appropriate executor
// based on which field is set.
type MultiExecutor struct {
	HTTP Executor
	Job  interface {
		Executor
		CleanupExecutor
	}
	Scale      Executor
	WaitReady  Executor
	WaitRemote Executor
}

// Execute dispatches to the appropriate executor based on the action type.
func (m *MultiExecutor) Execute(ctx context.Context, action *failoverv1alpha1.FailoverAction, namespace string, namePrefix string, actionIndex int) (*Result, error) {
	switch {
	case action.HTTP != nil:
		if m.HTTP == nil {
			return nil, fmt.Errorf("action %q: HTTP executor not configured", action.Name)
		}
		return m.HTTP.Execute(ctx, action, namespace, namePrefix, actionIndex)
	case action.Job != nil:
		if m.Job == nil {
			return nil, fmt.Errorf("action %q: Job executor not configured", action.Name)
		}
		return m.Job.Execute(ctx, action, namespace, namePrefix, actionIndex)
	case action.Scale != nil:
		if m.Scale == nil {
			return nil, fmt.Errorf("action %q: Scale executor not configured", action.Name)
		}
		return m.Scale.Execute(ctx, action, namespace, namePrefix, actionIndex)
	case action.WaitReady != nil:
		if m.WaitReady == nil {
			return nil, fmt.Errorf("action %q: WaitReady executor not configured", action.Name)
		}
		return m.WaitReady.Execute(ctx, action, namespace, namePrefix, actionIndex)
	case action.WaitRemote != nil:
		if m.WaitRemote == nil {
			return nil, fmt.Errorf("action %q: WaitRemote executor not configured", action.Name)
		}
		return m.WaitRemote.Execute(ctx, action, namespace, namePrefix, actionIndex)
	default:
		return nil, fmt.Errorf("action %q has no executor type set", action.Name)
	}
}

// Cleanup delegates cleanup to the job executor.
func (m *MultiExecutor) Cleanup(ctx context.Context, jobName string, namespace string) error {
	if m.Job == nil {
		return fmt.Errorf("cannot cleanup job %q: Job executor not configured", jobName)
	}
	return m.Job.Cleanup(ctx, jobName, namespace)
}
