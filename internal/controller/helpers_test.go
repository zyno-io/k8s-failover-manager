package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
	"github.com/zyno-io/k8s-failover-manager/internal/action"
	"github.com/zyno-io/k8s-failover-manager/internal/connkill"
	"github.com/zyno-io/k8s-failover-manager/internal/remotecluster"
	svcmanager "github.com/zyno-io/k8s-failover-manager/internal/service"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = failoverv1alpha1.AddToScheme(s)
	return s
}

func testFS() *failoverv1alpha1.FailoverService {
	return &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: failoverv1alpha1.FailoverServiceSpec{
			ServiceName:    "test-svc",
			FailoverActive: false,
			Ports:          []failoverv1alpha1.Port{{Name: "tcp", Port: 3306, Protocol: "TCP"}},
			PrimaryCluster: failoverv1alpha1.ClusterTarget{
				PrimaryModeAddress:  "primary.example.com",
				FailoverModeAddress: "10.0.0.1",
				OnFailover: &failoverv1alpha1.TransitionActions{
					PreActions:  []failoverv1alpha1.FailoverAction{{Name: "pre-fo"}},
					PostActions: []failoverv1alpha1.FailoverAction{{Name: "post-fo"}},
				},
				OnFailback: &failoverv1alpha1.TransitionActions{
					PreActions:  []failoverv1alpha1.FailoverAction{{Name: "pre-fb"}},
					PostActions: []failoverv1alpha1.FailoverAction{{Name: "post-fb"}},
				},
			},
			FailoverCluster: failoverv1alpha1.ClusterTarget{
				PrimaryModeAddress:  "10.0.0.2",
				FailoverModeAddress: "failover.example.com",
				OnFailover: &failoverv1alpha1.TransitionActions{
					PreActions: []failoverv1alpha1.FailoverAction{{Name: "fc-pre-fo"}},
				},
			},
		},
	}
}

type stubSyncer struct {
	result *remotecluster.SyncResult
	err    error
}

func (s *stubSyncer) Sync(_ context.Context, _ *failoverv1alpha1.FailoverService) (*remotecluster.SyncResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.result != nil {
		return s.result, nil
	}
	return &remotecluster.SyncResult{}, nil
}

type stubActionExecutor struct {
	result *action.Result
	err    error
}

func (s *stubActionExecutor) Execute(context.Context, *failoverv1alpha1.FailoverAction, string, string, int) (*action.Result, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

const (
	testOldTargetAddressIP = "10.10.10.10"
	testRemoteClusterID    = "cluster-remote"
	testPrimaryAddress     = "primary.example.com"
	testFailoverAddress    = "10.0.0.1"
)

// --- actionType tests ---

func TestActionType_HTTP(t *testing.T) {
	r := &FailoverServiceReconciler{}
	a := &failoverv1alpha1.FailoverAction{HTTP: &failoverv1alpha1.HTTPAction{URL: "http://x"}}
	if got := r.actionType(a); got != "http" {
		t.Errorf("expected 'http', got %q", got)
	}
}

func TestActionType_Job(t *testing.T) {
	r := &FailoverServiceReconciler{}
	a := &failoverv1alpha1.FailoverAction{Job: &failoverv1alpha1.JobAction{Image: "alpine"}}
	if got := r.actionType(a); got != "job" {
		t.Errorf("expected 'job', got %q", got)
	}
}

func TestActionType_Scale(t *testing.T) {
	r := &FailoverServiceReconciler{}
	a := &failoverv1alpha1.FailoverAction{Scale: &failoverv1alpha1.ScaleAction{Kind: "Deployment", Name: "w", Replicas: 1}}
	if got := r.actionType(a); got != "scale" {
		t.Errorf("expected 'scale', got %q", got)
	}
}

func TestActionType_WaitReady(t *testing.T) {
	r := &FailoverServiceReconciler{}
	a := &failoverv1alpha1.FailoverAction{WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "w"}}
	if got := r.actionType(a); got != "waitReady" {
		t.Errorf("expected 'waitReady', got %q", got)
	}
}

func TestActionType_Unknown(t *testing.T) {
	r := &FailoverServiceReconciler{}
	a := &failoverv1alpha1.FailoverAction{Name: "empty"}
	if got := r.actionType(a); got != "unknown" {
		t.Errorf("expected 'unknown', got %q", got)
	}
}

func TestActionRequeueInterval_UsesOverride(t *testing.T) {
	r := &FailoverServiceReconciler{}
	interval := int64(17)
	act := &failoverv1alpha1.FailoverAction{
		Name:                 "http",
		HTTP:                 &failoverv1alpha1.HTTPAction{URL: "http://example.com"},
		RetryIntervalSeconds: &interval,
	}

	if got := r.actionRequeueInterval(act); got != 17*time.Second {
		t.Fatalf("expected 17s, got %v", got)
	}
}

func TestActionMaxRetries_UsesOverride(t *testing.T) {
	r := &FailoverServiceReconciler{}
	retries := int32(9)
	act := &failoverv1alpha1.FailoverAction{
		Name:       "http",
		HTTP:       &failoverv1alpha1.HTTPAction{URL: "http://example.com"},
		MaxRetries: &retries,
	}

	if got := r.actionMaxRetries(act); got != 9 {
		t.Fatalf("expected 9 retries, got %d", got)
	}
}

// --- makeActionResults tests ---

func TestMakeActionResults(t *testing.T) {
	actions := []failoverv1alpha1.FailoverAction{
		{Name: "a1"},
		{Name: "a2"},
		{Name: "a3"},
	}
	results := makeActionResults(actions)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Name != actions[i].Name {
			t.Errorf("result[%d] name = %q, want %q", i, r.Name, actions[i].Name)
		}
		if r.Status != failoverv1alpha1.ActionStatusPending {
			t.Errorf("result[%d] status = %q, want Pending", i, r.Status)
		}
	}
}

func TestMakeActionResults_Empty(t *testing.T) {
	results := makeActionResults(nil)
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// --- getTransitionHooks tests ---

func TestGetTransitionHooks_PrimaryFailover(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()

	hooks := r.getTransitionHooks(fs, directionFailover)
	if hooks == nil {
		t.Fatal("expected non-nil hooks")
	}
	if len(hooks.PreActions) != 1 || hooks.PreActions[0].Name != "pre-fo" {
		t.Errorf("unexpected pre-actions: %+v", hooks.PreActions)
	}
}

func TestGetTransitionHooks_PrimaryFailback(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()

	hooks := r.getTransitionHooks(fs, directionFailback)
	if hooks == nil {
		t.Fatal("expected non-nil hooks")
	}
	if len(hooks.PreActions) != 1 || hooks.PreActions[0].Name != "pre-fb" {
		t.Errorf("unexpected pre-actions: %+v", hooks.PreActions)
	}
}

func TestGetTransitionHooks_FailoverRole(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: roleFailover}
	fs := testFS()

	hooks := r.getTransitionHooks(fs, directionFailover)
	if hooks == nil {
		t.Fatal("expected non-nil hooks")
	}
	if len(hooks.PreActions) != 1 || hooks.PreActions[0].Name != "fc-pre-fo" {
		t.Errorf("unexpected pre-actions: %+v", hooks.PreActions)
	}
}

// --- getOldTargetAddress tests ---

