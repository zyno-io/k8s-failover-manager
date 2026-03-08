# k8s-failover-manager

A Kubernetes operator that orchestrates automated failover for DNS-based services across two clusters. It manages traffic switching between primary and failover clusters while executing coordinated pre/post-transition actions such as HTTP calls, Kubernetes Jobs, scaling operations, and readiness checks.

## Features

- **Automated failover orchestration** between primary and failover clusters via a state machine
- **Pluggable transition actions** — HTTP requests, Kubernetes Jobs, Deployment/StatefulSet scaling, readiness checks, remote cluster coordination
- **Connection killing** — graceful draining of stale TCP connections during transitions via a DaemonSet agent using netlink
- **Multi-cluster state sync** — push-based synchronization with timestamp-based conflict resolution
- **Transition history** — `FailoverEvent` resources record every transition with outcome, timing, and action results
- **Prometheus metrics** — transition counts, durations, action results, sync timing, and more
- **Force-advance** — operator escape hatch to unstick a stalled transition pipeline
- **Dual addressing** — each cluster can expose different addresses in primary vs. failover mode (DNS names or IPs)

## How It Works

The operator manages a `FailoverService` custom resource that controls a Kubernetes Service. When `spec.failoverActive` changes, a transition state machine executes:

```
Idle → ExecutingPreActions → UpdatingResources → FlushingConnections → ExecutingPostActions → Idle
```

1. **Pre-actions** run (e.g. drain a database, promote a replica)
2. **Service is updated** to point to the new target address
3. **Stale connections are killed** via the connection-killer DaemonSet
4. **Post-actions** run (e.g. scale down old workers, send a Slack notification)

Each cluster runs its own instance of the operator with its own set of transition hooks. The two clusters sync state via a remote kubeconfig, using timestamps and monotonic revisions for conflict resolution.

## Architecture

```
┌─────────────────────────────────┐     ┌─────────────────────────────────┐
│         Primary Cluster         │     │        Failover Cluster         │
│                                 │     │                                 │
│  ┌───────────────────────────┐  │     │  ┌───────────────────────────┐  │
│  │    Controller Manager     │◄─┼─────┼─►│    Controller Manager     │  │
│  │  (CLUSTER_ROLE=primary)   │  │     │  │  (CLUSTER_ROLE=failover)  │  │
│  └─────────┬─────────────────┘  │     │  └─────────┬─────────────────┘  │
│            │                    │     │            │                    │
│  ┌─────────▼─────────────────┐  │     │  ┌─────────▼─────────────────┐  │
│  │   FailoverService CR      │  │     │  │   FailoverService CR      │  │
│  │   + Managed Service       │  │     │  │   + Managed Service       │  │
│  └───────────────────────────┘  │     │  └───────────────────────────┘  │
│                                 │     │                                 │
│  ┌───────────────────────────┐  │     │  ┌───────────────────────────┐  │
│  │  Connection-Killer        │  │     │  │  Connection-Killer        │  │
│  │  DaemonSet (per node)     │  │     │  │  DaemonSet (per node)     │  │
│  └───────────────────────────┘  │     │  └───────────────────────────┘  │
└─────────────────────────────────┘     └─────────────────────────────────┘
```

**Components:**

| Component | Description |
|---|---|
| **Controller Manager** | Deployment that watches `FailoverService` resources and orchestrates transitions |
| **Connection-Killer** | Privileged DaemonSet that kills TCP connections to old target IPs using netlink |
| **FailoverService CRD** | Custom resource defining service name, ports, cluster targets, and transition hooks |
| **FailoverEvent CRD** | Records transition history with outcome, timing, and action results |

## Getting Started

### Prerequisites

- Go 1.26.1+
- Docker 17.03+
- kubectl
- Access to two Kubernetes clusters (primary and failover)

### Installation

**Helm**

