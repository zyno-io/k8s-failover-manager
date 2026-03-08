package connkill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var signalLog = logf.Log.WithName("connkill-signaler")

const (
	// ConfigMapName is the name of the ConfigMap used to signal connection kills.
	ConfigMapName = "connection-kill-signal"
	agentLabelKey = "app.kubernetes.io/name"
	agentLabelVal = "connection-killer"
)

// KillSignal is the data structure written to the ConfigMap for each node.
type KillSignal struct {
	IP          string `json:"ip"`
	Seq         int64  `json:"seq"`
	Token       string `json:"token,omitempty"`
	Node        string `json:"node,omitempty"`
	FSNamespace string `json:"fsNamespace,omitempty"`
	FSName      string `json:"fsName,omitempty"`
}

// KillAck is the acknowledgment structure written back by the agent.
type KillAck struct {
	Seq         int64  `json:"seq"`
	Token       string `json:"token,omitempty"`
	Node        string `json:"node,omitempty"`
	FSNamespace string `json:"fsNamespace,omitempty"`
	FSName      string `json:"fsName,omitempty"`
	Killed      int    `json:"killed"`
	Errors      int    `json:"errors"`
}

// AckStatus describes the aggregate state of connection kill acknowledgments.
type AckStatus struct {
	// AllAcked is true when every expected node has ACKed with matching token and zero errors.
	AllAcked bool
	// AckedNodes lists nodes that have successfully ACKed.
	AckedNodes []string
	// PendingNodes lists nodes that have not yet ACKed.
	PendingNodes []string
	// ErrorNodes lists nodes that ACKed with errors.
	ErrorNodes []string
}

// Signaler writes connection kill commands to a ConfigMap for the DaemonSet agent to pick up.
type Signaler struct {
	Client    client.Client
	Namespace string
}

// maxConfigMapKeyLen is the maximum length for a ConfigMap data key.
// ConfigMap keys must be valid DNS subdomains, limited to 253 characters.
const maxConfigMapKeyLen = 253

// longestKeyPrefix is the longest prefix used in ConfigMap keys ("kill" = 4 chars).
// Using a fixed max body length based on this ensures kill/ack keys share identical
// body suffixes, so the agent can derive ACK keys by replacing the prefix.
const longestKeyPrefix = "kill"

// safeKey builds a ConfigMap key with the given prefix, truncating and hashing
// if needed to satisfy the 253-character DNS subdomain limit.
//
// The truncation decision is made using the longest key prefix ("kill"), not the
// provided prefix, so kill/ack keys for the same inputs always share the same body
// suffix. This is required because the agent derives ACK keys from kill-key suffixes.
func safeKey(prefix, nodeName, fsNamespace, fsName string) string {
	body := fmt.Sprintf("%s_%s_%s", nodeName, fsNamespace, fsName)
	// Use the longest prefix for the no-hash fit check to keep truncation behavior
	// prefix-invariant for the same body.
	maxRawBodyLen := maxConfigMapKeyLen - len(longestKeyPrefix) - 1 // "<prefix>_<body>"
	if len(body) <= maxRawBodyLen {
		key := prefix + "_" + body
		return key
	}
	h := sha256.Sum256([]byte(body))
	hashStr := hex.EncodeToString(h[:8]) // 16 hex chars
	// Fixed max body length based on longest prefix to keep kill/ack bodies identical.
	// Format: longestPrefix + "_" + truncatedBody + "-" + hash
	maxBodyLen := maxConfigMapKeyLen - len(longestKeyPrefix) - 1 - 1 - len(hashStr)
	truncatedBody := body[:maxBodyLen]
	return prefix + "_" + truncatedBody + "-" + hashStr
}

// killKey returns the ConfigMap key for a kill signal, namespaced by FailoverService identity.
// Uses "_" as the separator since "/" is not a valid ConfigMap key character and "." can
// appear in node names (e.g. node-a.prod), causing ambiguous prefix matching.
// "_" cannot appear in Kubernetes resource names ([a-z0-9-] only) so it's unambiguous.
func killKey(nodeName, fsNamespace, fsName string) string {
	return safeKey("kill", nodeName, fsNamespace, fsName)
}

// ackKey returns the ConfigMap key for an ACK, namespaced by FailoverService identity.
func ackKey(nodeName, fsNamespace, fsName string) string {
	return safeKey("ack", nodeName, fsNamespace, fsName)
}

