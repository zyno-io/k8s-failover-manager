# Metrics Reference

k8s-failover-manager exposes Prometheus metrics on the controller's HTTPS metrics endpoint (default port 8443).

## Transition Metrics

### `failover_transitions_total`

**Type:** Counter
**Labels:** `name`, `namespace`, `direction`

Total number of failover/failback transitions started.

```txt
# Transitions in the last hour
increase(failover_transitions_total[1h])

# Rate of failovers by direction
rate(failover_transitions_total{direction="to-failover"}[5m])
```

### `failover_transition_duration_seconds`

**Type:** Histogram
**Labels:** `name`, `namespace`, `direction`
**Buckets:** Default Prometheus buckets

End-to-end duration of completed transitions, from phase start to completion.

```txt
# p99 transition duration
histogram_quantile(0.99, rate(failover_transition_duration_seconds_bucket[1h]))

# Average transition time
rate(failover_transition_duration_seconds_sum[1h]) / rate(failover_transition_duration_seconds_count[1h])
```

### `failover_active`

**Type:** Gauge
**Labels:** `name`, `namespace`

Current failover state: `1` when failover is active, `0` when primary is active.

```txt
# Alert when failover is active for more than 30 minutes
failover_active == 1
```

## Action Metrics

### `failover_action_executions_total`

**Type:** Counter
**Labels:** `name`, `namespace`, `action`, `type`, `result`

Total number of action executions, broken down by action name, type (http/job/scale/waitReady), and result (success/failure).

```txt
# Failed actions
failover_action_executions_total{result="failure"}
```

### `failover_action_execution_duration_seconds`

**Type:** Histogram
**Labels:** `name`, `namespace`, `action`, `type`
**Buckets:** Default Prometheus buckets

Duration of individual action executions.

```txt
# Slowest actions
histogram_quantile(0.99, rate(failover_action_execution_duration_seconds_bucket[1h]))
```

## Connection Kill Metrics

### `failover_connection_kill_phases_total`

**Type:** Counter

Total number of successful connection kill phases (all agents acknowledged).

## Remote Sync Metrics

### `failover_remote_sync_duration_seconds`

**Type:** Histogram
**Buckets:** Default Prometheus buckets

Duration of remote cluster sync operations.

```txt
# Average sync duration
rate(failover_remote_sync_duration_seconds_sum[5m]) / rate(failover_remote_sync_duration_seconds_count[5m])
```

## Error Metrics

### `failover_reconcile_errors_total`

**Type:** Counter
**Labels:** `name`, `namespace`, `phase`

Reconcile errors by phase. Phase values: `service`, `connkill`, `remote-sync`.

```txt
# Alert on reconcile errors
increase(failover_reconcile_errors_total[5m]) > 0
```

## Accessing Metrics

The metrics endpoint is served over HTTPS with authentication. To access it:

### Via ServiceMonitor (Recommended)

Enable the ServiceMonitor in the Helm values:

```yaml
serviceMonitor:
  enabled: true
  labels:
    release: prometheus  # Match your Prometheus operator's selector
```

### Manual Access

Create a ClusterRoleBinding for the metrics reader role, then use a bearer token:

```bash
# Get a token
TOKEN=$(kubectl create token k8s-failover-manager-controller-manager \
  -n k8s-failover-manager-system)

# Fetch metrics
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://k8s-failover-manager-controller-manager-metrics-service.k8s-failover-manager-system:8443/metrics
```