func TestGetOldTargetAddress_Failover(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		TargetDirection: directionFailover,
	}

	addr := r.getOldTargetAddress(fs)
	if addr != testPrimaryAddress {
		t.Errorf("expected PrimaryModeAddress, got %q", addr)
	}
}

func TestGetOldTargetAddress_Failback(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		TargetDirection: directionFailback,
	}

	addr := r.getOldTargetAddress(fs)
	if addr != testFailoverAddress {
		t.Errorf("expected FailoverModeAddress, got %q", addr)
	}
}

func TestGetOldTargetAddress_UsesSnapshotWhenPresent(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		OldAddressSnapshot: testOldTargetAddressIP,
	}

	addr := r.getOldTargetAddress(fs)
	if addr != testOldTargetAddressIP {
		t.Errorf("expected snapshot address %q, got %q", testOldTargetAddressIP, addr)
	}
}

func TestGetTransitionAddresses_Failover(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()

	oldAddr, newAddr := r.getTransitionAddresses(fs, directionFailover)
	if oldAddr != testPrimaryAddress {
		t.Errorf("expected old address primary.example.com, got %q", oldAddr)
	}
	if newAddr != testFailoverAddress {
		t.Errorf("expected new address 10.0.0.1, got %q", newAddr)
	}
}

func TestGetTransitionAddresses_Failback(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()

	oldAddr, newAddr := r.getTransitionAddresses(fs, directionFailback)
	if oldAddr != testFailoverAddress {
		t.Errorf("expected old address 10.0.0.1, got %q", oldAddr)
	}
	if newAddr != testPrimaryAddress {
		t.Errorf("expected new address primary.example.com, got %q", newAddr)
	}
}

// --- forceAdvanceActionPhase tests ---

func TestForceAdvanceActionPhase_SkipsCurrentAction(t *testing.T) {
	transition := &failoverv1alpha1.TransitionStatus{
		CurrentActionIndex: 0,
		PreActionResults: []failoverv1alpha1.ActionResult{
			{Name: "a1", Status: failoverv1alpha1.ActionStatusFailed},
			{Name: "a2", Status: failoverv1alpha1.ActionStatusPending},
		},
	}

	r := &FailoverServiceReconciler{}
	r.forceAdvanceActionPhase(transition, true)

	if transition.PreActionResults[0].Status != failoverv1alpha1.ActionStatusSkipped {
		t.Errorf("expected Skipped, got %q", transition.PreActionResults[0].Status)
	}
	if transition.PreActionResults[0].Message != "Force-advanced by operator" {
		t.Errorf("unexpected message: %q", transition.PreActionResults[0].Message)
	}
	if transition.CurrentActionIndex != 1 {
		t.Errorf("expected index 1, got %d", transition.CurrentActionIndex)
	}
}

func TestForceAdvanceActionPhase_OutOfBounds(t *testing.T) {
	transition := &failoverv1alpha1.TransitionStatus{
		CurrentActionIndex: 5,
		PreActionResults: []failoverv1alpha1.ActionResult{
			{Name: "a1", Status: failoverv1alpha1.ActionStatusSucceeded},
		},
	}

	r := &FailoverServiceReconciler{}
	// Should not panic
	r.forceAdvanceActionPhase(transition, true)

	// Index unchanged since out of bounds
	if transition.CurrentActionIndex != 5 {
		t.Errorf("expected index unchanged at 5, got %d", transition.CurrentActionIndex)
	}
}

// --- handleForceAdvance tests ---

func TestHandleForceAdvance_NilTransition(t *testing.T) {
	r := &FailoverServiceReconciler{}
	fs := testFS()
	fs.Status.Transition = nil

	modified := r.handleForceAdvance(context.TODO(), fs, "true")
	if modified {
		t.Error("expected false when transition is nil")
	}
}

func TestHandleForceAdvance_PreActions(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:              failoverv1alpha1.PhaseExecutingPreActions,
		TargetDirection:    directionFailover,
		CurrentActionIndex: 0,
		PreActionResults: []failoverv1alpha1.ActionResult{
			{Name: "a1", Status: failoverv1alpha1.ActionStatusFailed},
		},
	}

	modified := r.handleForceAdvance(context.TODO(), fs, "true")
	if !modified {
		t.Error("expected true")
	}
	if fs.Status.Transition.PreActionResults[0].Status != failoverv1alpha1.ActionStatusSkipped {
		t.Error("expected action to be skipped")
	}
}

func TestHandleForceAdvance_PostActions(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:              failoverv1alpha1.PhaseExecutingPostActions,
		TargetDirection:    directionFailover,
		CurrentActionIndex: 0,
		PostActionResults: []failoverv1alpha1.ActionResult{
			{Name: "p1", Status: failoverv1alpha1.ActionStatusFailed},
		},
	}

	modified := r.handleForceAdvance(context.TODO(), fs, "true")
	if !modified {
		t.Error("expected true")
	}
	if fs.Status.Transition.PostActionResults[0].Status != failoverv1alpha1.ActionStatusSkipped {
		t.Error("expected post-action to be skipped")
	}
}

func TestHandleForceAdvance_UpdatingResources(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:           failoverv1alpha1.PhaseUpdatingResources,
		TargetDirection: directionFailover,
	}

	modified := r.handleForceAdvance(context.TODO(), fs, "true")
	if !modified {
		t.Error("expected true")
	}
	if fs.Status.Transition.Phase != failoverv1alpha1.PhaseFlushingConnections {
		t.Errorf("expected FlushingConnections, got %q", fs.Status.Transition.Phase)
	}
}

func TestHandleForceAdvance_FlushingConnections(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterRole: rolePrimary}
	fs := testFS()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:           failoverv1alpha1.PhaseFlushingConnections,
		TargetDirection: directionFailover,
	}

	modified := r.handleForceAdvance(context.TODO(), fs, "true")
	if !modified {
		t.Error("expected true")
	}
	if fs.Status.Transition.Phase != failoverv1alpha1.PhaseExecutingPostActions {
		t.Errorf("expected ExecutingPostActions, got %q", fs.Status.Transition.Phase)
	}
}

func TestReconcileFlushingConnections_SkipsForNonIPAndAdvances(t *testing.T) {
	ctx := context.TODO()
	fs := testFS()
	fs.UID = "uid-1"
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:           failoverv1alpha1.PhaseFlushingConnections,
		TargetDirection: directionFailover, // old target = primaryModeAddress (DNS in testFS)
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs).
		WithStatusSubresource(fs).
		Build()

	r := &FailoverServiceReconciler{
		Client:      c,
		ClusterRole: rolePrimary,
		ClusterID:   "cluster-a",
	}

	current := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, current); err != nil {
		t.Fatalf("get fs: %v", err)
	}

	res, err := r.reconcileFlushingConnections(ctx, current)
	if err != nil {
		t.Fatalf("reconcileFlushingConnections error: %v", err)
	}
	_ = res

	updated := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated); err != nil {
		t.Fatalf("get updated fs: %v", err)
	}
	if updated.Status.Transition == nil || updated.Status.Transition.Phase != failoverv1alpha1.PhaseExecutingPostActions {
		t.Fatalf("expected transition phase ExecutingPostActions, got %#v", updated.Status.Transition)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, failoverv1alpha1.ConditionConnectionsKilled)
	if cond == nil || cond.Reason != "Skipped" {
		t.Fatalf("expected ConnectionsKilled condition reason Skipped, got %#v", cond)
	}
}

