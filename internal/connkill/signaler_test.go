package connkill

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}

func readyAgentPod(name, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-ns",
			Labels: map[string]string{
				agentLabelKey: agentLabelVal,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{Name: "connection-killer", Image: "example/connection-killer:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func linuxNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				corev1.LabelOSStable: "linux",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func TestSignalKill_CreatesConfigMap(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	node1 := linuxNode("node-1")
	node2 := linuxNode("node-2")
	pod1 := readyAgentPod("connection-killer-node-1", "node-1")
	pod2 := readyAgentPod("connection-killer-node-2", "node-2")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node1, node2, pod1, pod2).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}
	nodeNames, err := s.SignalKill(ctx, "10.0.0.1", "token-1", "default", "my-fs")
	if err != nil {
		t.Fatalf("SignalKill error: %v", err)
	}

	if len(nodeNames) != 2 {
		t.Fatalf("expected 2 node names, got %d", len(nodeNames))
	}

	// Verify ConfigMap was created with namespaced keys
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, cm); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}

	for _, nodeName := range nodeNames {
		key := killKey(nodeName, "default", "my-fs")
		data, ok := cm.Data[key]
		if !ok {
			t.Fatalf("missing key %s in ConfigMap", key)
		}
		var sig KillSignal
		if err := json.Unmarshal([]byte(data), &sig); err != nil {
			t.Fatalf("unmarshal kill signal: %v", err)
		}
		if sig.IP != "10.0.0.1" {
			t.Errorf("expected IP 10.0.0.1, got %s", sig.IP)
		}
		if sig.Token != "token-1" {
			t.Errorf("expected token token-1, got %s", sig.Token)
		}
		if sig.Node != nodeName {
			t.Errorf("expected node %s, got %s", nodeName, sig.Node)
		}
		if sig.Seq != 1 {
			t.Errorf("expected seq 1, got %d", sig.Seq)
		}
	}
}

