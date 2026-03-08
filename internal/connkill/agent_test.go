//go:build linux

package connkill

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8swatch "k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func seqKey(nodeName, fsNamespace, fsName string) string { //nolint:unparam
	return strings.TrimPrefix(killKey(nodeName, fsNamespace, fsName), "kill_")
}

type flakyUpdateClient struct {
	client.Client
	failNextCMUpdate bool
}

func (c *flakyUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.failNextCMUpdate {
		if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == ConfigMapName {
			c.failNextCMUpdate = false
			return errors.New("injected configmap update failure")
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestRunOnce_MultiKeyScanning(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	sig1, _ := json.Marshal(KillSignal{IP: "10.0.0.1", Seq: 1, Token: "token-a"})
	sig2, _ := json.Marshal(KillSignal{IP: "10.0.0.2", Seq: 1, Token: "token-b"})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): string(sig1),
			killKey("node-1", "default", "fs-b"): string(sig2),
			killKey("node-2", "default", "fs-a"): string(sig1), // for a different node
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	// RunOnce should process both keys for node-1
	err := agent.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}

	// Verify sequence tracking for each key suffix.
	if agent.lastSeq == nil {
		t.Fatal("expected lastSeq map to be initialized")
	}
	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 1 {
		t.Errorf("expected lastSeq for fs-a to be 1, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}
	if agent.lastSeq[seqKey("node-1", "default", "fs-b")] != 1 {
		t.Errorf("expected lastSeq for fs-b to be 1, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-b")])
	}

	// Verify ACK keys were written
	updatedCM := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, updatedCM); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}

	ackA := ackKey("node-1", "default", "fs-a")
	ackB := ackKey("node-1", "default", "fs-b")

	if _, ok := updatedCM.Data[ackA]; !ok {
		t.Errorf("expected ACK key %s", ackA)
	}
	if _, ok := updatedCM.Data[ackB]; !ok {
		t.Errorf("expected ACK key %s", ackB)
	}
}

func TestRunOnce_UsesSignalNodeForTruncatedKey(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	nodeName := strings.Repeat("n", 240)
	fsName := strings.Repeat("f", 100)
	key := killKey(nodeName, "default", fsName)
	ack := ackKey(nodeName, "default", fsName)

	// Verify this test actually exercises the truncation path.
	if len(key) != maxConfigMapKeyLen {
		t.Fatalf("expected truncated key length %d, got %d", maxConfigMapKeyLen, len(key))
	}

	sigData, _ := json.Marshal(KillSignal{
		IP:    "10.0.0.1",
		Seq:   1,
		Token: "token-trunc",
		Node:  nodeName,
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			key: string(sigData),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	agent := &Agent{
		Client:    c,
		NodeName:  nodeName,
		Namespace: "test-ns",
	}

	if err := agent.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}

	if agent.lastSeq[strings.TrimPrefix(key, "kill_")] != 1 {
		t.Fatalf("expected lastSeq for truncated key to be 1, got %d", agent.lastSeq[strings.TrimPrefix(key, "kill_")])
	}

	updatedCM := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, updatedCM); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}
	if _, ok := updatedCM.Data[ack]; !ok {
		t.Fatalf("expected ACK key %q for truncated signal", ack)
	}
}

func TestRunOnce_IgnoresSignalForOtherNode(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	sigData, _ := json.Marshal(KillSignal{
		IP:    "10.0.0.1",
		Seq:   1,
		Token: "token-other-node",
		Node:  "node-2",
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): string(sigData),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	if err := agent.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}

	if len(agent.lastSeq) != 0 {
		t.Fatalf("expected no sequence updates for other node signal, got %v", agent.lastSeq)
	}

	updatedCM := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, updatedCM); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}
	if _, ok := updatedCM.Data[ackKey("node-1", "default", "fs-a")]; ok {
		t.Fatal("did not expect ACK for signal targeting another node")
	}
}