func TestReconcileFlushingConnections_AckTimeoutHalts(t *testing.T) {
	ctx := context.TODO()
	fs := testFS()
	fs.UID = "uid-2"
	fs.Spec.PrimaryCluster.PrimaryModeAddress = testOldTargetAddressIP
	old := metav1.NewTime(metav1.Now().Add(-2 * connKillTimeout))
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:           failoverv1alpha1.PhaseFlushingConnections,
		TargetDirection: directionFailover,
		ConnectionKill: &failoverv1alpha1.ConnectionKillStatus{
			SignalSent:    true,
			KillToken:     "tok-1",
			ExpectedNodes: []string{"node-a"},
			SignaledAt:    &old,
		},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      connkill.ConfigMapName,
			Namespace: "default",
		},
		Data: map[string]string{},
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs, cm).
		WithStatusSubresource(fs).
		Build()

	r := &FailoverServiceReconciler{
		Client:      c,
		ClusterRole: rolePrimary,
		ClusterID:   "cluster-a",
		ConnSignaler: &connkill.Signaler{
			Client:    c,
			Namespace: "default",
		},
	}

	current := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, current); err != nil {
		t.Fatalf("get fs: %v", err)
	}

	res, err := r.reconcileFlushingConnections(ctx, current)
	if err != nil {
		t.Fatalf("reconcileFlushingConnections error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("expected halt without requeue on ACK timeout, got %#v", res)
	}

	updated := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated); err != nil {
		t.Fatalf("get updated fs: %v", err)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, failoverv1alpha1.ConditionConnectionsKilled)
	if cond == nil || cond.Reason != "AckTimeout" {
		t.Fatalf("expected ConnectionsKilled reason AckTimeout, got %#v", cond)
	}
}

func TestReconcileFlushingConnections_NoReadyAgents_SkipsAndAdvances(t *testing.T) {
	ctx := context.TODO()
	fs := testFS()
	fs.UID = "uid-3"
	// Ensure old target is an IP so conn-kill logic is entered.
	fs.Spec.PrimaryCluster.PrimaryModeAddress = testOldTargetAddressIP
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:           failoverv1alpha1.PhaseFlushingConnections,
		TargetDirection: directionFailover,
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{corev1.LabelOSStable: "linux"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs, node).
		WithStatusSubresource(fs).
		Build()

	r := &FailoverServiceReconciler{
		Client:      c,
		ClusterRole: rolePrimary,
		ClusterID:   "cluster-a",
		ConnSignaler: &connkill.Signaler{
			Client:    c,
			Namespace: "default",
		},
	}

	current := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, current); err != nil {
		t.Fatalf("get fs: %v", err)
	}

	res, err := r.reconcileFlushingConnections(ctx, current)
	if err != nil {
		t.Fatalf("reconcileFlushingConnections error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected immediate requeue when advancing, got %#v", res)
	}

	updated := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated); err != nil {
		t.Fatalf("get updated fs: %v", err)
	}
	if updated.Status.Transition == nil || updated.Status.Transition.Phase != failoverv1alpha1.PhaseExecutingPostActions {
		t.Fatalf("expected transition phase ExecutingPostActions, got %#v", updated.Status.Transition)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, failoverv1alpha1.ConditionConnectionsKilled)
	if cond == nil || cond.Reason != "Skipped" {
		t.Fatalf("expected ConnectionsKilled condition reason Skipped, got %#v", cond)
	}
}

func TestReconcileFlushingConnections_WaitsForAcks(t *testing.T) {
	ctx := context.TODO()
	fs := testFS()
	fs.UID = "uid-4"
	fs.Spec.PrimaryCluster.PrimaryModeAddress = testOldTargetAddressIP
	now := metav1.Now()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:           failoverv1alpha1.PhaseFlushingConnections,
		TargetDirection: directionFailover,
		ConnectionKill: &failoverv1alpha1.ConnectionKillStatus{
			SignalSent:    true,
			KillToken:     "tok-pending",
			ExpectedNodes: []string{"node-a"},
			SignaledAt:    &now,
		},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      connkill.ConfigMapName,
			Namespace: "default",
		},
		Data: map[string]string{},
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs, cm).
		WithStatusSubresource(fs).
		Build()

	r := &FailoverServiceReconciler{
		Client:      c,
		ClusterRole: rolePrimary,
		ClusterID:   "cluster-a",
		ConnSignaler: &connkill.Signaler{
			Client:    c,
			Namespace: "default",
		},
	}

	current := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, current); err != nil {
		t.Fatalf("get fs: %v", err)
	}

	res, err := r.reconcileFlushingConnections(ctx, current)
	if err != nil {
		t.Fatalf("reconcileFlushingConnections error: %v", err)
	}
	if res.RequeueAfter != connKillPollInterval {
		t.Fatalf("expected requeue after %v, got %#v", connKillPollInterval, res)
	}

	updated := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated); err != nil {
		t.Fatalf("get updated fs: %v", err)
	}
	if updated.Status.Transition == nil || updated.Status.Transition.Phase != failoverv1alpha1.PhaseFlushingConnections {
		t.Fatalf("expected to stay in FlushingConnections, got %#v", updated.Status.Transition)
	}
}

func TestReconcileFlushingConnections_AllAcksAdvances(t *testing.T) {
	ctx := context.TODO()
	fs := testFS()
	fs.UID = "uid-5"
	fs.Spec.PrimaryCluster.PrimaryModeAddress = testOldTargetAddressIP
	now := metav1.Now()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:           failoverv1alpha1.PhaseFlushingConnections,
		TargetDirection: directionFailover,
		ConnectionKill: &failoverv1alpha1.ConnectionKillStatus{
			SignalSent:    true,
			KillToken:     "tok-acked",
			ExpectedNodes: []string{"node-a"},
			SignaledAt:    &now,
		},
	}

	ackData := `{"seq":1,"token":"tok-acked","killed":3,"errors":0}`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      connkill.ConfigMapName,
			Namespace: "default",
		},
		Data: map[string]string{
			"ack_node-a_default_test": ackData,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs, cm).
		WithStatusSubresource(fs).
		Build()

	r := &FailoverServiceReconciler{
		Client:      c,
		ClusterRole: rolePrimary,
		ClusterID:   "cluster-a",
		ConnSignaler: &connkill.Signaler{
			Client:    c,
			Namespace: "default",
		},
	}

	current := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, current); err != nil {
		t.Fatalf("get fs: %v", err)
	}

	res, err := r.reconcileFlushingConnections(ctx, current)
	if err != nil {
		t.Fatalf("reconcileFlushingConnections error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Fatalf("expected immediate requeue after successful ACKs, got %#v", res)
	}

	updated := &failoverv1alpha1.FailoverService{}
	if err := c.Get(ctx, types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated); err != nil {
		t.Fatalf("get updated fs: %v", err)
	}
	if updated.Status.Transition == nil || updated.Status.Transition.Phase != failoverv1alpha1.PhaseExecutingPostActions {
		t.Fatalf("expected transition phase ExecutingPostActions, got %#v", updated.Status.Transition)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, failoverv1alpha1.ConditionConnectionsKilled)
	if cond == nil || cond.Reason != "AllNodesAcked" {
		t.Fatalf("expected ConnectionsKilled condition reason AllNodesAcked, got %#v", cond)
	}
}

