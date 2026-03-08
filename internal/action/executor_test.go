package action

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

// mockExecutor implements Executor for testing dispatch.
type mockExecutor struct {
	result *Result
	err    error
}

func (m *mockExecutor) Execute(_ context.Context, _ *failoverv1alpha1.FailoverAction, _ string, _ string, _ int) (*Result, error) {
	return m.result, m.err
}

// mockJobExecutor implements both Executor and CleanupExecutor.
type mockJobExecutor struct {
	result     *Result
	err        error
	cleanupErr error
}

func (m *mockJobExecutor) Execute(_ context.Context, _ *failoverv1alpha1.FailoverAction, _ string, _ string, _ int) (*Result, error) {
	return m.result, m.err
}

func (m *mockJobExecutor) Cleanup(_ context.Context, _ string, _ string) error {
	return m.cleanupErr
}

func TestMultiExecutor_HTTPDispatch(t *testing.T) {
	expected := &Result{Completed: true, Succeeded: true, Message: "http ok"}
	me := &MultiExecutor{
		HTTP: &mockExecutor{result: expected},
	}
	action := &failoverv1alpha1.FailoverAction{
		Name: "test",
		HTTP: &failoverv1alpha1.HTTPAction{URL: "http://example.com"},
	}

	result, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != expected {
		t.Errorf("expected result %v, got %v", expected, result)
	}
}

func TestMultiExecutor_JobDispatch(t *testing.T) {
	expected := &Result{Completed: true, Succeeded: true, Message: "job ok"}
	me := &MultiExecutor{
		Job: &mockJobExecutor{result: expected},
	}
	action := &failoverv1alpha1.FailoverAction{
		Name: "test",
		Job:  &failoverv1alpha1.JobAction{Image: "alpine"},
	}

	result, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != expected {
		t.Errorf("expected result %v, got %v", expected, result)
	}
}

func TestMultiExecutor_ScaleDispatch(t *testing.T) {
	expected := &Result{Completed: true, Succeeded: true, Message: "scale ok"}
	me := &MultiExecutor{
		Scale: &mockExecutor{result: expected},
	}
	action := &failoverv1alpha1.FailoverAction{
		Name:  "test",
		Scale: &failoverv1alpha1.ScaleAction{Kind: "Deployment", Name: "web", Replicas: 3},
	}

	result, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != expected {
		t.Errorf("expected result %v, got %v", expected, result)
	}
}

func TestMultiExecutor_WaitReadyDispatch(t *testing.T) {
	expected := &Result{Completed: true, Succeeded: true, Message: "ready"}
	me := &MultiExecutor{
		WaitReady: &mockExecutor{result: expected},
	}
	action := &failoverv1alpha1.FailoverAction{
		Name:      "test",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "web"},
	}

	result, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != expected {
		t.Errorf("expected result %v, got %v", expected, result)
	}
}

func TestMultiExecutor_WaitRemoteDispatch(t *testing.T) {
	expected := &Result{Completed: true, Succeeded: true, Message: "remote ready"}
	me := &MultiExecutor{
		WaitRemote: &mockExecutor{result: expected},
	}
	actionName := promoteActionName
	action := &failoverv1alpha1.FailoverAction{
		Name: "test",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			ActionName: &actionName,
		},
	}

	result, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != expected {
		t.Errorf("expected result %v, got %v", expected, result)
	}
}

func TestMultiExecutor_NoActionTypeSet(t *testing.T) {
	me := &MultiExecutor{}
	action := &failoverv1alpha1.FailoverAction{Name: "empty"}

	_, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err == nil {
		t.Fatal("expected error when no action type set")
	}
	if got := err.Error(); got != `action "empty" has no executor type set` {
		t.Errorf("unexpected error message: %s", got)
	}
}