func TestRunOnce_AckWriteFailureDoesNotAdvanceSequence(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	sig, _ := json.Marshal(KillSignal{
		IP:    "10.0.0.1",
		Seq:   1,
		Token: "token-a",
		Node:  "node-1",
	})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): string(sig),
		},
	}

	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	c := &flakyUpdateClient{
		Client:           baseClient,
		failNextCMUpdate: true,
	}
	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	// First run: kill is processed, but ACK write fails.
	if err := agent.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}
	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 0 {
		t.Fatalf("expected sequence not advanced after ACK write failure, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}

	// Second run: same signal should be retried and ACK should now persist.
	if err := agent.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce error: %v", err)
	}
	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 1 {
		t.Fatalf("expected sequence advanced after ACK success, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}

	updatedCM := &corev1.ConfigMap{}
	if err := baseClient.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, updatedCM); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}
	if _, ok := updatedCM.Data[ackKey("node-1", "default", "fs-a")]; !ok {
		t.Fatal("expected ACK key after retry")
	}
}

func TestRunOnce_SkipsProcessedSequence(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	sig, _ := json.Marshal(KillSignal{IP: "10.0.0.1", Seq: 1, Token: "token-a"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): string(sig),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
		lastSeq:   map[string]int64{seqKey("node-1", "default", "fs-a"): 1}, // already processed
	}

	err := agent.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}

	// Should not write a new ACK since seq was already processed
	updatedCM := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, updatedCM); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}

	ackA := ackKey("node-1", "default", "fs-a")
	if _, ok := updatedCM.Data[ackA]; ok {
		t.Error("expected no ACK written for already-processed sequence")
	}
}

func TestRunOnce_NoSignals(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data:       map[string]string{},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	err := agent.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}
}

func TestRunOnce_ConfigMapNotFound(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// No ConfigMap exists
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	err := agent.RunOnce(ctx)
	if err != nil {
		t.Fatalf("expected nil error when ConfigMap not found, got: %v", err)
	}
}