// --- NewReconcilerFromEnv tests ---

func TestNewReconcilerFromEnv_RequiresClusterID(t *testing.T) {
	t.Setenv("CLUSTER_ROLE", "primary")
	t.Setenv("CLUSTER_ID", "")
	t.Setenv("REMOTE_KUBECONFIG_SECRET", "my-secret")
	t.Setenv("REMOTE_KUBECONFIG_NAMESPACE", "my-ns")

	_, err := NewReconcilerFromEnv(nil, testScheme())
	if err == nil {
		t.Fatal("expected error when CLUSTER_ID is empty")
	}
}

func TestNewReconcilerFromEnv_RequiresRemoteSecret(t *testing.T) {
	t.Setenv("CLUSTER_ROLE", "primary")
	t.Setenv("CLUSTER_ID", "test-cluster")
	t.Setenv("REMOTE_KUBECONFIG_SECRET", "")
	t.Setenv("REMOTE_KUBECONFIG_NAMESPACE", "my-ns")

	_, err := NewReconcilerFromEnv(nil, testScheme())
	if err == nil {
		t.Fatal("expected error when REMOTE_KUBECONFIG_SECRET is empty")
	}
}

func TestNewReconcilerFromEnv_RequiresRemoteNamespace(t *testing.T) {
	t.Setenv("CLUSTER_ROLE", "primary")
	t.Setenv("CLUSTER_ID", "test-cluster")
	t.Setenv("REMOTE_KUBECONFIG_SECRET", "my-secret")
	t.Setenv("REMOTE_KUBECONFIG_NAMESPACE", "")

	_, err := NewReconcilerFromEnv(nil, testScheme())
	if err == nil {
		t.Fatal("expected error when REMOTE_KUBECONFIG_NAMESPACE is empty")
	}
}

func TestNewReconcilerFromEnv_Defaults(t *testing.T) {
	t.Setenv("CLUSTER_ROLE", "")
	t.Setenv("CLUSTER_ID", "test-cluster")
	t.Setenv("REMOTE_KUBECONFIG_SECRET", "my-secret")
	t.Setenv("REMOTE_KUBECONFIG_NAMESPACE", "my-ns")

	r, err := NewReconcilerFromEnv(nil, testScheme())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ClusterRole != "primary" {
		t.Errorf("expected default ClusterRole=primary, got %q", r.ClusterRole)
	}
	if r.ClusterID != "test-cluster" {
		t.Errorf("expected ClusterID=test-cluster, got %q", r.ClusterID)
	}
}

func TestNewReconcilerFromEnv_InvalidRole(t *testing.T) {
	t.Setenv("CLUSTER_ROLE", "invalid")

	_, err := NewReconcilerFromEnv(nil, testScheme())
	if err == nil {
		t.Fatal("expected error for invalid CLUSTER_ROLE")
	}
}

// TestNewReconcilerFromEnv_RequiresClusterID above covers this case now that CLUSTER_ID is always required.

func TestApplyRemoteWinnerState_AdoptsRemoteRevision(t *testing.T) {
	const clusterLocal = "cluster-local"
	r := &FailoverServiceReconciler{ClusterID: clusterLocal}
	fs := testFS()
	fs.Status.ActiveGeneration = 2
	fs.Status.ActiveCluster = clusterLocal

	prev := false
	remoteSpecChangeTime := metav1.NewTime(time.Unix(200, 0))
	remote := &failoverv1alpha1.RemoteState{
		FailoverActive:   true,
		ActiveCluster:    testRemoteClusterID,
		ActiveGeneration: 9,
		SpecChangeTime:   &remoteSpecChangeTime,
	}

	r.applyRemoteWinnerState(fs, remote, prev)

	if fs.Status.ActiveGeneration != 9 {
		t.Fatalf("expected ActiveGeneration=9, got %d", fs.Status.ActiveGeneration)
	}
	if fs.Status.ActiveCluster != testRemoteClusterID {
		t.Fatalf("expected ActiveCluster=%s, got %q", testRemoteClusterID, fs.Status.ActiveCluster)
	}
	if fs.Status.LastKnownFailoverActive == nil {
		t.Fatal("expected LastKnownFailoverActive to be set")
	}
	if *fs.Status.LastKnownFailoverActive != prev {
		t.Fatalf("expected LastKnownFailoverActive=%v, got %v", prev, *fs.Status.LastKnownFailoverActive)
	}
	if fs.Status.RemoteState == nil || fs.Status.RemoteState.ActiveGeneration != 9 {
		t.Fatalf("expected RemoteState.ActiveGeneration=9, got %#v", fs.Status.RemoteState)
	}
	if fs.Status.LastTransitionTime != nil {
		t.Fatalf("expected LastTransitionTime to remain unset, got %#v", fs.Status.LastTransitionTime)
	}
	if fs.Status.SpecChangeTime == nil || !fs.Status.SpecChangeTime.Equal(&remoteSpecChangeTime) {
		t.Fatalf("expected SpecChangeTime=%v, got %#v", remoteSpecChangeTime, fs.Status.SpecChangeTime)
	}
}

func TestApplyRemoteWinnerState_HandlesNilRemoteState(t *testing.T) {
	const clusterLocal = "cluster-local"
	r := &FailoverServiceReconciler{ClusterID: clusterLocal}
	fs := testFS()
	fs.Status.ActiveGeneration = 5
	fs.Status.ActiveCluster = clusterLocal

	prev := true
	r.applyRemoteWinnerState(fs, nil, prev)

	if fs.Status.ActiveGeneration != 5 {
		t.Fatalf("expected ActiveGeneration unchanged, got %d", fs.Status.ActiveGeneration)
	}
	if fs.Status.ActiveCluster != clusterLocal {
		t.Fatalf("expected ActiveCluster unchanged, got %q", fs.Status.ActiveCluster)
	}
	if fs.Status.LastKnownFailoverActive == nil || *fs.Status.LastKnownFailoverActive != prev {
		t.Fatalf("expected LastKnownFailoverActive=%v, got %#v", prev, fs.Status.LastKnownFailoverActive)
	}
	if fs.Status.LastTransitionTime != nil {
		t.Fatalf("expected LastTransitionTime unchanged, got %#v", fs.Status.LastTransitionTime)
	}
}

// --- Reconcile dispatch tests ---

func TestReconcile_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	r := &FailoverServiceReconciler{
		Client:      c,
		Scheme:      testScheme(),
		ClusterRole: rolePrimary,
		ClusterID:   "test",
	}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "missing", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Error("expected no requeue for not-found resource")
	}
}

func TestReconcile_AddsFinalizer(t *testing.T) {
	fs := testFS()
	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs).
		WithStatusSubresource(fs).
		Build()
	r := &FailoverServiceReconciler{
		Client:         c,
		Scheme:         testScheme(),
		ClusterRole:    rolePrimary,
		ClusterID:      "test",
		ServiceManager: &svcmanager.Manager{Client: c, ClusterRole: rolePrimary},
		Syncer:         &stubSyncer{},
	}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	found := false
	for _, f := range updated.Finalizers {
		if f == finalizerName {
			found = true
		}
	}
	if !found {
		t.Error("expected finalizer to be added")
	}
}