func TestMultiExecutor_HTTPExecutorNil(t *testing.T) {
	me := &MultiExecutor{}
	action := &failoverv1alpha1.FailoverAction{
		Name: "http-nil",
		HTTP: &failoverv1alpha1.HTTPAction{URL: "http://example.com"},
	}

	_, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err == nil {
		t.Fatal("expected error for nil HTTP executor")
	}
	if got := err.Error(); got != `action "http-nil": HTTP executor not configured` {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestMultiExecutor_JobExecutorNil(t *testing.T) {
	me := &MultiExecutor{}
	action := &failoverv1alpha1.FailoverAction{
		Name: "job-nil",
		Job:  &failoverv1alpha1.JobAction{Image: "alpine"},
	}

	_, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err == nil {
		t.Fatal("expected error for nil Job executor")
	}
	if got := err.Error(); got != `action "job-nil": Job executor not configured` {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestMultiExecutor_ScaleExecutorNil(t *testing.T) {
	me := &MultiExecutor{}
	action := &failoverv1alpha1.FailoverAction{
		Name:  "scale-nil",
		Scale: &failoverv1alpha1.ScaleAction{Kind: "Deployment", Name: "web", Replicas: 1},
	}

	_, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err == nil {
		t.Fatal("expected error for nil Scale executor")
	}
	if got := err.Error(); got != `action "scale-nil": Scale executor not configured` {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestMultiExecutor_WaitReadyExecutorNil(t *testing.T) {
	me := &MultiExecutor{}
	action := &failoverv1alpha1.FailoverAction{
		Name:      "wr-nil",
		WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "web"},
	}

	_, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err == nil {
		t.Fatal("expected error for nil WaitReady executor")
	}
	if got := err.Error(); got != `action "wr-nil": WaitReady executor not configured` {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestMultiExecutor_WaitRemoteExecutorNil(t *testing.T) {
	me := &MultiExecutor{}
	actionName := promoteActionName
	action := &failoverv1alpha1.FailoverAction{
		Name: "wr-remote-nil",
		WaitRemote: &failoverv1alpha1.WaitRemoteAction{
			ActionName: &actionName,
		},
	}

	_, err := me.Execute(context.Background(), action, "ns", "prefix", 0)
	if err == nil {
		t.Fatal("expected error for nil WaitRemote executor")
	}
	if got := err.Error(); got != `action "wr-remote-nil": WaitRemote executor not configured` {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestTransitionStartedAtContextRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ctx := ContextWithTransitionStartedAt(context.Background(), now)

	got, ok := TransitionStartedAtFromContext(ctx)
	if !ok {
		t.Fatal("expected transition start time in context")
	}
	if !got.Equal(now) {
		t.Fatalf("expected %v, got %v", now, got)
	}
}

func TestOwnerRefContextRoundTrip(t *testing.T) {
	ref := &metav1.OwnerReference{Name: "fs"}
	ctx := ContextWithOwnerRef(context.Background(), ref)

	got := OwnerRefFromContext(ctx)
	if got == nil || got.Name != "fs" {
		t.Fatalf("expected owner ref fs, got %#v", got)
	}
}

func TestMultiExecutor_Cleanup(t *testing.T) {
	je := &mockJobExecutor{}
	me := &MultiExecutor{Job: je}

	err := me.Cleanup(context.Background(), "my-job", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMultiExecutor_Cleanup_Error(t *testing.T) {
	je := &mockJobExecutor{cleanupErr: fmt.Errorf("cleanup failed")}
	me := &MultiExecutor{Job: je}

	err := me.Cleanup(context.Background(), "my-job", "default")
	if err == nil {
		t.Fatal("expected cleanup error")
	}
}

func TestMultiExecutor_CleanupNilJob(t *testing.T) {
	me := &MultiExecutor{}

	err := me.Cleanup(context.Background(), "my-job", "default")
	if err == nil {
		t.Fatal("expected error when Job executor is nil")
	}
	if got := err.Error(); got != `cannot cleanup job "my-job": Job executor not configured` {
		t.Errorf("unexpected error: %s", got)
	}
}
