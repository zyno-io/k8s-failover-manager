# Actions

Actions are ordered hooks that run before and after traffic switching during a failover or failback transition. They allow you to coordinate complex workflows like draining databases, promoting replicas, scaling workloads, and sending notifications.

## Action Types

Each action must specify exactly one of the five action types:

### HTTP

Makes an HTTP request to a URL. Useful for webhooks, drain endpoints, and API calls.

```yaml
- name: drain-primary
  http:
    url: "http://mysql-primary:9090/drain"
    method: POST
    headers:
      Content-Type: "application/json"
      Authorization: "Bearer token123"
    body: '{"graceful": true}'
```

| Field | Default | Description |
|-------|---------|-------------|
| `url` | *required* | Target URL |
| `method` | `GET` | HTTP method |
| `headers` | `{}` | Request headers |
| `body` | `""` | Request body |
| `timeoutSeconds` | `30` | HTTP request timeout (minimum 1) |

**Behavior:**
- Configurable timeout per request (default 30 seconds)
- Transport errors (DNS failures, connection refused) trigger controller-level retries (default 5 attempts, 5-second interval — customizable per-action)
- Non-2xx status codes are treated as action failures (no automatic retry)
- Synchronous — completes immediately

### Job

Creates a Kubernetes Job to run arbitrary workloads. Ideal for database migrations, data synchronization, or any task requiring specific tooling.

```yaml
- name: promote-replica
  job:
    image: "mysql:8.0"
    serviceAccountName: mysql-admin
    script: |
      mysql -h localhost -e "STOP REPLICA; RESET REPLICA ALL;"
    activeDeadlineSeconds: 120
    maxRetries: 2
```

| Field | Default | Description |
|-------|---------|-------------|
| `image` | *required* | Container image |
| `command` | — | Command array. If both `command` and `script` are set, `script` takes precedence. |
| `script` | — | Shell script (wrapped in `/bin/sh -c`). Takes precedence over `command`. |
| `env` | `[]` | Environment variables |
| `serviceAccountName` | `""` | Service account for the Job's pod |
| `activeDeadlineSeconds` | `300` | Maximum runtime before the Job is terminated |
| `maxRetries` | `0` | Number of retries on failure (0–10) |

**Behavior:**
- Asynchronous — the controller creates the Job and polls for completion every 30 seconds (as a safety fallback; Job status changes also trigger reconciles automatically)
- Idempotent — checks for existing Jobs before creating (safe across controller restarts)
- Job names include a `transitionID` (Unix nanosecond timestamp) to prevent collisions between transitions
- Jobs are cleaned up (deleted with background propagation) after the transition completes
- The Job gets an `OwnerReference` pointing to the `FailoverService` (pods are owned by the Job via the Kubernetes Job controller)

### Scale

Scales a Deployment or StatefulSet to a target replica count. Useful for scaling down workers on the old primary or scaling up on the new primary.

```yaml
- name: scale-down-workers
  scale:
    kind: Deployment
    name: mysql-workers
    replicas: 0
```

| Field | Default | Description |
|-------|---------|-------------|
| `kind` | *required* | `Deployment` or `StatefulSet` |
| `name` | *required* | Name of the resource |
| `replicas` | *required* | Target replica count |
| `waitReady` | `false` | If true, wait for all replicas to become ready after scaling |
| `timeoutSeconds` | `300` | Timeout for the waitReady check (minimum 1). Only used when `waitReady: true`. |

**Behavior:**
- Updates the scale subresource
- If `waitReady: false` (default), completes immediately after scaling
- If `waitReady: true`, polls until all replicas are ready or `timeoutSeconds` is exceeded
- Fails if the target resource doesn't exist

### WaitReady

Waits for a Deployment or StatefulSet to become fully ready. Typically used after a `scale` action.

```yaml
- name: wait-workers-ready
  waitReady:
    kind: Deployment
    name: mysql-workers
    timeoutSeconds: 120
```

| Field | Default | Description |
|-------|---------|-------------|
| `kind` | *required* | `Deployment` or `StatefulSet` |
| `name` | *required* | Name of the resource |
| `timeoutSeconds` | `300` | Maximum time to wait |

**Behavior:**
- Asynchronous — polls every 5 seconds until ready
- Ready means: `ReadyReplicas == UpdatedReplicas == AvailableReplicas == desired replicas` and `ObservedGeneration == Generation`
- Times out with a failure if not ready within `timeoutSeconds`

### WaitRemote

Waits for the remote cluster to reach a specific phase or complete a specific action during its transition. This enables coordinated multi-cluster transitions where one cluster waits for the other before proceeding.

```yaml
- name: wait-for-remote-preactions
  waitRemote:
    phase: UpdatingResources
    connectTimeoutSeconds: 10
    actionTimeoutSeconds: 300
```

Or wait for a specific named action on the remote cluster:

```yaml
- name: wait-for-remote-drain
  waitRemote:
    actionName: drain-primary
    actionTimeoutSeconds: 600
```