func TestReconcile_UnknownPhaseResetsTransition(t *testing.T) {
	fs := testFS()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase: failoverv1alpha1.FailoverPhase("CorruptedPhase"),
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs).
		WithStatusSubresource(fs).
		Build()
	r := &FailoverServiceReconciler{
		Client:         c,
		Scheme:         testScheme(),
		ClusterRole:    rolePrimary,
		ClusterID:      "test",
		ServiceManager: &svcmanager.Manager{Client: c, ClusterRole: rolePrimary},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &failoverv1alpha1.FailoverService{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated); err != nil {
		t.Fatalf("get updated fs: %v", err)
	}
	if updated.Status.Transition != nil {
		t.Fatalf("expected transition to be reset, got %#v", updated.Status.Transition)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, failoverv1alpha1.ConditionActionFailed)
	if cond == nil || cond.Reason != "UnknownPhase" {
		t.Fatalf("expected ActionFailed condition reason UnknownPhase, got %#v", cond)
	}
}

// --- handleActionState tests ---

func TestHandleActionState_Succeeded(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-has"
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "a1", Status: failoverv1alpha1.ActionStatusSucceeded}},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	handled, res, err := r.handleActionState(context.Background(), fs, &failoverv1alpha1.FailoverAction{Name: "a1"}, &transition.PreActionResults[0], transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !handled || res != (ctrl.Result{Requeue: true}) {
		t.Error("expected handled=true, Requeue=true")
	}
	if transition.CurrentActionIndex != 1 {
		t.Errorf("expected index 1, got %d", transition.CurrentActionIndex)
	}
}

func TestHandleActionState_FailedIgnoreFailure(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-hafi"
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "a1", Status: failoverv1alpha1.ActionStatusFailed}},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	act := &failoverv1alpha1.FailoverAction{Name: "a1", IgnoreFailure: true}
	result := &transition.PreActionResults[0]
	handled, _, err := r.handleActionState(context.Background(), fs, act, result, transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !handled {
		t.Error("expected handled=true")
	}
	if result.Status != failoverv1alpha1.ActionStatusSkipped {
		t.Errorf("expected Skipped, got %q", result.Status)
	}
	if transition.CurrentActionIndex != 1 {
		t.Errorf("expected index 1, got %d", transition.CurrentActionIndex)
	}
}

func TestHandleActionState_FailedHalts(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-hafh"
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		EventName:          "event-hafh",
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "a1", Status: failoverv1alpha1.ActionStatusFailed, Message: "err"}},
	}
	fs.Status.Transition = transition
	event := &failoverv1alpha1.FailoverEvent{
		ObjectMeta: metav1.ObjectMeta{Name: "event-hafh", Namespace: fs.Namespace},
		Status: failoverv1alpha1.FailoverEventStatus{
			Outcome: failoverv1alpha1.EventOutcomeInProgress,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs, event).
		WithStatusSubresource(fs, event).
		Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	handled, res, err := r.handleActionState(context.Background(), fs, &failoverv1alpha1.FailoverAction{Name: "a1"}, &transition.PreActionResults[0], transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !handled {
		t.Error("expected handled=true")
	}
	if res != (ctrl.Result{}) {
		t.Error("expected no requeue (halt)")
	}
	cond := meta.FindStatusCondition(fs.Status.Conditions, failoverv1alpha1.ConditionActionFailed)
	if cond == nil {
		t.Error("expected ActionFailed condition")
	}
	updatedEvent := &failoverv1alpha1.FailoverEvent{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "event-hafh", Namespace: fs.Namespace}, updatedEvent); err != nil {
		t.Fatalf("get FailoverEvent: %v", err)
	}
	if updatedEvent.Status.Outcome != failoverv1alpha1.EventOutcomeFailed {
		t.Fatalf("expected event outcome Failed, got %q", updatedEvent.Status.Outcome)
	}
}

func TestHandleActionState_PendingToRunning(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-hapt"
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "a1", Status: failoverv1alpha1.ActionStatusPending}},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	result := &transition.PreActionResults[0]
	handled, res, err := r.handleActionState(context.Background(), fs, &failoverv1alpha1.FailoverAction{Name: "a1"}, result, transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !handled || res != (ctrl.Result{Requeue: true}) {
		t.Error("expected handled=true, Requeue=true")
	}
	if result.Status != failoverv1alpha1.ActionStatusRunning {
		t.Errorf("expected Running, got %q", result.Status)
	}
	if result.StartedAt == nil {
		t.Error("expected StartedAt to be set")
	}
}

func TestHandleActionState_WaitReadyTimeout(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-hawrt"
	startedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	timeout := int64(60)
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "w1", Status: failoverv1alpha1.ActionStatusRunning, StartedAt: &startedAt}},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	act := &failoverv1alpha1.FailoverAction{Name: "w1", WaitReady: &failoverv1alpha1.WaitReadyAction{Kind: "Deployment", Name: "web", TimeoutSeconds: &timeout}}
	result := &transition.PreActionResults[0]
	handled, _, err := r.handleActionState(context.Background(), fs, act, result, transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !handled {
		t.Error("expected handled=true for timeout")
	}
	if result.Status != failoverv1alpha1.ActionStatusFailed {
		t.Errorf("expected Failed, got %q", result.Status)
	}
}

// --- runAction tests ---

func TestRunAction_JobCompleted(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ra1"
	now := metav1.Now()
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		TransitionID:       "t1",
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "j1", Status: failoverv1alpha1.ActionStatusRunning, StartedAt: &now}},
	}
	fs.Status.Transition = transition
	jobObj := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-failover-t1-action-0", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "a", Image: "alpine"}}}}},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs, jobObj).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ActionExecutor: &action.MultiExecutor{Job: &action.JobExecutor{Client: c}},
	}
	act := &failoverv1alpha1.FailoverAction{Name: "j1", Job: &failoverv1alpha1.JobAction{Image: "alpine"}}
	result := &transition.PreActionResults[0]
	_, err := r.runAction(context.Background(), fs, act, result, transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.Status != failoverv1alpha1.ActionStatusSucceeded {
		t.Errorf("expected Succeeded, got %q", result.Status)
	}
	if transition.CurrentActionIndex != 1 {
		t.Errorf("expected index 1, got %d", transition.CurrentActionIndex)
	}
}

func TestRunAction_JobFailed(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ra2"
	now := metav1.Now()
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		TransitionID:       "t2",
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "j1", Status: failoverv1alpha1.ActionStatusRunning, StartedAt: &now}},
	}
	fs.Status.Transition = transition
	jobObj := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-failover-t2-action-0", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "a", Image: "alpine"}}}}},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs, jobObj).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ActionExecutor: &action.MultiExecutor{Job: &action.JobExecutor{Client: c}},
	}
	act := &failoverv1alpha1.FailoverAction{Name: "j1", Job: &failoverv1alpha1.JobAction{Image: "alpine"}}
	result := &transition.PreActionResults[0]
	_, err := r.runAction(context.Background(), fs, act, result, transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.Status != failoverv1alpha1.ActionStatusFailed {
		t.Errorf("expected Failed, got %q", result.Status)
	}
}

