# Monitoring

## Key Metrics to Watch

### Failover State

The `failover_active` gauge tells you which mode each FailoverService is in:

```txt
failover_active{namespace="default"}
```

Alert if failover has been active for an extended period (may indicate a forgotten failback):

```yaml
- alert: FailoverActiveExtended
  expr: failover_active == 1
  for: 1h
  labels:
    severity: warning
  annotations:
    summary: "Failover has been active for over 1 hour"
    description: "{{ $labels.name }} in {{ $labels.namespace }} has been in failover mode for more than 1 hour."
```

### Transition Failures

Monitor for reconcile errors that may indicate stuck transitions:

```yaml
- alert: FailoverReconcileErrors
  expr: increase(failover_reconcile_errors_total[5m]) > 0
  labels:
    severity: warning
  annotations:
    summary: "Failover reconcile errors detected"
    description: "{{ $labels.name }} in {{ $labels.namespace }} has reconcile errors in phase {{ $labels.phase }}."
```

### Action Failures

Track failed actions that may halt transitions:

```yaml
- alert: FailoverActionFailed
  expr: increase(failover_action_executions_total{result="failure"}[5m]) > 0
  labels:
    severity: critical
  annotations:
    summary: "Failover action failed"
    description: "Action {{ $labels.action }} ({{ $labels.type }}) failed for {{ $labels.name }} in {{ $labels.namespace }}."
```

### Transition Duration

Alert on slow transitions:

```yaml
- alert: FailoverTransitionSlow
  expr: histogram_quantile(0.99, rate(failover_transition_duration_seconds_bucket[1h])) > 300
  labels:
    severity: warning
  annotations:
    summary: "Failover transitions are taking too long"
```

### Remote Sync Health

Monitor sync duration for degradation:

```yaml
- alert: RemoteSyncSlow
  expr: histogram_quantile(0.99, rate(failover_remote_sync_duration_seconds_bucket[5m])) > 10
  labels:
    severity: warning
  annotations:
    summary: "Remote cluster sync is slow"
```

## Grafana Dashboard

### Suggested Panels

**Overview Row:**
- Failover state gauge per FailoverService (`failover_active`)
- Total transitions over time (`increase(failover_transitions_total[1h])`)
- Active transition phase (from `kubectl` or custom metric)

**Transitions Row:**
- Transition duration heatmap (`failover_transition_duration_seconds_bucket`)
- Transitions by direction (failover vs failback)
- Transition error rate

**Actions Row:**
- Action duration by type (`failover_action_execution_duration_seconds_bucket`)
- Action success/failure rate (`failover_action_executions_total` by result)
- Failed action table

**Infrastructure Row:**
- Connection kill phases (`failover_connection_kill_phases_total`)
- Remote sync duration (`failover_remote_sync_duration_seconds_bucket`)
- Reconcile error rate by phase

## Status Conditions

Use `kubectl` to check the health of a FailoverService:

```bash
kubectl get failoverservice my-service -o jsonpath='{.status.conditions}' | jq
```

| Condition | Healthy Value | Description |
|-----------|--------------|-------------|
| `ServiceReady` | `True` | Managed Service is correctly configured. |
| `RemoteSynced` | `True` | Remote cluster state matches local. |
| `TransitionInProgress` | absent | No transition is running (condition is removed when idle). |
| `ActionFailed` | absent | No actions have failed (condition is removed on completion). |
| `ConnectionsKilled` | `True` (during transition) | All agents acknowledged kill signals. |

## Logs

The controller uses structured logging via `controller-runtime`. Key log messages to watch for:

```bash
# Follow controller logs
kubectl logs -n k8s-failover-manager-system -l control-plane=controller-manager -f
```

| Message | Level | Meaning |
|---------|-------|---------|
| `"beginning transition"` | Info | A failover/failback transition has started. |
| `"transition complete"` | Info | A transition finished successfully. |
| `"action failed"` | Error | An action failed during a transition. |
| `"connection kill timeout"` | Error | Not all agents acknowledged within 60 seconds. |
| `"remote sync conflict"` | Warning | Local and remote states disagreed; conflict was resolved. |
| `"cluster IDs are empty"` | Warning | Cluster IDs are not set; conflict resolution may be unpredictable. |
| `"cluster IDs are identical"` | Warning | Both clusters have the same ID; change one. |