```sh
# Build and push images
make docker-build docker-push IMG=<registry>/k8s-failover-manager:latest
make docker-build-connection-killer docker-push-connection-killer CONN_KILLER_IMG=<registry>/connection-killer:latest

# Install via Helm
helm install k8s-failover-manager charts/k8s-failover-manager \
  --namespace k8s-failover-manager-system --create-namespace \
  --set image.repository=<registry>/k8s-failover-manager \
  --set image.tag=latest \
  --set connectionKiller.image.repository=<registry>/connection-killer \
  --set connectionKiller.image.tag=latest \
  --set failoverConfig.clusterRole=primary \
  --set failoverConfig.clusterId=us-east-1
```

### Configuration

The controller is configured via a ConfigMap named `<release-name>-config` in the controller namespace (for example, `k8s-failover-manager-config`):

| Key | Description | Required |
|---|---|---|
| `cluster-role` | `primary` or `failover` | Yes |
| `cluster-id` | Unique identifier for this cluster (e.g. `us-east-1`) | Yes |
| `remote-kubeconfig-secret` | Name of a Secret containing the remote cluster's kubeconfig | Yes |
| `remote-kubeconfig-namespace` | Namespace of the remote kubeconfig Secret | Yes |

Example:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: k8s-failover-manager-config
  namespace: k8s-failover-manager-system
data:
  cluster-role: "primary"
  cluster-id: "us-east-1"
  remote-kubeconfig-secret: "remote-cluster-kubeconfig"
  remote-kubeconfig-namespace: "k8s-failover-manager-system"
```

For multi-cluster sync, create a Secret containing the remote cluster's kubeconfig:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: remote-cluster-kubeconfig
  namespace: k8s-failover-manager-system
type: Opaque
data:
  kubeconfig: <base64-encoded-kubeconfig>
```

## Usage

### FailoverService Custom Resource

```yaml
apiVersion: k8s-failover.zyno.io/v1alpha1
kind: FailoverService
metadata:
  name: mysql-failover
  namespace: default
spec:
  serviceName: mysql-dns-alias
  ports:
    - name: mysql
      port: 3306
      protocol: TCP

  failoverActive: false

  primaryCluster:
    primaryModeAddress: "mysql-primary.mysql.svc.cluster.local"
    failoverModeAddress: "192.168.50.60" # remote cluster; may be a LoadBalancer IP, NodePort, etc.
    onFailover:
      preActions:
        - name: drain-primary
          http:
            url: "http://mysql-primary.mysql.svc.cluster.local:9090/drain"
            method: POST
      postActions:
        - name: scale-down-workers
          scale:
            kind: Deployment
            name: mysql-workers
            replicas: 0
    onFailback:
      preActions:
        - name: scale-up-workers
          scale:
            kind: Deployment
            name: mysql-workers
            replicas: 3
        - name: wait-workers-ready
          waitReady:
            kind: Deployment
            name: mysql-workers
            timeoutSeconds: 120

  failoverCluster:
    primaryModeAddress: "10.20.30.40" # remote cluster; may be a LoadBalancer IP, NodePort, etc.
    failoverModeAddress: "mysql-failover.mysql.svc.cluster.local"
    onFailover:
      preActions:
        - name: promote-replica
          job:
            image: "mysql:8.0"
            serviceAccountName: mysql-admin
            script: |
              mysql -h localhost -e "STOP REPLICA; RESET REPLICA ALL;"
            activeDeadlineSeconds: 120
            maxRetries: 2
      postActions:
        - name: notify-slack
          http:
            url: "https://hooks.slack.com/services/XXX"
            method: POST
            headers:
              Content-Type: "application/json"
            body: '{"text":"Failover complete"}'
          ignoreFailure: true
```

### Triggering a Failover

Toggle `spec.failoverActive` to initiate a transition:

```sh
# Failover
kubectl patch failoverservice mysql-failover --type=merge -p '{"spec":{"failoverActive":true}}'

# Failback
kubectl patch failoverservice mysql-failover --type=merge -p '{"spec":{"failoverActive":false}}'
```

