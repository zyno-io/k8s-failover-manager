# CRD Specification

**API Group:** `k8s-failover.zyno.io`
**Version:** `v1alpha1`
**Kind:** `FailoverService`

## Spec

### FailoverServiceSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `serviceName` | `string` | Yes | Name of the managed Kubernetes Service. Immutable after creation. Must be a valid DNS label. |
| `ports` | `[]ServicePort` | Yes | Port definitions for the managed Service. |
| `failoverActive` | `bool` | No | The failover toggle. Set to `true` to activate failover, `false` to restore primary. Defaults to `false`. |
| `primaryCluster` | `ClusterTarget` | Yes | Configuration for the primary cluster side. |
| `failoverCluster` | `ClusterTarget` | Yes | Configuration for the failover cluster side. |

### ServicePort

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | `string` | Yes | — | IANA service name for the port. |
| `port` | `int32` | Yes | — | Port number (1–65535). |
| `protocol` | `string` | No | `TCP` | Protocol: `TCP`, `UDP`, or `SCTP`. |

### ClusterTarget

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `primaryModeAddress` | `string` | Yes | Address used when `failoverActive: false`. Can be a DNS name or IP address. |
| `failoverModeAddress` | `string` | Yes | Address used when `failoverActive: true`. Can be a DNS name or IP address. |
| `onFailover` | `TransitionActions` | No | Actions to run when transitioning to failover. |
| `onFailback` | `TransitionActions` | No | Actions to run when transitioning back to primary. |

### TransitionActions

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `preActions` | `[]FailoverAction` | No | Actions run before traffic switches. |
| `postActions` | `[]FailoverAction` | No | Actions run after traffic switches and connections are flushed. |

### FailoverAction

Exactly one of `http`, `job`, `scale`, or `waitReady` must be set.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Unique name for the action. |
| `ignoreFailure` | `bool` | No | If `true`, the transition continues even if this action fails. Default `false`. |
| `http` | `HTTPAction` | — | HTTP request action. |
| `job` | `JobAction` | — | Kubernetes Job action. |
| `scale` | `ScaleAction` | — | Scale Deployment/StatefulSet action. |
| `waitReady` | `WaitReadyAction` | — | Wait for readiness action. |

### HTTPAction

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `url` | `string` | Yes | — | Target URL. |
| `method` | `string` | No | `GET` | HTTP method. |
| `headers` | `map[string]string` | No | `{}` | Request headers. |
| `body` | `string` | No | `""` | Request body. |

### JobAction

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `image` | `string` | Yes | — | Container image. |
| `command` | `[]string` | No | — | Command array. If both `command` and `script` are set, `script` takes precedence. |
| `script` | `string` | No | — | Shell script (wrapped in `/bin/sh -c`). Takes precedence over `command` if both are set. |
| `env` | `[]EnvVar` | No | `[]` | Environment variables. Each entry has `name` and `value` fields (simplified type, not the full Kubernetes `EnvVar`). |
| `serviceAccountName` | `string` | No | `""` | Service account for the Job's pod. |
| `activeDeadlineSeconds` | `int64` | No | `300` | Maximum runtime before the Job is terminated. |
| `maxRetries` | `int32` | No | `0` | Number of retries on failure (0–10). Maps to Kubernetes `backoffLimit`. |

### ScaleAction

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `kind` | `string` | Yes | `Deployment` or `StatefulSet`. |
| `name` | `string` | Yes | Name of the resource. |
| `replicas` | `int32` | Yes | Target replica count. |

### WaitReadyAction

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `kind` | `string` | Yes | — | `Deployment` or `StatefulSet`. |
| `name` | `string` | Yes | — | Name of the resource. |
| `timeoutSeconds` | `int32` | No | `300` | Maximum time to wait for readiness. |

## Status

### FailoverServiceStatus

