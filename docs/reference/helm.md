# Helm Chart Reference

The Helm chart installs the k8s-failover-manager controller, CRDs, RBAC, and optionally the connection killer DaemonSet.

## Installation

```bash
helm install k8s-failover-manager ./charts/k8s-failover-manager \
  --namespace k8s-failover-manager-system \
  --create-namespace \
  -f values.yaml
```

## Values

### Failover Configuration

| Parameter | Default | Description |
|-----------|---------|-------------|
| `failoverConfig.clusterRole` | `primary` | Role of this cluster: `primary` or `failover`. |
| `failoverConfig.clusterId` | `""` | **Required.** Unique identifier for this cluster. Must be different across clusters. |
| `failoverConfig.remoteKubeconfigSecret` | `""` | **Required.** Name of the Secret containing the remote cluster's kubeconfig. |
| `failoverConfig.remoteKubeconfigNamespace` | `""` | **Required.** Namespace of the remote kubeconfig Secret. |

### Controller Image

| Parameter | Default | Description |
|-----------|---------|-------------|
| `image.repository` | `controller` | Container image repository. |
| `image.tag` | `latest` | Image tag. |
| `image.digest` | `""` | Image digest. Overrides `tag` when set. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy. |

### Controller Resources

| Parameter | Default | Description |
|-----------|---------|-------------|
| `resources.limits.cpu` | `500m` | CPU limit. |
| `resources.limits.memory` | `128Mi` | Memory limit. |
| `resources.requests.cpu` | `10m` | CPU request. |
| `resources.requests.memory` | `64Mi` | Memory request. |

### Connection Killer

| Parameter | Default | Description |
|-----------|---------|-------------|
| `connectionKiller.enabled` | `true` | Deploy the connection killer DaemonSet. |
| `connectionKiller.image.repository` | `connection-killer` | Connection killer image repository. |
| `connectionKiller.image.tag` | `latest` | Connection killer image tag. |
| `connectionKiller.image.digest` | `""` | Connection killer image digest. |
| `connectionKiller.image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `connectionKiller.resources.limits.cpu` | `100m` | CPU limit. |
| `connectionKiller.resources.limits.memory` | `64Mi` | Memory limit. |
| `connectionKiller.resources.requests.cpu` | `10m` | CPU request. |
| `connectionKiller.resources.requests.memory` | `32Mi` | Memory request. |

### Replica Count and Leader Election

| Parameter | Default | Description |
|-----------|---------|-------------|
| `replicaCount` | `1` | Number of controller replicas. |
| `leaderElection.enabled` | `true` | Enable leader election. Required when `replicaCount > 1`. |

### Metrics

| Parameter | Default | Description |
|-----------|---------|-------------|
| `metrics.enabled` | `true` | Enable the metrics endpoint. |
| `metrics.port` | `8443` | Metrics server port (HTTPS). |

### ServiceMonitor (Prometheus Operator)

| Parameter | Default | Description |
|-----------|---------|-------------|
| `serviceMonitor.enabled` | `false` | Create a ServiceMonitor resource. |
| `serviceMonitor.labels` | `{}` | Additional labels for the ServiceMonitor. |
| `serviceMonitor.tlsConfig.insecureSkipVerify` | `false` | Skip TLS certificate verification. |
| `serviceMonitor.tlsConfig.caFile` | `""` | Path to the CA certificate file. |
| `serviceMonitor.tlsConfig.serverName` | `""` | Expected server name for TLS verification. |

## Generated Resources

The chart creates the following Kubernetes resources:

| Resource | Name | Description |
|----------|------|-------------|
| Deployment | `k8s-failover-manager-controller-manager` | Controller manager. |
| DaemonSet | `k8s-failover-manager-connection-killer` | Connection killer (if enabled). |
| ConfigMap | `k8s-failover-manager-config` | Controller configuration. |
| ServiceAccount | `k8s-failover-manager-controller-manager` | Controller service account. |
| ServiceAccount | `k8s-failover-manager-connection-killer` | Connection killer service account (if enabled). |
| ClusterRole | `k8s-failover-manager-manager-role` | Controller RBAC. |
| ClusterRoleBinding | `k8s-failover-manager-manager-rolebinding` | Controller RBAC binding. |
| Role | `k8s-failover-manager-leader-election-role` | Leader election RBAC. |
| RoleBinding | `k8s-failover-manager-leader-election-rolebinding` | Leader election RBAC binding. |
| Role | `k8s-failover-manager-remote-kubeconfig` | Namespaced read access to the configured remote kubeconfig Secret. |
| RoleBinding | `k8s-failover-manager-remote-kubeconfig` | Binds the controller service account to the remote kubeconfig Secret Role. |
| Role | `k8s-failover-manager-connection-killer-role` | Connection killer ConfigMap access (if enabled). |
| RoleBinding | `k8s-failover-manager-connection-killer-rolebinding` | Connection killer RBAC binding (if enabled). |
| Service | `k8s-failover-manager-controller-manager-metrics-service` | Metrics endpoint. |
| ServiceMonitor | `k8s-failover-manager-controller-manager-metrics-monitor` | Prometheus ServiceMonitor (if enabled). |
| CRD | `failoverservices.k8s-failover.zyno.io` | FailoverService CRD (installed from `crds/`). |
| CRD | `failoverevents.k8s-failover.zyno.io` | FailoverEvent CRD for transition history (installed from `crds/`). |

## Security

The controller runs with a hardened security context:

- `readOnlyRootFilesystem: true`
- `allowPrivilegeEscalation: false`
- `runAsNonRoot: true`
- All capabilities dropped

The connection killer requires elevated privileges:

- `hostPID: true`
- Capabilities: `NET_ADMIN`, `SYS_ADMIN`
- Runs as root (UID 0)

Remote kubeconfig access is granted through a namespaced `Role` and `RoleBinding` in `remoteKubeconfigNamespace`, scoped to `get` only the specific named Secret specified in `remoteKubeconfigSecret`.

## Example Values

### Minimal

```yaml
failoverConfig:
  clusterRole: primary
  clusterId: prod-us-east-1
  remoteKubeconfigSecret: remote-kubeconfig
  remoteKubeconfigNamespace: k8s-failover-manager-system

image:
  repository: ghcr.io/zyno-io/k8s-failover-manager
  tag: v1.0.0

connectionKiller:
  image:
    repository: ghcr.io/zyno-io/k8s-failover-manager-connection-killer
    tag: v1.0.0
```

### With Monitoring

```yaml
failoverConfig:
  clusterRole: primary
  clusterId: prod-us-east-1
  remoteKubeconfigSecret: failover-cluster-kubeconfig
  remoteKubeconfigNamespace: k8s-failover-manager-system

image:
  repository: ghcr.io/zyno-io/k8s-failover-manager
  tag: v1.0.0

connectionKiller:
  image:
    repository: ghcr.io/zyno-io/k8s-failover-manager-connection-killer
    tag: v1.0.0

serviceMonitor:
  enabled: true
  labels:
    release: prometheus
  tlsConfig:
    insecureSkipVerify: true
```