| Field | Default | Description |
|-------|---------|-------------|
| `phase` | — | Wait for the remote to reach or pass this phase. One of: `""` (idle), `ExecutingPreActions`, `UpdatingResources`, `FlushingConnections`, `ExecutingPostActions`. Use `""` to wait for the remote to return to idle (i.e. complete its transition). |
| `actionName` | — | Wait for a named action on the remote to succeed. |
| `connectTimeoutSeconds` | `10` | Timeout for connecting to the remote cluster (minimum 1). |
| `actionTimeoutSeconds` | `300` | Timeout for waiting for the condition to be met (minimum 1). |

Exactly one of `phase` or `actionName` must be set.

**Behavior:**
- Asynchronous — polls the remote cluster's `FailoverService` status until the condition is met
- Phase mode: uses phase ordering (Idle < ExecutingPreActions < UpdatingResources < FlushingConnections < ExecutingPostActions) to determine if the remote has reached or passed the target phase
- ActionName mode: searches the remote's `preActionResults` and `postActionResults` for a matching action name with status `Succeeded` or `Skipped`
- Fails if the remote action has status `Failed`
- Uses the local transition's start time to correlate with the remote transition, preventing false positives when the remote hasn't started yet
- Times out if the condition is not met within `actionTimeoutSeconds`

## Per-Action Retry Settings

In addition to type-specific fields, every action supports these optional fields for controlling retry behavior:

| Field | Default | Description |
|-------|---------|-------------|
| `maxRetries` | `5` | Maximum number of retry attempts (0–50). Set to 0 to disable retries. |
| `retryIntervalSeconds` | `5` | Seconds between retry attempts (1–300). |
| `ignoreFailure` | `false` | Continue the transition even if this action fails. |

These apply to all action types and override the controller's default retry settings.

## Pre-Actions vs Post-Actions

Actions are organized into two groups per transition direction:

```yaml
spec:
  primaryCluster:
    onFailover:
      preActions:   # Run BEFORE traffic switches
        - ...
      postActions:  # Run AFTER traffic switches and connections are flushed
        - ...
    onFailback:
      preActions:
        - ...
      postActions:
        - ...
```

**Pre-actions** run before the Service is updated and connections are killed. Use them for:
- Draining the old primary
- Promoting a replica on the failover cluster
- Preparing the new primary for traffic

**Post-actions** run after traffic has switched and connections have been flushed. Use them for:
- Scaling down workers on the old primary
- Sending notifications
- Cleaning up temporary resources

## Ignoring Failures

By default, a failed action halts the transition. Set `ignoreFailure: true` to continue even if the action fails:

```yaml
- name: notify-slack
  http:
    url: "https://hooks.slack.com/services/XXX"
    method: POST
    body: '{"text":"Failover complete"}'
  ignoreFailure: true
```

Failed actions with `ignoreFailure: true` are recorded as `Skipped` in the transition status.

## Execution Order

Actions within a group (pre or post) are executed **sequentially** in the order they appear in the spec. The controller processes one action at a time, persisting its state before and after execution.

## Action Results

Each action's progress is tracked in `status.transition.preActionResults` or `status.transition.postActionResults`:

| Status | Meaning |
|--------|---------|
| `Pending` | Not yet started |
| `Running` | Currently executing |
| `Succeeded` | Completed successfully |
| `Failed` | Failed (transition halted unless `ignoreFailure: true`) |
| `Skipped` | Failed but `ignoreFailure: true` was set |

## Cluster-Specific Actions

Actions are defined per-cluster, so each cluster can have different transition hooks:

```yaml
spec:
  primaryCluster:
    onFailover:
      preActions:
        - name: drain-primary
          http:
            url: "http://mysql-primary:9090/drain"
            method: POST
  failoverCluster:
    onFailover:
      preActions:
        - name: promote-replica
          job:
            image: "mysql:8.0"
            script: "mysql -e 'STOP REPLICA; RESET REPLICA ALL;'"
```

The controller only executes actions defined for its own cluster role (primary or failover), as set by `failoverConfig.clusterRole` in the Helm values.

## Retry Behavior

All action types support per-action `maxRetries` and `retryIntervalSeconds` to override the defaults.

- **HTTP actions**: Transport errors are retried (default 5 attempts, 5-second intervals). Non-2xx responses are not retried.
- **Job actions**: Retries are controlled by the Job's `maxRetries` field (Kubernetes `backoffLimit`). The controller polls until the Job succeeds, fails, or exceeds `activeDeadlineSeconds`. Controller-level requeue uses a 30-second interval.
- **Scale actions**: Transient API errors are retried. If `waitReady: true`, polls until ready or `timeoutSeconds`.
- **WaitReady actions**: Polls until ready or timeout. No retry — it either converges or times out.
- **WaitRemote actions**: Polls until the remote condition is met or `actionTimeoutSeconds` is exceeded.
