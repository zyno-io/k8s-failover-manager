package remotecluster

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
	"github.com/zyno-io/k8s-failover-manager/internal/metrics"
)

const (
	// DefaultSyncTimeout is the maximum time to wait for a remote sync operation.
	DefaultSyncTimeout = 30 * time.Second
)

// SyncResult describes the outcome of a sync operation.
type SyncResult struct {
	// RemoteWins is true if the remote cluster's state was adopted.
	RemoteWins bool
	// RemoteState is the last known state of the remote cluster.
	RemoteState *failoverv1alpha1.RemoteState
}

// Syncer handles push-based state sync with the remote cluster.
type Syncer struct {
	ClientProvider *ClientProvider
	ClusterID      string
	SyncTimeout    time.Duration
}

func (s *Syncer) syncTimeout() time.Duration {
	if s.SyncTimeout > 0 {
		return s.SyncTimeout
	}
	return DefaultSyncTimeout
}

// Sync pushes local state to the remote cluster or adopts remote state if it wins.
// Returns a SyncResult indicating who won and the remote state.
func (s *Syncer) Sync(ctx context.Context, local *failoverv1alpha1.FailoverService) (*SyncResult, error) {
	start := time.Now()
	defer func() {
		metrics.RemoteSyncDuration.Observe(time.Since(start).Seconds())
	}()

	// Apply timeout to prevent blocking the reconcile loop
	syncCtx, cancel := context.WithTimeout(ctx, s.syncTimeout())
	defer cancel()

	remoteClient, err := s.ClientProvider.GetClient(syncCtx)
	if err != nil {
		return nil, fmt.Errorf("getting remote client: %w", err)
	}

	remote := &failoverv1alpha1.FailoverService{}
	err = remoteClient.Get(syncCtx, types.NamespacedName{
		Name:      local.Name,
		Namespace: local.Namespace,
	}, remote)
	if err != nil {
		return nil, fmt.Errorf("getting remote FailoverService: %w", err)
	}

	now := metav1.Now()
	result := &SyncResult{
		RemoteState: &failoverv1alpha1.RemoteState{
			FailoverActive:   remote.Spec.FailoverActive,
			ActiveCluster:    remote.Status.ActiveCluster,
			ActiveGeneration: remote.Status.ActiveGeneration,
			LastSyncTime:     &now,
			ClusterID:        s.remoteClusterID(remote),
			SpecChangeTime:   remote.Status.SpecChangeTime,
		},
	}

	// If both clusters agree on failoverActive, no conflict to resolve
	if remote.Spec.FailoverActive == local.Spec.FailoverActive {
		return result, nil
	}

	// Conflict resolution: higher activeGeneration wins
	if remote.Status.ActiveGeneration > local.Status.ActiveGeneration {
		result.RemoteWins = true
		return result, nil
	}

	if remote.Status.ActiveGeneration == local.Status.ActiveGeneration {
		// Tiebreaker 1: compare specChangeTime — more recent wins
		remoteTime := remote.Status.SpecChangeTime
		localTime := local.Status.SpecChangeTime
		if remoteTime != nil && localTime != nil {
			if remoteTime.After(localTime.Time) {
				result.RemoteWins = true
				return result, nil
			}
			if localTime.After(remoteTime.Time) {
				// Local wins — fall through to push
			} else {
				// Timestamps equal — fall back to cluster ID comparison
				remoteID := s.remoteClusterID(remote)
				if remoteID > s.ClusterID {
					result.RemoteWins = true
					return result, nil
				}
			}
		} else {
			// One or both timestamps nil — fall back to cluster ID comparison
			remoteID := s.remoteClusterID(remote)
			if remoteID > s.ClusterID {
				result.RemoteWins = true
				return result, nil
			}
		}
	}

	// Local wins — push our failoverActive to remote
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		remote := &failoverv1alpha1.FailoverService{}
		if err := remoteClient.Get(syncCtx, types.NamespacedName{
			Name:      local.Name,
			Namespace: local.Namespace,
		}, remote); err != nil {
			return err
		}
		remote.Spec.FailoverActive = local.Spec.FailoverActive
		return remoteClient.Update(syncCtx, remote)
	}); err != nil {
		if errors.IsConflict(err) {
			return nil, fmt.Errorf("pushing state to remote (conflict): %w", err)
		}
		return nil, fmt.Errorf("pushing state to remote: %w", err)
	}

	// Reflect pushed state immediately for accurate status reporting.
	result.RemoteState.FailoverActive = local.Spec.FailoverActive
	result.RemoteState.LastSyncTime = &now

	log.Info("Pushed state to remote cluster",
		"name", local.Name,
		"namespace", local.Namespace,
		"failoverActive", local.Spec.FailoverActive,
		"activeGeneration", local.Status.ActiveGeneration,
		"activeCluster", local.Status.ActiveCluster,
	)

	return result, nil
}

// remoteClusterID extracts the cluster ID from the remote CR's annotation.
func (s *Syncer) remoteClusterID(remote *failoverv1alpha1.FailoverService) string {
	if id, ok := remote.Annotations["k8s-failover.zyno.io/cluster-id"]; ok {
		return id
	}
	return ""
}

// SetClusterIDAnnotation is a helper for the controller to set its own cluster ID.
func SetClusterIDAnnotation(obj client.Object, clusterID string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["k8s-failover.zyno.io/cluster-id"] = clusterID
	obj.SetAnnotations(annotations)
}