func TestRunAction_JobStillRunning(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ra3"
	now := metav1.Now()
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		TransitionID:       "t3",
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "j1", Status: failoverv1alpha1.ActionStatusRunning, StartedAt: &now}},
	}
	fs.Status.Transition = transition
	jobObj := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-failover-t3-action-0", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "a", Image: "alpine"}}}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs, jobObj).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ActionExecutor: &action.MultiExecutor{Job: &action.JobExecutor{Client: c}},
	}
	act := &failoverv1alpha1.FailoverAction{Name: "j1", Job: &failoverv1alpha1.JobAction{Image: "alpine"}}
	result := &transition.PreActionResults[0]
	res, err := r.runAction(context.Background(), fs, act, result, transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.Status != failoverv1alpha1.ActionStatusRunning {
		t.Errorf("expected still Running, got %q", result.Status)
	}
	if res.RequeueAfter != jobRequeueInterval {
		t.Errorf("expected requeue after %v, got %v", jobRequeueInterval, res.RequeueAfter)
	}
}

func TestRunAction_ExecutorErrorRetries(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ra4"
	now := metav1.Now()
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		TransitionID:       "t4",
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "h1", Status: failoverv1alpha1.ActionStatusRunning, StartedAt: &now}},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ActionExecutor: &action.MultiExecutor{
			HTTP: &stubActionExecutor{err: errors.New("dial tcp timeout")},
		},
	}
	act := &failoverv1alpha1.FailoverAction{Name: "h1", HTTP: &failoverv1alpha1.HTTPAction{URL: "http://example.com"}}
	result := &transition.PreActionResults[0]
	res, err := r.runAction(context.Background(), fs, act, result, transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res.RequeueAfter != defaultRequeueInterval {
		t.Fatalf("expected retry requeue after %v, got %#v", defaultRequeueInterval, res)
	}
	if result.RetryCount != 1 {
		t.Fatalf("expected RetryCount=1, got %d", result.RetryCount)
	}
	if result.Status != failoverv1alpha1.ActionStatusRunning {
		t.Fatalf("expected status to remain Running, got %q", result.Status)
	}
}

func TestRunAction_ExecutorErrorExceedsMaxRetries(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ra5"
	now := metav1.Now()
	transition := &failoverv1alpha1.TransitionStatus{
		TargetDirection:    directionFailover,
		TransitionID:       "t5",
		CurrentActionIndex: 0,
		PreActionResults: []failoverv1alpha1.ActionResult{
			{Name: "h1", Status: failoverv1alpha1.ActionStatusRunning, StartedAt: &now, RetryCount: defaultMaxRetries},
		},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ActionExecutor: &action.MultiExecutor{
			HTTP: &stubActionExecutor{err: errors.New("dial tcp timeout")},
		},
	}
	act := &failoverv1alpha1.FailoverAction{Name: "h1", HTTP: &failoverv1alpha1.HTTPAction{URL: "http://example.com"}}
	result := &transition.PreActionResults[0]
	res, err := r.runAction(context.Background(), fs, act, result, transition)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Fatalf("expected immediate requeue after terminal failure, got %#v", res)
	}
	if result.RetryCount != defaultMaxRetries+1 {
		t.Fatalf("expected RetryCount=%d, got %d", defaultMaxRetries+1, result.RetryCount)
	}
	if result.Status != failoverv1alpha1.ActionStatusFailed {
		t.Fatalf("expected status Failed, got %q", result.Status)
	}
	if result.CompletedAt == nil {
		t.Fatal("expected CompletedAt to be set for terminal retry failure")
	}
}

// --- executeActionPhase tests ---

func TestExecuteActionPhase_AllDoneAdvances(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-eap1"
	transition := &failoverv1alpha1.TransitionStatus{
		Phase:              failoverv1alpha1.PhaseExecutingPreActions,
		TargetDirection:    directionFailover,
		CurrentActionIndex: 1,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "pre-fo", Status: failoverv1alpha1.ActionStatusSucceeded}},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	res, err := r.executeActionPhase(context.Background(), fs, true)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Error("expected Requeue=true")
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.Transition.Phase != failoverv1alpha1.PhaseUpdatingResources {
		t.Errorf("expected UpdatingResources, got %q", updated.Status.Transition.Phase)
	}
}