func TestRunOnce_InvalidIP(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	sig, _ := json.Marshal(KillSignal{IP: "not-an-ip", Seq: 1, Token: "token-1"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): string(sig),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	// Should not panic — invalid IP is logged and skipped
	err := agent.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}

	// Should not have updated lastSeq for this entry since it was skipped
	if agent.lastSeq != nil && agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 0 {
		t.Errorf("expected lastSeq not updated for invalid IP, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}
}

func TestInitFromConfigMap_SeedsFromACKsNotKills(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Simulate: kill signal at seq=3 exists but only ACK at seq=2.
	// After restart, agent should process seq=3 (not skip it).
	killSig, _ := json.Marshal(KillSignal{IP: "10.0.0.1", Seq: 3, Token: "token-c"})
	ackData, _ := json.Marshal(KillAck{Seq: 2, Token: "token-b", Killed: 1})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): string(killSig),
			ackKey("node-1", "default", "fs-a"):  string(ackData),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	// initFromConfigMap should seed from ACK (seq=2), not kill (seq=3)
	agent.initFromConfigMap(ctx)

	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 2 {
		t.Fatalf("expected lastSeq=2 from ACK, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}

	// RunOnce should now process the pending kill signal at seq=3
	err := agent.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}

	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 3 {
		t.Errorf("expected lastSeq=3 after processing, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}
}

func TestInitFromConfigMap_NoACK_ProcessesPendingKill(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// Kill signal exists but no ACK — agent should process it after restart
	killSig, _ := json.Marshal(KillSignal{IP: "10.0.0.1", Seq: 1, Token: "token-a"})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): string(killSig),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	agent.initFromConfigMap(ctx)

	// No ACK exists, so lastSeq should be 0 for this suffix
	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 0 {
		t.Fatalf("expected lastSeq=0 (no ACK), got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}

	// RunOnce should process the kill signal
	err := agent.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}

	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 1 {
		t.Errorf("expected lastSeq=1 after processing, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}
}

func TestWatch_ContextCancellation(t *testing.T) {
	scheme := newScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- agent.Watch(ctx, 50*time.Millisecond)
	}()

	// Let it tick at least once
	time.Sleep(100 * time.Millisecond)
	cancel()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestWatch_InitializesFromConfigMap(t *testing.T) {
	scheme := newScheme()

	ackData, _ := json.Marshal(KillAck{Seq: 5, Token: "old-token", Killed: 1})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			ackKey("node-1", "default", "fs-a"): string(ackData),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- agent.Watch(ctx, 50*time.Millisecond)
	}()

	// Give it time to init and tick
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-errCh

	// Verify lastSeq was seeded from ACK
	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 5 {
		t.Errorf("expected lastSeq=5 from ACK init, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}
}

func TestWatch_ProcessesClientsetWatchEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheme := newScheme()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data:       map[string]string{},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	clientset := k8sfake.NewClientset()
	watcher := k8swatch.NewFake()
	clientset.PrependWatchReactor("configmaps", k8stesting.DefaultWatchReactor(watcher, nil))

	agent := &Agent{
		Client:    c,
		Clientset: clientset,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- agent.Watch(ctx, time.Hour)
	}()

	sig, _ := json.Marshal(KillSignal{
		IP:    "10.0.0.1",
		Seq:   1,
		Token: "token-watch",
		Node:  "node-1",
	})

	updatedCM := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, updatedCM); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}
	if updatedCM.Data == nil {
		updatedCM.Data = map[string]string{}
	}
	updatedCM.Data[killKey("node-1", "default", "fs-a")] = string(sig)
	if err := c.Update(ctx, updatedCM); err != nil {
		t.Fatalf("Update ConfigMap error: %v", err)
	}
	watcher.Modify(updatedCM.DeepCopy())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, current); err != nil {
			t.Fatalf("Get ConfigMap error: %v", err)
		}
		if _, ok := current.Data[ackKey("node-1", "default", "fs-a")]; ok {
			cancel()
			if err := <-errCh; !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context cancellation, got %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for ACK from watch-triggered RunOnce")
}

func TestRunOnce_MalformedSignalJSON(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): "not-valid-json{{{",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	// Should not return error — malformed signals are logged and skipped
	err := agent.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}

	// lastSeq should not be set for this entry
	if agent.lastSeq != nil && agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 0 {
		t.Errorf("expected lastSeq not set for malformed signal, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}
}

func TestInitFromConfigMap_MalformedACK(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			ackKey("node-1", "default", "fs-a"): "not-json",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	agent.initFromConfigMap(ctx)

	// Malformed ACK should be skipped, lastSeq defaults to 0
	if agent.lastSeq[seqKey("node-1", "default", "fs-a")] != 0 {
		t.Errorf("expected lastSeq=0 for malformed ACK, got %d", agent.lastSeq[seqKey("node-1", "default", "fs-a")])
	}
}

func TestInitFromConfigMap_IgnoresOtherNodes(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	ackData, _ := json.Marshal(KillAck{Seq: 3, Token: "tok"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			ackKey("node-2", "default", "fs-a"): string(ackData), // different node
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	agent.initFromConfigMap(ctx)

	// Should not have any entries since ACK is for node-2
	if len(agent.lastSeq) != 0 {
		t.Errorf("expected empty lastSeq for different node, got %v", agent.lastSeq)
	}
}

func TestInitFromConfigMap_NoConfigMap(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := &Agent{
		Client:    c,
		NodeName:  "node-1",
		Namespace: "test-ns",
	}

	// Should not panic when ConfigMap doesn't exist
	agent.initFromConfigMap(ctx)

	// lastSeq may be nil or empty — either is fine
	if len(agent.lastSeq) > 0 {
		t.Errorf("expected nil or empty lastSeq, got %v", agent.lastSeq)
	}
}