| Field | Type | Description |
|-------|------|-------------|
| `observedGeneration` | `int64` | The generation most recently observed by the controller. |
| `activeCluster` | `string` | Which cluster is currently serving traffic (`primary` or `failover`). |
| `activeGeneration` | `int64` | Monotonically increasing counter, incremented on each completed transition. Used for conflict resolution in multi-cluster sync. |
| `lastTransitionTime` | `Time` | Timestamp of the last completed transition. |
| `lastKnownFailoverActive` | `*bool` | The last known value of `spec.failoverActive`. Used to detect transitions. |
| `clusters` | `[]ClusterStatus` | Per-cluster observability. |
| `remoteState` | `RemoteState` | Last-known state of the remote cluster. |
| `transition` | `TransitionStatus` | In-flight transition state. Empty when idle. |
| `conditions` | `[]Condition` | Standard Kubernetes conditions. |

### TransitionStatus

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `string` | Current phase: `ExecutingPreActions`, `UpdatingResources`, `FlushingConnections`, `ExecutingPostActions`, or empty (idle). |
| `targetDirection` | `string` | Either `"failover"` or `"failback"`. |
| `transitionID` | `string` | Unique ID (Unix nanosecond timestamp). |
| `currentActionIndex` | `int` | Index of the current action being executed. |
| `preActionResults` | `[]ActionResult` | Status of each pre-action. |
| `postActionResults` | `[]ActionResult` | Status of each post-action. |
| `startedAt` | `Time` | When the transition started. |
| `oldAddressSnapshot` | `string` | The address that was active before the transition. Captured at transition start for crash safety. |
| `newAddressSnapshot` | `string` | The target address for the transition. Captured at transition start for crash safety. |
| `connectionKill` | `ConnectionKillStatus` | Connection kill progress. |

### ConnectionKillStatus

| Field | Type | Description |
|-------|------|-------------|
| `signalSent` | `bool` | Whether the kill signal has been sent. |
| `killToken` | `string` | Unique token for this kill operation, used to match ACKs. |
| `expectedNodes` | `[]string` | Node names that should ACK the kill. |
| `ackedNodes` | `[]string` | Nodes that have successfully ACKed. |
| `signaledAt` | `Time` | When the kill signal was sent. |

### ClusterStatus

| Field | Type | Description |
|-------|------|-------------|
| `clusterID` | `string` | Unique identifier for this cluster. |
| `appliedGeneration` | `int64` | The `activeGeneration` that this cluster has fully applied. |
| `lastAppliedTime` | `Time` | When this cluster last completed a transition. |
| `phase` | `string` | Current phase of this cluster's transition (empty when idle). |

### ActionResult

| Field | Type | Description |
|-------|------|-------------|
| `name` | `string` | Action name from spec. |
| `status` | `string` | `Pending`, `Running`, `Succeeded`, `Failed`, or `Skipped`. |
| `message` | `string` | Human-readable result message. |
| `startedAt` | `Time` | When this action started executing. |
| `completedAt` | `Time` | When this action finished. |
| `jobName` | `string` | Name of the Job created for job-type actions. |
| `retryCount` | `int` | How many transient errors have occurred for this action. |

### Conditions

| Type | Description |
|------|-------------|
| `ServiceReady` | The managed Service is correctly configured and points to the expected address. |
| `RemoteSynced` | The remote cluster's state matches the local cluster (only present when remote sync is configured). |
| `ConnectionsKilled` | All connection-killer agents have acknowledged the kill signal. |
| `TransitionInProgress` | A failover/failback transition is currently in progress. |
| `ActionFailed` | An action failed during the current transition. |

## Annotations

| Annotation | Description |
|------------|-------------|
| `k8s-failover.zyno.io/cluster-id` | Set by the controller to identify this cluster. Used for remote sync conflict resolution. |
| `k8s-failover.zyno.io/force-advance` | Set to `"true"` to force-advance past a stuck phase or action. The annotation is removed after processing. |

## kubectl Output

```
$ kubectl get failoverservice
NAME             SERVICE          FAILOVER ACTIVE   PHASE   ACTIVE CLUSTER   GENERATION   AGE
mysql-failover   mysql-dns-alias  false                      primary          1            5m
```

## Full Example

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
