package action

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

type fakeRemoteClientProvider struct {
	client client.Client
	err    error
}

const promoteActionName = "promote"
const (
	directionFailover = "failover"
	directionFailback = "failback"
)

func (f *fakeRemoteClientProvider) GetClient(context.Context) (client.Client, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.client, nil
}

func waitRemoteScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = failoverv1alpha1.AddToScheme(s)
	return s
}

func waitRemoteContext(startedAt time.Time) context.Context {
	ctx := ContextWithOwnerRef(context.Background(), &metav1.OwnerReference{Name: "mysql"})
	ctx = ContextWithTransitionStartedAt(ctx, startedAt)
	return ContextWithTransitionDirection(ctx, directionFailover)
}

func TestWaitRemoteExecutor_ActionNameUsesCompletedEvent(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	eventStartedAt := metav1.NewTime(localStartedAt.Add(30 * time.Second))
	eventCompletedAt := metav1.NewTime(localStartedAt.Add(45 * time.Second))
	lastTransitionTime := metav1.NewTime(localStartedAt.Add(45 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			LastTransitionTime: &lastTransitionTime,
		},
	}
	event := &failoverv1alpha1.FailoverEvent{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql-failover-event", Namespace: "default"},
		Spec: failoverv1alpha1.FailoverEventSpec{
			FailoverServiceRef: "mysql",
			Direction:          directionFailover,
			TriggeredBy:        "remote-sync",
		},
		Status: failoverv1alpha1.FailoverEventStatus{
			StartedAt:   &eventStartedAt,
			CompletedAt: &eventCompletedAt,
			Outcome:     failoverv1alpha1.EventOutcomeSucceeded,
			PreActionResults: []failoverv1alpha1.ActionResult{
				{Name: "promote", Status: failoverv1alpha1.ActionStatusSucceeded},
			},
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote, event).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)
	actionName := promoteActionName

	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			ActionName: &actionName,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || !result.Succeeded {
		t.Fatalf("expected completed success, got %#v", result)
	}
}

func TestWaitRemoteExecutor_ActionNameFailsWhenCompletedEventMissingAction(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	eventStartedAt := metav1.NewTime(localStartedAt.Add(30 * time.Second))
	eventCompletedAt := metav1.NewTime(localStartedAt.Add(45 * time.Second))
	lastTransitionTime := metav1.NewTime(localStartedAt.Add(45 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			LastTransitionTime: &lastTransitionTime,
		},
	}
	event := &failoverv1alpha1.FailoverEvent{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql-failover-event", Namespace: "default"},
		Spec: failoverv1alpha1.FailoverEventSpec{
			FailoverServiceRef: "mysql",
			Direction:          directionFailover,
			TriggeredBy:        "remote-sync",
		},
		Status: failoverv1alpha1.FailoverEventStatus{
			StartedAt:   &eventStartedAt,
			CompletedAt: &eventCompletedAt,
			Outcome:     failoverv1alpha1.EventOutcomeSucceeded,
			PreActionResults: []failoverv1alpha1.ActionResult{
				{Name: "other-action", Status: failoverv1alpha1.ActionStatusSucceeded},
			},
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote, event).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)
	actionName := promoteActionName

	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			ActionName: &actionName,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || result.Succeeded {
		t.Fatalf("expected terminal failure, got %#v", result)
	}
	if !strings.Contains(result.Message, `completed without action "promote"`) {
		t.Fatalf("unexpected message: %q", result.Message)
	}
}

