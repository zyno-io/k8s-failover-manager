# Troubleshooting

## Stuck Transitions

### Symptoms

- `kubectl get failoverservice` shows a non-empty `PHASE` column
- `TransitionInProgress` condition remains `True` for an extended period

### Diagnosis

Check the transition status:

```bash
kubectl get failoverservice my-service -o jsonpath='{.status.transition}' | jq
```

Look at:
- `phase` — which phase is stuck
- `preActionResults` / `postActionResults` — which action is stuck or failed
- `connectionKill` — whether agents have acknowledged

### Force-Advancing Past a Stuck Phase

If a transition is stuck (e.g., an action is permanently failing or a connection killer agent is down), you can force-advance:

```bash
kubectl annotate failoverservice my-service \
  k8s-failover.zyno.io/force-advance=true
```

This will:
- **During an action phase**: Skip the current action (marks it as `Skipped`) and proceed
- **During connection flushing**: Skip waiting for remaining agent acknowledgments
- The annotation is automatically removed after processing

::: warning
Force-advancing skips safety checks. Use it only when you've verified the stuck component and understand the implications (e.g., stale connections may persist if you skip connection flushing).
:::

You may need to apply the annotation multiple times to advance through multiple stuck actions.

## Action Failures

### HTTP Action Fails

**Common causes:**
- Target service is not reachable from the controller pod
- Target service is returning non-2xx status codes
- DNS resolution failure

**Debug:**

```bash
# Check controller logs for the specific action
kubectl logs -n k8s-failover-manager-system -l control-plane=controller-manager | grep "action failed"

# Test connectivity from the controller pod
kubectl exec -n k8s-failover-manager-system deploy/k8s-failover-manager-controller-manager -- \
  wget -q -O- http://target-service:9090/health
```

### Job Action Fails

**Common causes:**
- Image pull failure
- Script error
- Insufficient RBAC for the Job's service account
- `activeDeadlineSeconds` exceeded

**Debug:**

```bash
# Find the Job
kubectl get jobs -l app.kubernetes.io/managed-by=k8s-failover-manager

# Check Job status
kubectl describe job <job-name>

# Check pod logs
kubectl logs job/<job-name>
```

### Scale Action Fails

**Common causes:**
- Target Deployment/StatefulSet doesn't exist
- Name mismatch

**Debug:**

```bash
# Verify the target exists
kubectl get deployment <name>
kubectl get statefulset <name>
```

### WaitReady Times Out

**Common causes:**
- Pods failing to start (image pull, crash loop)
- Insufficient resources
- Readiness probe failing

**Debug:**

```bash
kubectl get pods -l app=<workload-name>
kubectl describe pod <pod-name>
```

## Connection Killer Issues

### Agents Not Acknowledging

**Symptoms:**
- Transition stuck in `FlushingConnections` phase
- `connectionKill.expectedNodes` has entries not present in `connectionKill.ackedNodes`

**Debug:**

```bash
# Check agent pods
kubectl get pods -n k8s-failover-manager-system -l app.kubernetes.io/name=connection-killer

# Check agent logs
kubectl logs -n k8s-failover-manager-system -l app.kubernetes.io/name=connection-killer

# Check the ConfigMap
kubectl get configmap connection-kill-signal -n k8s-failover-manager-system -o yaml
```

**Common causes:**
- Agent pod is not running on all nodes
- Agent doesn't have `hostPID: true` or required capabilities
- ConfigMap RBAC is missing
- Node was added after DaemonSet was deployed (should self-heal)

### Agent Crashes

If the agent crashes and restarts, it re-reads the ConfigMap and seeds its sequence counter from existing ACK entries to avoid replaying completed kills.

## Remote Sync Issues

### RemoteSynced Condition is False

**Debug:**

```bash
# Check the remote state in status
kubectl get failoverservice my-service -o jsonpath='{.status.remoteState}' | jq

# Check controller logs for sync errors
kubectl logs -n k8s-failover-manager-system -l control-plane=controller-manager | grep "remote sync"
```

**Common causes:**
- Remote kubeconfig Secret is missing or has wrong format (must have a `kubeconfig` key)
- Remote cluster is unreachable
- RBAC on the remote cluster doesn't allow get/update of FailoverService
- Network policy blocking cross-cluster communication

### Conflicting States

If both clusters show different `failoverActive` values, the conflict resolution algorithm will resolve it:

1. Higher `activeGeneration` wins
2. On tie: lexicographic cluster ID comparison

Check both clusters:

```bash
# On primary
kubectl get failoverservice my-service -o jsonpath='{.status.activeGeneration}'

# On failover
kubectl get failoverservice my-service -o jsonpath='{.status.activeGeneration}'
```

The cluster with the higher `activeGeneration` is considered authoritative.

## Service Not Created

If the managed Service is not appearing:

```bash
# Check the FailoverService status
kubectl describe failoverservice my-service

# Check controller logs
kubectl logs -n k8s-failover-manager-system -l control-plane=controller-manager | grep "my-service"
```

**Common causes:**
- `serviceName` conflicts with an existing Service not owned by the FailoverService
- Controller doesn't have RBAC to create Services in the target namespace
- Invalid address format

## Controller Not Starting

```bash
# Check pod status
kubectl get pods -n k8s-failover-manager-system -l control-plane=controller-manager

# Check events
kubectl describe pod -n k8s-failover-manager-system -l control-plane=controller-manager

# Check logs
kubectl logs -n k8s-failover-manager-system -l control-plane=controller-manager
```

**Common causes:**
- Image pull failure
- Missing CRDs (install with `kubectl apply -f charts/k8s-failover-manager/crds/`)
- Invalid configuration (check ConfigMap values)
- Insufficient RBAC

## Common Patterns

### Safe Failover Checklist

Before triggering failover in production:

1. Verify both clusters are healthy: `kubectl get failoverservice -o wide` on both clusters
2. Check `RemoteSynced` condition is `True` (if using remote sync)
3. Verify connection killer agents are running: `kubectl get ds -n k8s-failover-manager-system`
4. Review pre/post actions for correctness
5. Trigger failover: `kubectl patch failoverservice my-service --type merge -p '{"spec":{"failoverActive":true}}'`
6. Monitor the transition: `kubectl get failoverservice my-service -w`
7. Verify the Service is pointing to the new address: `kubectl get svc <service-name> -o yaml`

### Recovering From a Failed Transition

If a transition fails partway through, the controller **halts** and waits for intervention:

1. Check which phase and action failed (see [Diagnosis](#diagnosis) above)
2. Fix the underlying issue (e.g., fix the target service, correct RBAC)
3. Use force-advance to skip the failed action: `kubectl annotate failoverservice my-service k8s-failover.zyno.io/force-advance=true`
4. Repeat force-advance if multiple actions need to be skipped
5. After the transition completes, verify the Service is pointing to the correct address