// SignalKill creates or updates the connection-kill-signal ConfigMap with kill commands
// for all nodes. The destIP is the address whose connections should be killed.
// The killToken is included in each signal so agents can match ACKs to the correct kill.
// fsNamespace and fsName identify the FailoverService to prevent interference between
// concurrent transitions of different FailoverService resources.
// Returns the list of expected node names.
func (s *Signaler) SignalKill(ctx context.Context, destIP string, killToken string, fsNamespace, fsName string) ([]string, error) {
	nodeNames, err := s.discoverAgentNodes(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodeNames) == 0 {
		signalLog.Info("No ready connection-killer agents discovered; skipping connection kill signal")
		return nil, nil
	}

	isRetryable := func(err error) bool {
		return errors.IsConflict(err) || errors.IsAlreadyExists(err)
	}
	err = retry.OnError(retry.DefaultRetry, isRetryable, func() error {
		cm := &corev1.ConfigMap{}
		err := s.Client.Get(ctx, types.NamespacedName{
			Name:      ConfigMapName,
			Namespace: s.Namespace,
		}, cm)

		if errors.IsNotFound(err) {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ConfigMapName,
					Namespace: s.Namespace,
				},
				Data: make(map[string]string),
			}
			s.populateKillSignals(cm, nodeNames, destIP, killToken, fsNamespace, fsName)
			return s.Client.Create(ctx, cm)
		}
		if err != nil {
			return fmt.Errorf("getting kill signal configmap: %w", err)
		}

		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		s.populateKillSignals(cm, nodeNames, destIP, killToken, fsNamespace, fsName)
		return s.Client.Update(ctx, cm)
	})
	if err != nil {
		return nil, err
	}

	signalLog.Info("Signaled connection kill", "ip", destIP, "token", killToken, "nodes", strconv.Itoa(len(nodeNames)))
	return nodeNames, nil
}

// CheckAcks reads the ConfigMap and returns the acknowledgment status for the given
// kill token and expected node set, scoped to the identified FailoverService.
func (s *Signaler) CheckAcks(ctx context.Context, killToken string, expectedNodes []string, fsNamespace, fsName string) (*AckStatus, error) {
	if len(expectedNodes) == 0 {
		return &AckStatus{AllAcked: true}, nil
	}

	cm := &corev1.ConfigMap{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Name:      ConfigMapName,
		Namespace: s.Namespace,
	}, cm); err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap was deleted — treat all nodes as pending so timeout logic can fire.
			return &AckStatus{PendingNodes: expectedNodes}, nil
		}
		return nil, fmt.Errorf("getting kill signal configmap: %w", err)
	}

	status := &AckStatus{}
	for _, nodeName := range expectedNodes {
		ak := ackKey(nodeName, fsNamespace, fsName)
		ackData, ok := cm.Data[ak]
		if !ok {
			status.PendingNodes = append(status.PendingNodes, nodeName)
			continue
		}

		var ack KillAck
		if err := json.Unmarshal([]byte(ackData), &ack); err != nil {
			status.PendingNodes = append(status.PendingNodes, nodeName)
			continue
		}

		// Check token match
		if ack.Token != killToken {
			status.PendingNodes = append(status.PendingNodes, nodeName)
			continue
		}

		if ack.Errors > 0 {
			status.ErrorNodes = append(status.ErrorNodes, nodeName)
		} else {
			status.AckedNodes = append(status.AckedNodes, nodeName)
		}
	}

	status.AllAcked = len(status.PendingNodes) == 0 && len(status.ErrorNodes) == 0
	return status, nil
}

// CleanupSignals removes kill/ack entries for a given FailoverService identity.
// It is best-effort and safe to call when no entries exist.
func (s *Signaler) CleanupSignals(ctx context.Context, fsNamespace, fsName string) error {
	isRetryable := func(err error) bool {
		return errors.IsConflict(err)
	}
	return retry.OnError(retry.DefaultRetry, isRetryable, func() error {
		cm := &corev1.ConfigMap{}
		err := s.Client.Get(ctx, types.NamespacedName{
			Name:      ConfigMapName,
			Namespace: s.Namespace,
		}, cm)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("getting kill signal configmap for cleanup: %w", err)
		}
		if len(cm.Data) == 0 {
			return nil
		}

		toDelete := make(map[string]struct{})
		for key, data := range cm.Data {
			switch {
			case strings.HasPrefix(key, "kill_"):
				if signalMatchesFS(key, data, fsNamespace, fsName) {
					toDelete[key] = struct{}{}
					// ACK key shares the same suffix by design.
					toDelete["ack_"+strings.TrimPrefix(key, "kill_")] = struct{}{}
				}
			case strings.HasPrefix(key, "ack_"):
				if ackMatchesFS(key, data, fsNamespace, fsName) {
					toDelete[key] = struct{}{}
				}
			}
		}

		if len(toDelete) == 0 {
			return nil
		}
		for key := range toDelete {
			delete(cm.Data, key)
		}
		return s.Client.Update(ctx, cm)
	})
}