func TestSignalKill_IncrementsSequence(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	node := linuxNode("node-1")
	pod := readyAgentPod("connection-killer-node-1", "node-1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}

	// First signal
	_, err := s.SignalKill(ctx, "10.0.0.1", "token-1", "default", "my-fs")
	if err != nil {
		t.Fatalf("first SignalKill error: %v", err)
	}

	// Second signal - seq should increment
	_, err = s.SignalKill(ctx, "10.0.0.2", "token-2", "default", "my-fs")
	if err != nil {
		t.Fatalf("second SignalKill error: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, cm); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}

	key := killKey("node-1", "default", "my-fs")
	var sig KillSignal
	if err := json.Unmarshal([]byte(cm.Data[key]), &sig); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sig.Seq != 2 {
		t.Errorf("expected seq 2 after second signal, got %d", sig.Seq)
	}
	if sig.IP != "10.0.0.2" {
		t.Errorf("expected IP 10.0.0.2, got %s", sig.IP)
	}
}

func TestSignalKill_ConcurrentFSDoNotInterfere(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	node := linuxNode("node-1")
	pod := readyAgentPod("connection-killer-node-1", "node-1")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}

	// Signal for FS "default/fs-a"
	_, err := s.SignalKill(ctx, "10.0.0.1", "token-a", "default", "fs-a")
	if err != nil {
		t.Fatalf("SignalKill fs-a error: %v", err)
	}

	// Signal for FS "default/fs-b"
	_, err = s.SignalKill(ctx, "10.0.0.2", "token-b", "default", "fs-b")
	if err != nil {
		t.Fatalf("SignalKill fs-b error: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, cm); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}

	// Verify both keys exist independently
	keyA := killKey("node-1", "default", "fs-a")
	keyB := killKey("node-1", "default", "fs-b")

	var sigA, sigB KillSignal
	if err := json.Unmarshal([]byte(cm.Data[keyA]), &sigA); err != nil {
		t.Fatalf("unmarshal key-a: %v", err)
	}
	if err := json.Unmarshal([]byte(cm.Data[keyB]), &sigB); err != nil {
		t.Fatalf("unmarshal key-b: %v", err)
	}

	if sigA.IP != "10.0.0.1" || sigA.Token != "token-a" {
		t.Errorf("fs-a signal incorrect: %+v", sigA)
	}
	if sigB.IP != "10.0.0.2" || sigB.Token != "token-b" {
		t.Errorf("fs-b signal incorrect: %+v", sigB)
	}
}

func TestCheckAcks_AllAcked(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	ack1, _ := json.Marshal(KillAck{Seq: 1, Token: "token-1", Killed: 5, Errors: 0})
	ack2, _ := json.Marshal(KillAck{Seq: 1, Token: "token-1", Killed: 3, Errors: 0})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			ackKey("node-1", "default", "my-fs"): string(ack1),
			ackKey("node-2", "default", "my-fs"): string(ack2),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}
	status, err := s.CheckAcks(ctx, "token-1", []string{"node-1", "node-2"}, "default", "my-fs")
	if err != nil {
		t.Fatalf("CheckAcks error: %v", err)
	}

	if !status.AllAcked {
		t.Error("expected AllAcked=true")
	}
	if len(status.AckedNodes) != 2 {
		t.Errorf("expected 2 acked nodes, got %d", len(status.AckedNodes))
	}
	if len(status.PendingNodes) != 0 {
		t.Errorf("expected 0 pending nodes, got %d", len(status.PendingNodes))
	}
}

func TestCheckAcks_TokenMismatch(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	ack, _ := json.Marshal(KillAck{Seq: 1, Token: "wrong-token", Killed: 5, Errors: 0})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			ackKey("node-1", "default", "my-fs"): string(ack),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}
	status, err := s.CheckAcks(ctx, "token-1", []string{"node-1"}, "default", "my-fs")
	if err != nil {
		t.Fatalf("CheckAcks error: %v", err)
	}

	if status.AllAcked {
		t.Error("expected AllAcked=false for token mismatch")
	}
	if len(status.PendingNodes) != 1 {
		t.Errorf("expected 1 pending node, got %d", len(status.PendingNodes))
	}
}

func TestCheckAcks_ErrorNodes(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	ack, _ := json.Marshal(KillAck{Seq: 1, Token: "token-1", Killed: 2, Errors: 3})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			ackKey("node-1", "default", "my-fs"): string(ack),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}
	status, err := s.CheckAcks(ctx, "token-1", []string{"node-1"}, "default", "my-fs")
	if err != nil {
		t.Fatalf("CheckAcks error: %v", err)
	}

	if status.AllAcked {
		t.Error("expected AllAcked=false when errors present")
	}
	if len(status.ErrorNodes) != 1 {
		t.Errorf("expected 1 error node, got %d", len(status.ErrorNodes))
	}
}

func TestCheckAcks_NamespacedIsolation(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// ACK for fs-a
	ackA, _ := json.Marshal(KillAck{Seq: 1, Token: "token-a", Killed: 1, Errors: 0})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			ackKey("node-1", "default", "fs-a"): string(ackA),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}

	// Check ACKs for fs-b — should see pending since ack is for fs-a
	status, err := s.CheckAcks(ctx, "token-b", []string{"node-1"}, "default", "fs-b")
	if err != nil {
		t.Fatalf("CheckAcks error: %v", err)
	}

	if status.AllAcked {
		t.Error("expected AllAcked=false for different FS")
	}
	if len(status.PendingNodes) != 1 {
		t.Errorf("expected 1 pending node, got %d", len(status.PendingNodes))
	}
}

func TestKillKey(t *testing.T) {
	key := killKey("node-1", "default", "my-fs")
	expected := "kill_node-1_default_my-fs"
	if key != expected {
		t.Errorf("expected %s, got %s", expected, key)
	}
}