func TestExecuteActionPhase_PostDoneCompletes(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-eap2"
	now := metav1.Now()
	transition := &failoverv1alpha1.TransitionStatus{
		Phase:              failoverv1alpha1.PhaseExecutingPostActions,
		TargetDirection:    directionFailover,
		TransitionID:       "txn-eap2",
		StartedAt:          &now,
		CurrentActionIndex: 1,
		PostActionResults:  []failoverv1alpha1.ActionResult{{Name: "post-fo", Status: failoverv1alpha1.ActionStatusSucceeded}},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ActionExecutor: &action.MultiExecutor{Job: &action.JobExecutor{Client: c}},
		Syncer:         &stubSyncer{},
	}

	res, err := r.executeActionPhase(context.Background(), fs, false)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res.RequeueAfter != antiEntropyInterval {
		t.Errorf("expected antiEntropy requeue, got %v", res.RequeueAfter)
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.Transition != nil {
		t.Error("expected transition cleared after completion")
	}
}

func TestExecuteActionPhase_ResultsMismatch(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-eap3"
	transition := &failoverv1alpha1.TransitionStatus{
		Phase:              failoverv1alpha1.PhaseExecutingPreActions,
		TargetDirection:    directionFailover,
		CurrentActionIndex: 0,
		PreActionResults:   []failoverv1alpha1.ActionResult{},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	res, err := r.executeActionPhase(context.Background(), fs, true)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Error("expected Requeue=true")
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.Transition.Phase != failoverv1alpha1.PhaseUpdatingResources {
		t.Errorf("expected UpdatingResources on mismatch, got %q", updated.Status.Transition.Phase)
	}
}

func TestExecuteActionPhase_NegativeIndex(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-eap4"
	transition := &failoverv1alpha1.TransitionStatus{
		Phase:              failoverv1alpha1.PhaseExecutingPreActions,
		TargetDirection:    directionFailover,
		CurrentActionIndex: -1,
		PreActionResults:   []failoverv1alpha1.ActionResult{{Name: "pre-fo", Status: failoverv1alpha1.ActionStatusPending}},
	}
	fs.Status.Transition = transition
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	_, err := r.executeActionPhase(context.Background(), fs, true)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if transition.CurrentActionIndex < 0 {
		t.Error("expected negative index to be corrected")
	}
}

// --- completeTransition tests ---

func TestCompleteTransition_Basic(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ct1"
	now := metav1.Now()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		TargetDirection:  directionFailover,
		TransitionID:     "txn-ct",
		StartedAt:        &now,
		PreActionResults: []failoverv1alpha1.ActionResult{{Name: "a1", Status: failoverv1alpha1.ActionStatusSucceeded}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test-cluster",
		ActionExecutor: &action.MultiExecutor{Job: &action.JobExecutor{Client: c}},
		Syncer:         &stubSyncer{},
	}
	res, err := r.completeTransition(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res.RequeueAfter != antiEntropyInterval {
		t.Errorf("expected antiEntropy requeue, got %v", res.RequeueAfter)
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.Transition != nil {
		t.Error("expected transition cleared")
	}
	if updated.Status.ActiveCluster != "test-cluster" {
		t.Errorf("expected ActiveCluster=test-cluster, got %q", updated.Status.ActiveCluster)
	}
}

func TestCompleteTransition_SyncErrorSetsCondition(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ct2"
	now := metav1.Now()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		TargetDirection: directionFailover,
		TransitionID:    "txn-ct2",
		StartedAt:       &now,
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client:    c,
		ClusterID: "test-cluster",
		Syncer:    &stubSyncer{err: errors.New("remote unavailable")},
	}

	res, err := r.completeTransition(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res.RequeueAfter != antiEntropyInterval {
		t.Fatalf("expected antiEntropy requeue, got %v", res.RequeueAfter)
	}

	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	cond := meta.FindStatusCondition(updated.Status.Conditions, failoverv1alpha1.ConditionRemoteSynced)
	if cond == nil || cond.Reason != "SyncFailed" {
		t.Fatalf("expected RemoteSynced condition reason SyncFailed, got %#v", cond)
	}
}

func TestCompleteTransition_RemoteWinsAdoptsRemoteState(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ct3"
	fs.Spec.FailoverActive = true
	now := metav1.Now()
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		TargetDirection: directionFailover,
		TransitionID:    "txn-ct3",
		StartedAt:       &now,
		EventName:       "event-ct3",
	}
	remoteSpecChangeTime := metav1.NewTime(time.Unix(300, 0))
	event := &failoverv1alpha1.FailoverEvent{
		ObjectMeta: metav1.ObjectMeta{Name: "event-ct3", Namespace: fs.Namespace},
		Status: failoverv1alpha1.FailoverEventStatus{
			Outcome: failoverv1alpha1.EventOutcomeInProgress,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs, event).
		WithStatusSubresource(fs, event).
		Build()
	r := &FailoverServiceReconciler{
		Client:    c,
		ClusterID: "cluster-local",
		Syncer: &stubSyncer{
			result: &remotecluster.SyncResult{
				RemoteWins: true,
				RemoteState: &failoverv1alpha1.RemoteState{
					FailoverActive:   true,
					ActiveGeneration: 11,
					ActiveCluster:    testRemoteClusterID,
					ClusterID:        testRemoteClusterID,
					SpecChangeTime:   &remoteSpecChangeTime,
				},
			},
		},
	}

	res, err := r.completeTransition(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Fatalf("expected immediate requeue when remote wins, got %#v", res)
	}

	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if !updated.Spec.FailoverActive {
		t.Fatalf("expected spec.failoverActive=true after remote adoption")
	}
	if updated.Status.ActiveGeneration != 11 {
		t.Fatalf("expected ActiveGeneration=11, got %d", updated.Status.ActiveGeneration)
	}
	if updated.Status.ActiveCluster != testRemoteClusterID {
		t.Fatalf("expected ActiveCluster=%s, got %q", testRemoteClusterID, updated.Status.ActiveCluster)
	}
	if updated.Status.LastKnownFailoverActive == nil || !*updated.Status.LastKnownFailoverActive {
		t.Fatalf("expected LastKnownFailoverActive to preserve previous local state=true, got %#v", updated.Status.LastKnownFailoverActive)
	}
	if updated.Status.SpecChangeTime == nil || !updated.Status.SpecChangeTime.Equal(&remoteSpecChangeTime) {
		t.Fatalf("expected SpecChangeTime=%v, got %#v", remoteSpecChangeTime, updated.Status.SpecChangeTime)
	}
	updatedEvent := &failoverv1alpha1.FailoverEvent{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "event-ct3", Namespace: fs.Namespace}, updatedEvent); err != nil {
		t.Fatalf("get FailoverEvent: %v", err)
	}
	if updatedEvent.Status.Outcome != failoverv1alpha1.EventOutcomeSucceeded {
		t.Fatalf("expected event outcome Succeeded, got %q", updatedEvent.Status.Outcome)
	}
	if updatedEvent.Status.CompletedAt == nil {
		t.Fatal("expected event CompletedAt to be set")
	}
}

// --- cleanupTransitionJobs tests ---

func TestCleanupTransitionJobs(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ctj"
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		PreActionResults:  []failoverv1alpha1.ActionResult{{Name: "a1", JobName: "job-1"}, {Name: "a2"}},
		PostActionResults: []failoverv1alpha1.ActionResult{{Name: "p1", JobName: "job-2"}},
	}
	job1 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job-1", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "a", Image: "alpine"}}}}},
	}
	job2 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job-2", Namespace: "default"},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "a", Image: "alpine"}}}}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs, job1, job2).Build()
	r := &FailoverServiceReconciler{
		Client:         c,
		ActionExecutor: &action.MultiExecutor{Job: &action.JobExecutor{Client: c}},
	}
	r.cleanupTransitionJobs(context.Background(), fs)
	j := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "job-1", Namespace: "default"}, j); err == nil {
		t.Error("expected job-1 to be deleted")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "job-2", Namespace: "default"}, j); err == nil {
		t.Error("expected job-2 to be deleted")
	}
}

// --- reconcileUpdatingResources tests ---

