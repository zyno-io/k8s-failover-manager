# Remote Clusters

k8s-failover-manager synchronizes failover state between two clusters. The controller on each cluster pushes its state to the remote cluster and resolves conflicts using timestamps and monotonic revision numbers.

## Prerequisites

- **NTP synchronization** — Both clusters must have synchronized clocks (e.g., via NTP or chrony). Timestamp-based conflict resolution relies on accurate timestamps. Clock skew beyond a few seconds can lead to incorrect tiebreaking.

## Setup

### 1. Create a Kubeconfig Secret

On each cluster, create a Secret containing the kubeconfig for the **other** cluster:

```bash
# On the primary cluster, create a secret with the failover cluster's kubeconfig
kubectl create secret generic remote-kubeconfig \
  --namespace k8s-failover-manager-system \
  --from-file=kubeconfig=/path/to/failover-cluster-kubeconfig.yaml

# On the failover cluster, create a secret with the primary cluster's kubeconfig
kubectl create secret generic remote-kubeconfig \
  --namespace k8s-failover-manager-system \
  --from-file=kubeconfig=/path/to/primary-cluster-kubeconfig.yaml
```

The Secret must contain a key named `kubeconfig` with a valid kubeconfig file.

### 2. Configure the Helm Chart

On each cluster, set the remote kubeconfig values:

```yaml
failoverConfig:
  clusterRole: primary            # "primary" on primary, "failover" on failover
  clusterId: cluster-1            # Unique ID for this cluster
  remoteKubeconfigSecret: remote-kubeconfig
  remoteKubeconfigNamespace: k8s-failover-manager-system
```

::: warning
Both clusters **must** have unique `clusterId` values. If the cluster IDs are the same, conflict resolution will produce unpredictable results.
:::

All three fields — `clusterId`, `remoteKubeconfigSecret`, and `remoteKubeconfigNamespace` — are required. The controller will fail to start if any are missing.

## How Sync Works

### Push-Based Model

The controller pushes state to the remote cluster rather than pulling. On each reconcile:

1. The controller reads the remote `FailoverService` by the same name and namespace
2. Compares `spec.failoverActive` between local and remote
3. If they agree, no action is needed
4. If they disagree, conflict resolution determines which cluster wins

### Conflict Resolution

When the local and remote `spec.failoverActive` values differ:

1. **Higher `activeGeneration` wins** — each completed transition increments the `activeGeneration` counter in status, so the cluster that most recently completed a transition takes precedence
2. **On generation tie: more recent `specChangeTime` wins** — the cluster whose `spec.failoverActive` changed more recently takes precedence
3. **On timestamp tie: lexicographic cluster ID comparison** — the cluster with the lexicographically higher ID wins (deterministic tiebreaker)

If the **local** cluster wins, the controller pushes its `spec.failoverActive` value to the remote cluster using `retry.RetryOnConflict`.

If the **remote** cluster wins, the controller adopts the remote state by updating the local `spec.failoverActive` and status.

### Sync Timing

State synchronization happens at two points:

- **Steady state** — every 60 seconds during the anti-entropy reconcile loop
- **After transition** — immediately after a transition completes

### Cluster ID Annotation

Each controller stamps the `k8s-failover.zyno.io/cluster-id` annotation on its `FailoverService` CR. This is used by the remote controller to identify which cluster the state came from.

## Client Caching

The remote cluster client is cached and reused across reconcile loops. The cache is invalidated when the kubeconfig Secret's `resourceVersion` changes, so you can rotate kubeconfigs without restarting the controller.

## RBAC

When `remoteKubeconfigSecret` is configured, the Helm chart creates a namespaced `Role` and `RoleBinding` in `remoteKubeconfigNamespace`, scoped to `get` only the specific named Secret.

The kubeconfig must grant the controller permissions to:

- `get` and `update` `FailoverService` resources in the target namespace on the remote cluster

## Status Fields

The sync state is reflected in:

```yaml
status:
  specChangeTime: "2024-01-15T10:29:55Z"
  remoteState:
    failoverActive: false
    activeGeneration: 3
    specChangeTime: "2024-01-15T10:29:50Z"
    lastSyncTime: "2024-01-15T10:30:00Z"
  conditions:
    - type: RemoteSynced
      status: "True"
```

- `specChangeTime` — when the local `spec.failoverActive` last changed (used for timestamp-based conflict resolution)
- `remoteState.specChangeTime` — the remote cluster's equivalent timestamp