func TestAckKey(t *testing.T) {
	key := ackKey("node-1", "default", "my-fs")
	expected := "ack_node-1_default_my-fs"
	if key != expected {
		t.Errorf("expected %s, got %s", expected, key)
	}
}

func TestCheckAcks_MalformedJSON(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			ackKey("node-1", "default", "my-fs"): "not-valid-json{{{",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}
	status, err := s.CheckAcks(ctx, "token-1", []string{"node-1"}, "default", "my-fs")
	if err != nil {
		t.Fatalf("CheckAcks error: %v", err)
	}

	if status.AllAcked {
		t.Error("expected AllAcked=false for malformed JSON")
	}
	if len(status.PendingNodes) != 1 {
		t.Errorf("expected 1 pending node (malformed treated as pending), got %d", len(status.PendingNodes))
	}
}

func TestSignalKill_NilDataMap(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	node := linuxNode("node-1")
	pod := readyAgentPod("connection-killer-node-1", "node-1")
	// Existing ConfigMap with nil Data
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod, cm).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}
	nodeNames, err := s.SignalKill(ctx, "10.0.0.1", "token-1", "default", "my-fs")
	if err != nil {
		t.Fatalf("SignalKill error: %v", err)
	}
	if len(nodeNames) != 1 {
		t.Fatalf("expected 1 node name, got %d", len(nodeNames))
	}

	// Verify data was populated
	updated := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, updated); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}
	key := killKey("node-1", "default", "my-fs")
	if _, ok := updated.Data[key]; !ok {
		t.Errorf("expected key %s in ConfigMap Data", key)
	}
}

func TestSignalKill_NoAgentPods_SkipsSignalCreation(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	node1 := linuxNode("node-1")
	node2 := linuxNode("node-2")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node1, node2).Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}
	nodeNames, err := s.SignalKill(ctx, "10.0.0.1", "token-1", "default", "my-fs")
	if err != nil {
		t.Fatalf("SignalKill error: %v", err)
	}
	if len(nodeNames) != 0 {
		t.Fatalf("expected no nodes to be targeted when no agent pods exist, got %v", nodeNames)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, cm); err == nil {
		t.Fatal("expected no ConfigMap to be created when no agent pods exist")
	}
}

func TestSignalKill_TargetsEligibleNodesEvenWhenPodsUnready(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	node1 := linuxNode("node-1")
	node2 := linuxNode("node-2")
	node3 := linuxNode("node-3")

	readyPod := readyAgentPod("connection-killer-node-1", "node-1")
	notReadyPod := readyAgentPod("connection-killer-node-2", "node-2")
	notReadyPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse},
	}
	pendingPod := readyAgentPod("connection-killer-node-3", "node-3")
	pendingPod.Status.Phase = corev1.PodPending

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node1, node2, node3, readyPod, notReadyPod, pendingPod).
		Build()

	s := &Signaler{Client: c, Namespace: "test-ns"}
	nodeNames, err := s.SignalKill(ctx, "10.0.0.1", "token-1", "default", "my-fs")
	if err != nil {
		t.Fatalf("SignalKill error: %v", err)
	}
	if len(nodeNames) != 3 {
		t.Fatalf("expected all eligible nodes as targets, got %v", nodeNames)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, cm); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}
	for _, nodeName := range []string{"node-1", "node-2", "node-3"} {
		if _, ok := cm.Data[killKey(nodeName, "default", "my-fs")]; !ok {
			t.Fatalf("expected kill signal for %s", nodeName)
		}
	}
}

func TestDiscoverAgentNodes_NoPodsReturnsNil(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	// With no nodes present, discoverAgentNodes should return nil.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := &Signaler{Client: c, Namespace: "test-ns"}

	nodes, err := s.discoverAgentNodes(ctx)
	if err != nil {
		t.Fatalf("discoverAgentNodes error: %v", err)
	}
	if nodes != nil {
		t.Fatalf("expected nil nodes when no pods found, got %v", nodes)
	}
}