func TestWaitRemoteExecutor_ActionNameUsesActiveTransitionResults(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	remoteStartedAt := metav1.NewTime(localStartedAt.Add(30 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			Transition: &failoverv1alpha1.TransitionStatus{
				StartedAt:       &remoteStartedAt,
				TargetDirection: directionFailover,
				PreActionResults: []failoverv1alpha1.ActionResult{
					{Name: "promote", Status: failoverv1alpha1.ActionStatusSucceeded},
				},
			},
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)
	actionName := promoteActionName

	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			ActionName: &actionName,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || !result.Succeeded {
		t.Fatalf("expected completed success, got %#v", result)
	}
}

func TestWaitRemoteExecutor_ActionNameFailsOnDuplicateMatches(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	remoteStartedAt := metav1.NewTime(localStartedAt.Add(30 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			Transition: &failoverv1alpha1.TransitionStatus{
				StartedAt:       &remoteStartedAt,
				TargetDirection: directionFailover,
				PreActionResults: []failoverv1alpha1.ActionResult{
					{Name: promoteActionName, Status: failoverv1alpha1.ActionStatusSucceeded},
				},
				PostActionResults: []failoverv1alpha1.ActionResult{
					{Name: promoteActionName, Status: failoverv1alpha1.ActionStatusSucceeded},
				},
			},
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)
	actionName := promoteActionName

	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			ActionName: &actionName,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || result.Succeeded {
		t.Fatalf("expected terminal ambiguity failure, got %#v", result)
	}
	if !strings.Contains(result.Message, "ambiguous") {
		t.Fatalf("unexpected message: %q", result.Message)
	}
}

func TestWaitRemoteExecutor_PhaseIdleRequiresRecentCompletion(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	lastTransitionTime := metav1.NewTime(localStartedAt.Add(20 * time.Second))
	eventStartedAt := metav1.NewTime(localStartedAt.Add(10 * time.Second))
	eventCompletedAt := metav1.NewTime(localStartedAt.Add(20 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			LastTransitionTime: &lastTransitionTime,
		},
	}
	event := &failoverv1alpha1.FailoverEvent{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql-failover-event", Namespace: "default"},
		Spec: failoverv1alpha1.FailoverEventSpec{
			FailoverServiceRef: "mysql",
			Direction:          directionFailover,
			TriggeredBy:        "remote-sync",
		},
		Status: failoverv1alpha1.FailoverEventStatus{
			StartedAt:   &eventStartedAt,
			CompletedAt: &eventCompletedAt,
			Outcome:     failoverv1alpha1.EventOutcomeSucceeded,
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote, event).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)

	idle := failoverv1alpha1.PhaseIdle
	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			Phase: &idle,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Completed || !result.Succeeded {
		t.Fatalf("expected completed success, got %#v", result)
	}
}

func TestWaitRemoteExecutor_PhaseIdleDoesNotUseLastTransitionTimeWithoutMatchingEvent(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	lastTransitionTime := metav1.NewTime(localStartedAt.Add(20 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			LastTransitionTime: &lastTransitionTime,
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)

	idle := failoverv1alpha1.PhaseIdle
	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			Phase: &idle,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Fatalf("expected still waiting without matching event, got %#v", result)
	}
	if !strings.Contains(result.Message, "participate") {
		t.Fatalf("unexpected message: %q", result.Message)
	}
}

func TestWaitRemoteExecutor_PhaseRejectsPredatingTransition(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	remoteStartedAt := metav1.NewTime(localStartedAt.Add(-30 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			Transition: &failoverv1alpha1.TransitionStatus{
				StartedAt:       &remoteStartedAt,
				Phase:           failoverv1alpha1.PhaseUpdatingResources,
				TargetDirection: directionFailover,
			},
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)

	targetPhase := failoverv1alpha1.PhaseExecutingPreActions
	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			Phase: &targetPhase,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Fatalf("expected still waiting, got %#v", result)
	}
	if !strings.Contains(result.Message, "predates local start") {
		t.Fatalf("unexpected message: %q", result.Message)
	}
}

func TestWaitRemoteExecutor_PhaseRejectsOppositeDirectionTransition(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	remoteStartedAt := metav1.NewTime(localStartedAt.Add(30 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			Transition: &failoverv1alpha1.TransitionStatus{
				StartedAt:       &remoteStartedAt,
				Phase:           failoverv1alpha1.PhaseUpdatingResources,
				TargetDirection: directionFailback,
			},
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)

	targetPhase := failoverv1alpha1.PhaseExecutingPreActions
	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			Phase: &targetPhase,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Fatalf("expected still waiting for matching direction, got %#v", result)
	}
	if !strings.Contains(result.Message, directionFailover) {
		t.Fatalf("unexpected message: %q", result.Message)
	}
}

func TestWaitRemoteExecutor_ActionNameIgnoresOppositeDirectionEvent(t *testing.T) {
	localStartedAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	eventStartedAt := metav1.NewTime(localStartedAt.Add(30 * time.Second))
	eventCompletedAt := metav1.NewTime(localStartedAt.Add(45 * time.Second))
	lastTransitionTime := metav1.NewTime(localStartedAt.Add(45 * time.Second))

	remote := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "default"},
		Status: failoverv1alpha1.FailoverServiceStatus{
			LastTransitionTime: &lastTransitionTime,
		},
	}
	event := &failoverv1alpha1.FailoverEvent{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql-failback-event", Namespace: "default"},
		Spec: failoverv1alpha1.FailoverEventSpec{
			FailoverServiceRef: "mysql",
			Direction:          directionFailback,
			TriggeredBy:        "remote-sync",
		},
		Status: failoverv1alpha1.FailoverEventStatus{
			StartedAt:   &eventStartedAt,
			CompletedAt: &eventCompletedAt,
			Outcome:     failoverv1alpha1.EventOutcomeSucceeded,
			PreActionResults: []failoverv1alpha1.ActionResult{
				{Name: promoteActionName, Status: failoverv1alpha1.ActionStatusSucceeded},
			},
		},
	}

	remoteClient := fake.NewClientBuilder().WithScheme(waitRemoteScheme()).WithObjects(remote, event).Build()
	executor := &WaitRemoteExecutor{ClientProvider: &fakeRemoteClientProvider{client: remoteClient}}
	ctx := waitRemoteContext(localStartedAt)
	actionName := promoteActionName

	result, err := executor.Execute(ctx, &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			ActionName: &actionName,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Fatalf("expected still waiting for matching direction, got %#v", result)
	}
	if !strings.Contains(result.Message, "no active transition") {
		t.Fatalf("unexpected message: %q", result.Message)
	}
}

func TestWaitRemoteExecutor_RemoteClientErrorIsSoftFailure(t *testing.T) {
	executor := &WaitRemoteExecutor{
		ClientProvider: &fakeRemoteClientProvider{err: errors.New("remote down")},
	}
	actionName := promoteActionName

	result, err := executor.Execute(context.Background(), &failoverv1alpha1.FailoverAction{
		Name: "wait-remote",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			ActionName: &actionName,
		},
	}, "default", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Completed {
		t.Fatalf("expected soft failure, got %#v", result)
	}
}
