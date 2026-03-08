//go:build linux

package connkill

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var agentLog = logf.Log.WithName("connkill-agent")

// Agent watches the connection-kill-signal ConfigMap and destroys matching connections.
type Agent struct {
	Client    client.Client
	Clientset kubernetes.Interface
	NodeName  string
	Namespace string

	// lastSeq tracks the last processed sequence number per kill-key suffix.
	lastSeq map[string]int64
}

// RunOnce checks the ConfigMap for new kill signals and processes them.
// It scans all kill keys and selects entries for this node.
// New-format signals include the target node in payload; legacy entries rely on key prefix matching.
func (a *Agent) RunOnce(ctx context.Context) error {
	cm := &corev1.ConfigMap{}
	if err := a.Client.Get(ctx, types.NamespacedName{
		Name:      ConfigMapName,
		Namespace: a.Namespace,
	}, cm); err != nil {
		if errors.IsNotFound(err) {
			return nil // ConfigMap not yet created; nothing to do
		}
		return fmt.Errorf("getting kill signal configmap: %w", err)
	}

	legacyNodePrefix := "kill_" + a.NodeName + "_"

	for key, signalData := range cm.Data {
		if !strings.HasPrefix(key, "kill_") {
			continue
		}

		var signal KillSignal
		if err := json.Unmarshal([]byte(signalData), &signal); err != nil {
			agentLog.Error(err, "Failed to unmarshal kill signal", "key", key)
			continue
		}

		// Prefer explicit node targeting when available.
		if signal.Node != "" {
			if signal.Node != a.NodeName {
				continue
			}
		} else if !strings.HasPrefix(key, legacyNodePrefix) {
			// Backward compatibility: legacy entries did not include signal.Node.
			continue
		}

		if a.lastSeq == nil {
			a.lastSeq = make(map[string]int64)
		}

		seqKey := strings.TrimPrefix(key, "kill_")
		if signal.Seq <= a.lastSeq[seqKey] {
			continue // already processed
		}

		agentLog.Info("Processing kill signal", "ip", signal.IP, "seq", signal.Seq, "node", a.NodeName, "key", key)

		destIP := net.ParseIP(signal.IP)
		if destIP == nil {
			agentLog.Error(nil, "Invalid IP in kill signal", "ip", signal.IP, "key", key)
			continue
		}

		killed, errCount, err := KillConnectionsAllNamespaces(destIP)
		if err != nil {
			agentLog.Error(err, "Error killing connections", "ip", signal.IP)
			errCount++
		}

		agentLog.Info("Kill complete", "ip", signal.IP, "killed", killed, "errors", errCount)

		// Write ACK using the exact key suffix so CheckAcks can match deterministically.
		ackKey := "ack_" + seqKey
		if err := a.writeAck(ctx, ackKey, signal.Seq, signal.Token, signal.FSNamespace, signal.FSName, killed, errCount); err != nil {
			agentLog.Error(err, "Failed to write ACK", "key", ackKey)
			// Do not advance lastSeq unless ACK persistence succeeded; otherwise
			// we'd suppress retries and the controller could time out waiting.
			continue
		}
		a.lastSeq[seqKey] = signal.Seq
	}

	return nil
}

func (a *Agent) writeAck(ctx context.Context, ackKey string, seq int64, token, fsNamespace, fsName string, killed, errCount int) error {
	ack := KillAck{
		Seq:         seq,
		Token:       token,
		Node:        a.NodeName,
		FSNamespace: fsNamespace,
		FSName:      fsName,
		Killed:      killed,
		Errors:      errCount,
	}
	data, err := json.Marshal(ack)
	if err != nil {
		return fmt.Errorf("marshaling ack: %w", err)
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		if err := a.Client.Get(ctx, types.NamespacedName{
			Name:      ConfigMapName,
			Namespace: a.Namespace,
		}, cm); err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[ackKey] = string(data)
		return a.Client.Update(ctx, cm)
	})
}

// initFromConfigMap seeds lastSeq from ACK entries (already-processed work) so that
// signals that were completed before a restart are not replayed, while in-flight
// signals that have not yet been ACKed are still processed.
func (a *Agent) initFromConfigMap(ctx context.Context) {
	cm := &corev1.ConfigMap{}
	if err := a.Client.Get(ctx, types.NamespacedName{
		Name:      ConfigMapName,
		Namespace: a.Namespace,
	}, cm); err != nil {
		// ConfigMap may not exist yet — that's fine.
		return
	}

	// Seed from ACK keys, not kill keys. ACKs represent completed work;
	// kill signals without a matching ACK still need processing.
	prefix := "ack_"
	a.lastSeq = make(map[string]int64)

	for key, ackData := range cm.Data {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		seqKey := strings.TrimPrefix(key, prefix)
		var ack KillAck
		if err := json.Unmarshal([]byte(ackData), &ack); err != nil {
			continue
		}
		if ack.Node != "" {
			if ack.Node != a.NodeName {
				continue
			}
		} else if !strings.HasPrefix(seqKey, a.NodeName+"_") {
			// Backward compatibility for ACKs written before node field existed.
			continue
		}
		a.lastSeq[seqKey] = ack.Seq
	}

	if len(a.lastSeq) > 0 {
		agentLog.Info("Initialized lastSeq from existing ACKs", "entries", len(a.lastSeq))
	}
}

// Watch uses a Kubernetes Watch on the specific ConfigMap instead of polling.
// Falls back to the given interval for reconnection retries.
func (a *Agent) Watch(ctx context.Context, interval time.Duration) error {
	a.initFromConfigMap(ctx)

	// If no Clientset is available, fall back to ticker-based polling
	if a.Clientset == nil {
		return a.pollWatch(ctx, interval)
	}

	for {
		if err := a.watchOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			agentLog.Error(err, "Watch failed, reconnecting after interval")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
}

// watchOnce opens a Watch on the ConfigMap and processes events until the watch closes or context is cancelled.
func (a *Agent) watchOnce(ctx context.Context) error {
	fieldSelector := fields.OneTermEqualSelector("metadata.name", ConfigMapName).String()

	watcher, err := a.Clientset.CoreV1().ConfigMaps(a.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fieldSelector,
	})
	if err != nil {
		return fmt.Errorf("starting configmap watch: %w", err)
	}
	defer watcher.Stop()

	// Process immediately after reconnection to catch signals during disconnect
	if err := a.RunOnce(ctx); err != nil {
		agentLog.Error(err, "Error processing kill signal after reconnect")
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			if event.Type == watch.Modified || event.Type == watch.Added {
				if err := a.RunOnce(ctx); err != nil {
					agentLog.Error(err, "Error processing kill signal")
				}
			}
		}
	}
}

// pollWatch is the fallback ticker-based polling loop.
func (a *Agent) pollWatch(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := a.RunOnce(ctx); err != nil {
				agentLog.Error(err, "Error processing kill signal")
			}
		}
	}
}