func TestDiscoverAgentNodes_WithoutAgentPodsReturnsNil(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	node1 := linuxNode("node-1")
	node2 := linuxNode("node-2")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node1, node2).Build()
	s := &Signaler{Client: c, Namespace: "test-ns"}

	nodes, err := s.discoverAgentNodes(ctx)
	if err != nil {
		t.Fatalf("discoverAgentNodes error: %v", err)
	}
	if nodes != nil {
		t.Fatalf("expected nil nodes when no agent pods exist, got %v", nodes)
	}
}

func TestCheckAcks_EmptyExpectedNodes_AllAckedWithoutConfigMap(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	s := &Signaler{Client: c, Namespace: "test-ns"}

	status, err := s.CheckAcks(ctx, "token-1", nil, "default", "my-fs")
	if err != nil {
		t.Fatalf("CheckAcks error: %v", err)
	}
	if !status.AllAcked {
		t.Fatal("expected AllAcked=true for empty expected nodes")
	}
	if len(status.AckedNodes) != 0 || len(status.PendingNodes) != 0 || len(status.ErrorNodes) != 0 {
		t.Fatalf("expected empty node lists, got %+v", status)
	}
}

func TestSafeKey_Short(t *testing.T) {
	key := safeKey("kill", "node-1", "default", "my-fs")
	if key != "kill_node-1_default_my-fs" {
		t.Errorf("unexpected key: %s", key)
	}
}

func TestSafeKey_LongTruncated(t *testing.T) {
	longName := strings.Repeat("a", 250)
	key := safeKey("kill", "node-1", "default", longName)
	if len(key) > maxConfigMapKeyLen {
		t.Errorf("key exceeds max length: %d > %d", len(key), maxConfigMapKeyLen)
	}
	if len(key) != maxConfigMapKeyLen {
		t.Errorf("expected exactly %d chars, got %d", maxConfigMapKeyLen, len(key))
	}
}

func TestSafeKey_Deterministic(t *testing.T) {
	longName := strings.Repeat("b", 200)
	k1 := safeKey("kill", "node-1", "default", longName)
	k2 := safeKey("kill", "node-1", "default", longName)
	if k1 != k2 {
		t.Error("expected deterministic key generation")
	}
}

func TestSafeKey_DifferentInputsDifferentKeys(t *testing.T) {
	longName1 := strings.Repeat("c", 200)
	longName2 := strings.Repeat("d", 200)
	k1 := safeKey("kill", "node-1", "default", longName1)
	k2 := safeKey("kill", "node-1", "default", longName2)
	if k1 == k2 {
		t.Error("expected different keys for different inputs")
	}
}

func TestKillKeyAckKeyConsistency(t *testing.T) {
	// Verify kill and ack keys are different for the same inputs
	kk := killKey("node-1", "default", "my-fs")
	ak := ackKey("node-1", "default", "my-fs")
	if kk == ak {
		t.Error("kill and ack keys should differ")
	}
	if !strings.HasPrefix(kk, "kill_") {
		t.Error("kill key should start with kill_")
	}
	if !strings.HasPrefix(ak, "ack_") {
		t.Error("ack key should start with ack_")
	}
}

func TestTruncatedKeys_AgentPrefixReplacement(t *testing.T) {
	// Simulate what the agent does: extract fsSuffix from kill key, construct ACK key.
	// This must produce the same key as ackKey() for truncated keys.
	longName := strings.Repeat("x", 250)
	nodeName := "node-1"

	kk := killKey(nodeName, "default", longName)
	ak := ackKey(nodeName, "default", longName)

	// Agent's logic: strip "kill_" prefix, prepend "ack_".
	seqKey := strings.TrimPrefix(kk, "kill_")
	agentACK := "ack_" + seqKey

	if agentACK != ak {
		t.Errorf("agent-derived ACK key does not match canonical ACK key\n  agent: %s\n  canon: %s", agentACK, ak)
	}
}