### Monitoring Transition Progress

```sh
kubectl get failoverservice mysql-failover
```

```
NAME             SERVICE          FAILOVER ACTIVE   PHASE                  REVISION   AGE
mysql-failover   mysql-dns-alias  true              ExecutingPreActions    3          5m
```

### Force-Advancing a Stuck Transition

If a transition gets stuck (e.g. a Job times out), you can force-advance past the current phase:

```sh
kubectl annotate failoverservice mysql-failover k8s-failover.zyno.io/force-advance=true
```

### Transition Actions

Each action type supports different use cases:

| Action | Use Case | Execution |
|---|---|---|
| `http` | Webhooks, API calls, drain endpoints | Synchronous; fails on non-2xx |
| `job` | Database promotion, migrations, scripts | Async; creates a K8s Job and polls for completion |
| `scale` | Scale Deployments/StatefulSets up or down | Synchronous (or async with `waitReady: true`) |
| `waitReady` | Wait for replicas to become ready | Polls until ready or timeout (default 300s) |
| `waitRemote` | Wait for remote cluster phase/action | Polls remote until condition met or timeout |

Actions run sequentially within each phase. Set `ignoreFailure: true` on an action to continue the pipeline even if it fails.

### Address Resolution

The managed Service is pointed to different addresses depending on the cluster role and failover state:

| Cluster Role | `failoverActive: false` | `failoverActive: true` |
|---|---|---|
| **primary** | `primaryCluster.primaryModeAddress` | `primaryCluster.failoverModeAddress` |
| **failover** | `failoverCluster.primaryModeAddress` | `failoverCluster.failoverModeAddress` |

- DNS name addresses create an **ExternalName** Service
- IP addresses create a **ClusterIP** Service with a managed EndpointSlice

## Metrics

The operator exposes Prometheus metrics on the metrics endpoint:

| Metric | Type | Description |
|---|---|---|
| `failover_transitions_total` | Counter | Total transitions by name, namespace, direction |
| `failover_transition_duration_seconds` | Histogram | End-to-end transition duration |
| `failover_active` | Gauge | Current failover state (0 or 1) |
| `failover_action_executions_total` | Counter | Action executions by type and result |
| `failover_action_execution_duration_seconds` | Histogram | Individual action duration |
| `failover_connection_kill_phases_total` | Counter | Connection kill phases completed (all agents ACKed) |
| `failover_remote_sync_duration_seconds` | Histogram | Remote cluster sync duration |
| `failover_reconcile_errors_total` | Counter | Reconcile errors by phase |

## Security Notes

- Treat `FailoverService` write access as privileged. A user who can create or update a `FailoverService` can:
  - Trigger outbound HTTP calls from the controller (`http` actions)
  - Execute arbitrary containers/scripts in-cluster (`job` actions)
  - Scale workloads (`scale` actions)
- Restrict who can write `FailoverService` resources and which namespaces they can target.
- The Helm chart grants Secrets `get` RBAC scoped to the specific Secret named in `failoverConfig.remoteKubeconfigSecret`. Changing the remote sync Secret name requires a `helm upgrade` to update the RBAC rule.
- `ServiceMonitor` now defaults to TLS verification (`serviceMonitor.tlsConfig.insecureSkipVerify: false`). Only set it to `true` when required by your cert setup.

## Development

```sh
# Run unit tests
make test

# Run linter
make lint

# Run locally against current kubeconfig
make run

# Run e2e tests (requires Kind)
make test-e2e

# Regenerate CRDs and RBAC after editing types or markers
make manifests generate
```

## Uninstall

```sh
# Delete FailoverService instances
kubectl delete failoverservices --all --all-namespaces

# Remove the release
helm uninstall k8s-failover-manager -n k8s-failover-manager-system

# Remove CRDs (not removed by Helm by design)
make uninstall
```

## License

This project is licensed under the [MIT License](LICENSE).