func (s *Signaler) populateKillSignals(cm *corev1.ConfigMap, nodeNames []string, destIP string, killToken string, fsNamespace, fsName string) {
	for _, nodeName := range nodeNames {
		key := killKey(nodeName, fsNamespace, fsName)
		seq := int64(1)
		if existing, ok := cm.Data[key]; ok {
			var signal KillSignal
			if err := json.Unmarshal([]byte(existing), &signal); err == nil {
				seq = signal.Seq + 1
			}
		}
		signalData := KillSignal{
			IP:          destIP,
			Seq:         seq,
			Token:       killToken,
			Node:        nodeName,
			FSNamespace: fsNamespace,
			FSName:      fsName,
		}
		data, _ := json.Marshal(signalData)
		cm.Data[key] = string(data)
	}
}

func signalMatchesFS(key, signalData, fsNamespace, fsName string) bool {
	var signal KillSignal
	if err := json.Unmarshal([]byte(signalData), &signal); err == nil {
		if signal.FSNamespace != "" || signal.FSName != "" {
			return signal.FSNamespace == fsNamespace && signal.FSName == fsName
		}
	}
	// Backward compatibility for old entries without fsNamespace/fsName in payload.
	return strings.HasSuffix(key, "_"+fsNamespace+"_"+fsName)
}

func ackMatchesFS(key, ackData, fsNamespace, fsName string) bool {
	var ack KillAck
	if err := json.Unmarshal([]byte(ackData), &ack); err == nil {
		if ack.FSNamespace != "" || ack.FSName != "" {
			return ack.FSNamespace == fsNamespace && ack.FSName == fsName
		}
	}
	// Backward compatibility for old entries without fsNamespace/fsName in payload.
	return strings.HasSuffix(key, "_"+fsNamespace+"_"+fsName)
}

func (s *Signaler) discoverAgentNodes(ctx context.Context) ([]string, error) {
	agentPods := &corev1.PodList{}
	if err := s.Client.List(
		ctx,
		agentPods,
		client.InNamespace(s.Namespace),
		client.MatchingLabels{agentLabelKey: agentLabelVal},
	); err != nil {
		return nil, fmt.Errorf("listing connection-killer pods: %w", err)
	}

	if len(agentPods.Items) == 0 {
		signalLog.Info("No connection-killer agent pods discovered; skipping connection kill signal")
		return nil, nil
	}

	readyAgentNodes := make(map[string]struct{})
	for _, pod := range agentPods.Items {
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning || !isPodReady(pod.Status.Conditions) {
			continue
		}
		readyAgentNodes[pod.Spec.NodeName] = struct{}{}
	}

	nodes := &corev1.NodeList{}
	if err := s.Client.List(
		ctx,
		nodes,
		client.MatchingLabels{corev1.LabelOSStable: "linux"},
	); err != nil {
		return nil, fmt.Errorf("listing connection-killer target nodes: %w", err)
	}

	eligibleNodeNames := make([]string, 0, len(nodes.Items))
	missingReadyAgents := make([]string, 0)
	for _, node := range nodes.Items {
		if !isNodeEligible(node) {
			continue
		}
		eligibleNodeNames = append(eligibleNodeNames, node.Name)
		if _, ok := readyAgentNodes[node.Name]; !ok {
			missingReadyAgents = append(missingReadyAgents, node.Name)
		}
	}

	if len(eligibleNodeNames) == 0 {
		signalLog.Info(
			"No eligible Linux nodes found for connection kill",
			"podsFound", strconv.Itoa(len(agentPods.Items)),
			"nodesFound", strconv.Itoa(len(nodes.Items)),
		)
		return nil, nil
	}

	sort.Strings(eligibleNodeNames)
	if len(missingReadyAgents) > 0 {
		sort.Strings(missingReadyAgents)
		signalLog.Info(
			"Connection-killer agents missing or unready on eligible nodes; waiting for ACK timeout if they do not recover",
			"eligibleNodes", eligibleNodeNames,
			"missingReadyAgents", missingReadyAgents,
		)
	}

	return eligibleNodeNames, nil
}

func isPodReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isNodeEligible(node corev1.Node) bool {
	if node.DeletionTimestamp != nil || node.Spec.Unschedulable {
		return false
	}
	if node.Labels[corev1.LabelOSStable] != "linux" {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return true
}