func TestTruncatedKeys_BoundaryConsistency(t *testing.T) {
	// Boundary case: body is one char longer than what "kill_<body>" can fit.
	// Without prefix-invariant truncation, kill could truncate while ack would not.
	maxRawBodyLen := maxConfigMapKeyLen - len(longestKeyPrefix) - 1
	nodeName := strings.Repeat("n", 63)
	fsNamespace := strings.Repeat("a", 63)
	fsNameLen := maxRawBodyLen - len(nodeName) - len(fsNamespace) - 2 + 1
	fsName := strings.Repeat("b", fsNameLen)

	kk := killKey(nodeName, fsNamespace, fsName)
	ak := ackKey(nodeName, fsNamespace, fsName)
	if len(kk) != maxConfigMapKeyLen {
		t.Fatalf("expected kill key length %d, got %d", maxConfigMapKeyLen, len(kk))
	}
	if len(ak) > maxConfigMapKeyLen {
		t.Fatalf("ack key exceeds max length: %d", len(ak))
	}

	// Agent-derived ACK key from kill suffix must exactly match canonical ackKey.
	seqKey := strings.TrimPrefix(kk, "kill_")
	agentACK := "ack_" + seqKey
	if agentACK != ak {
		t.Fatalf("boundary mismatch: agent ACK %q != canonical ACK %q", agentACK, ak)
	}
}

func TestCleanupSignals_RemovesOnlyTargetFailoverServiceEntries(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()

	killA, _ := json.Marshal(KillSignal{
		IP:          "10.0.0.1",
		Seq:         1,
		Token:       "token-a",
		Node:        "node-1",
		FSNamespace: "default",
		FSName:      "fs-a",
	})
	ackA, _ := json.Marshal(KillAck{
		Seq:         1,
		Token:       "token-a",
		Node:        "node-1",
		FSNamespace: "default",
		FSName:      "fs-a",
		Killed:      2,
		Errors:      0,
	})
	killB, _ := json.Marshal(KillSignal{
		IP:          "10.0.0.2",
		Seq:         1,
		Token:       "token-b",
		Node:        "node-1",
		FSNamespace: "default",
		FSName:      "fs-b",
	})
	ackB, _ := json.Marshal(KillAck{
		Seq:         1,
		Token:       "token-b",
		Node:        "node-1",
		FSNamespace: "default",
		FSName:      "fs-b",
		Killed:      1,
		Errors:      0,
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "test-ns"},
		Data: map[string]string{
			killKey("node-1", "default", "fs-a"): string(killA),
			ackKey("node-1", "default", "fs-a"):  string(ackA),
			killKey("node-1", "default", "fs-b"): string(killB),
			ackKey("node-1", "default", "fs-b"):  string(ackB),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	s := &Signaler{Client: c, Namespace: "test-ns"}

	if err := s.CleanupSignals(ctx, "default", "fs-a"); err != nil {
		t.Fatalf("CleanupSignals error: %v", err)
	}

	updated := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ConfigMapName, Namespace: "test-ns"}, updated); err != nil {
		t.Fatalf("Get ConfigMap error: %v", err)
	}

	if _, ok := updated.Data[killKey("node-1", "default", "fs-a")]; ok {
		t.Fatalf("expected fs-a kill key to be removed")
	}
	if _, ok := updated.Data[ackKey("node-1", "default", "fs-a")]; ok {
		t.Fatalf("expected fs-a ack key to be removed")
	}
	if _, ok := updated.Data[killKey("node-1", "default", "fs-b")]; !ok {
		t.Fatalf("expected fs-b kill key to remain")
	}
	if _, ok := updated.Data[ackKey("node-1", "default", "fs-b")]; !ok {
		t.Fatalf("expected fs-b ack key to remain")
	}
}