func TestReconcileUpdatingResources(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-rur"
	fs.Status.Transition = &failoverv1alpha1.TransitionStatus{
		Phase:           failoverv1alpha1.PhaseUpdatingResources,
		TargetDirection: directionFailover,
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ServiceManager: &svcmanager.Manager{Client: c, ClusterRole: rolePrimary},
	}
	res, err := r.reconcileUpdatingResources(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Error("expected Requeue=true")
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.Transition.Phase != failoverv1alpha1.PhaseFlushingConnections {
		t.Errorf("expected FlushingConnections, got %q", updated.Status.Transition.Phase)
	}
}

// --- reconcileSteadyState tests ---

func TestReconcileSteadyState_NilSyncResult(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-rss"
	fa := false
	fs.Status.LastKnownFailoverActive = &fa
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test", Syncer: &stubSyncer{}}

	res, err := r.reconcileSteadyState(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res.RequeueAfter != antiEntropyInterval {
		t.Errorf("expected antiEntropy requeue, got %v", res.RequeueAfter)
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.ObservedGeneration != fs.Generation {
		t.Errorf("expected ObservedGeneration=%d, got %d", fs.Generation, updated.Status.ObservedGeneration)
	}
}

func TestReconcileSteadyState_SyncErrorSetsCondition(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-rss-err"
	fa := false
	fs.Status.LastKnownFailoverActive = &fa
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client:      c,
		ClusterRole: rolePrimary,
		ClusterID:   "test",
		Syncer:      &stubSyncer{err: errors.New("remote sync failed")},
	}

	res, err := r.reconcileSteadyState(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res.RequeueAfter != antiEntropyInterval {
		t.Fatalf("expected antiEntropy requeue, got %v", res.RequeueAfter)
	}

	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	cond := meta.FindStatusCondition(updated.Status.Conditions, failoverv1alpha1.ConditionRemoteSynced)
	if cond == nil || cond.Reason != "SyncFailed" {
		t.Fatalf("expected RemoteSynced condition reason SyncFailed, got %#v", cond)
	}
}

func TestReconcileSteadyState_RemoteWinsAdoptsState(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-rss-win"
	fs.Spec.FailoverActive = false
	prev := false
	fs.Status.LastKnownFailoverActive = &prev
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	remoteSpecChangeTime := metav1.NewTime(time.Unix(400, 0))
	r := &FailoverServiceReconciler{
		Client:      c,
		ClusterRole: rolePrimary,
		ClusterID:   "cluster-local",
		Syncer: &stubSyncer{
			result: &remotecluster.SyncResult{
				RemoteWins: true,
				RemoteState: &failoverv1alpha1.RemoteState{
					FailoverActive:   true,
					ActiveGeneration: 8,
					ActiveCluster:    testRemoteClusterID,
					ClusterID:        testRemoteClusterID,
					SpecChangeTime:   &remoteSpecChangeTime,
				},
			},
		},
	}

	res, err := r.reconcileSteadyState(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Fatalf("expected immediate requeue when remote wins, got %#v", res)
	}

	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if !updated.Spec.FailoverActive {
		t.Fatalf("expected spec.failoverActive=true after remote adoption")
	}
	if updated.Status.ActiveGeneration != 8 {
		t.Fatalf("expected ActiveGeneration=8, got %d", updated.Status.ActiveGeneration)
	}
	if updated.Status.ActiveCluster != testRemoteClusterID {
		t.Fatalf("expected ActiveCluster=%s, got %q", testRemoteClusterID, updated.Status.ActiveCluster)
	}
	if updated.Status.LastKnownFailoverActive == nil || *updated.Status.LastKnownFailoverActive != prev {
		t.Fatalf("expected LastKnownFailoverActive to preserve previous local state=%v, got %#v", prev, updated.Status.LastKnownFailoverActive)
	}
	if updated.Status.SpecChangeTime == nil || !updated.Status.SpecChangeTime.Equal(&remoteSpecChangeTime) {
		t.Fatalf("expected SpecChangeTime=%v, got %#v", remoteSpecChangeTime, updated.Status.SpecChangeTime)
	}
	if updated.Status.Transition == nil {
		t.Fatal("expected remote adoption to begin a local transition")
	}
	if updated.Status.LastTransitionTime != nil {
		t.Fatalf("expected LastTransitionTime to remain unset until local completion, got %#v", updated.Status.LastTransitionTime)
	}
}

// --- reconcileIdle tests ---

func TestReconcileIdle_InitialReconcile(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ri1"
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ServiceManager: &svcmanager.Manager{Client: c, ClusterRole: rolePrimary},
		Syncer:         &stubSyncer{},
	}
	res, err := r.reconcileIdle(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res.RequeueAfter != antiEntropyInterval {
		t.Errorf("expected antiEntropy requeue, got %v", res.RequeueAfter)
	}
}

func TestReconcileIdle_DetectsTransition(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-ri2"
	fa := false
	fs.Status.LastKnownFailoverActive = &fa
	fs.Spec.FailoverActive = true
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{
		Client: c, ClusterRole: rolePrimary, ClusterID: "test",
		ServiceManager: &svcmanager.Manager{Client: c, ClusterRole: rolePrimary},
	}
	res, err := r.reconcileIdle(context.Background(), fs)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Error("expected Requeue=true for transition")
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.Transition == nil {
		t.Error("expected transition to be started")
	}
}

// --- beginTransition tests ---

func TestBeginTransition_WithPreActions(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-bt1"
	fs.Spec.FailoverActive = true
	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(fs).
		WithStatusSubresource(fs, &failoverv1alpha1.FailoverEvent{}).
		Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	res, err := r.beginTransition(context.Background(), fs, "manual")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Error("expected Requeue=true")
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.Transition.Phase != failoverv1alpha1.PhaseExecutingPreActions {
		t.Errorf("expected ExecutingPreActions, got %q", updated.Status.Transition.Phase)
	}
	if updated.Status.Transition.EventName == "" {
		t.Fatal("expected EventName to be set")
	}
	event := &failoverv1alpha1.FailoverEvent{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: updated.Status.Transition.EventName, Namespace: fs.Namespace}, event); err != nil {
		t.Fatalf("get FailoverEvent: %v", err)
	}
	if event.Spec.FailoverServiceRef != fs.Name || event.Status.Outcome != failoverv1alpha1.EventOutcomeInProgress {
		t.Fatalf("unexpected event %#v", event)
	}
}

func TestBeginTransition_NoPreActionsSkipsToUpdate(t *testing.T) {
	fs := testFS()
	fs.UID = "uid-bt2"
	fs.Spec.FailoverActive = false
	fs.Spec.PrimaryCluster.OnFailback = &failoverv1alpha1.TransitionActions{
		PostActions: []failoverv1alpha1.FailoverAction{{Name: "post-only"}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fs).WithStatusSubresource(fs).Build()
	r := &FailoverServiceReconciler{Client: c, ClusterRole: rolePrimary, ClusterID: "test"}

	res, err := r.beginTransition(context.Background(), fs, "manual")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Error("expected Requeue=true")
	}
	updated := &failoverv1alpha1.FailoverService{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: fs.Name, Namespace: fs.Namespace}, updated)
	if updated.Status.Transition.Phase != failoverv1alpha1.PhaseUpdatingResources {
		t.Errorf("expected UpdatingResources when no pre-actions, got %q", updated.Status.Transition.Phase)
	}
}

func TestFailoverEventName_TruncatesLongNames(t *testing.T) {
	r := &FailoverServiceReconciler{}
	startedAt := metav1.NewTime(time.Unix(500, 0))
	fs := &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{
			Name: "this-is-a-very-long-failoverservice-name-that-needs-to-be-truncated-for-events",
		},
	}

	name := r.failoverEventName(fs, directionFailover, &startedAt)
	if len(name) > 63 {
		t.Fatalf("expected event name length <= 63, got %d (%q)", len(name), name)
	}
	if name == "" {
		t.Fatal("expected non-empty event name")
	}
}

// --- updateClusterStatus tests ---

func TestUpdateClusterStatus_NewEntry(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterID: "cluster-a"}
	fs := testFS()
	r.updateClusterStatus(fs, failoverv1alpha1.PhaseIdle)
	if len(fs.Status.Clusters) != 1 {
		t.Fatalf("expected 1 cluster entry, got %d", len(fs.Status.Clusters))
	}
	if fs.Status.Clusters[0].ClusterID != "cluster-a" {
		t.Errorf("expected ClusterID=cluster-a, got %q", fs.Status.Clusters[0].ClusterID)
	}
}

func TestUpdateClusterStatus_UpdatesExisting(t *testing.T) {
	r := &FailoverServiceReconciler{ClusterID: "cluster-a"}
	fs := testFS()
	fs.Status.Clusters = []failoverv1alpha1.ClusterStatus{{ClusterID: "cluster-a", Phase: failoverv1alpha1.PhaseIdle}}
	r.updateClusterStatus(fs, failoverv1alpha1.PhaseUpdatingResources)
	if len(fs.Status.Clusters) != 1 {
		t.Fatalf("expected 1 cluster entry, got %d", len(fs.Status.Clusters))
	}
	if fs.Status.Clusters[0].Phase != failoverv1alpha1.PhaseUpdatingResources {
		t.Errorf("expected UpdatingResources, got %q", fs.Status.Clusters[0].Phase)
	}
}
