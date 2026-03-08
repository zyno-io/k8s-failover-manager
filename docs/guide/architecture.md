# Architecture

## Components

k8s-failover-manager consists of two main components:

### Controller Manager

A Deployment that runs the reconciliation loop. It:

- Watches `FailoverService` custom resources
- Manages Kubernetes Services and EndpointSlices
- Executes transition actions (HTTP, Job, Scale, WaitReady, WaitRemote)
- Coordinates connection killing via ConfigMap signals
- Syncs state with the remote cluster

### Connection Killer DaemonSet

A DaemonSet agent running on every Linux node. It:

- Watches a ConfigMap for kill signals from the controller
- Enumerates all network namespaces on the node (via `/proc/*/ns/net`)
- Destroys TCP sockets matching the target IP using Linux netlink (`SOCK_DESTROY`)
- Writes acknowledgments back to the ConfigMap

## State Machine

When `spec.failoverActive` changes, the controller drives a transition through five phases:

```
                    ┌─────────────────────────────────────────────────────┐
                    │                                                     │
                    ▼                                                     │
                ┌───────┐     ┌──────────────┐     ┌──────────────┐     │
  spec change   │       │     │  Executing   │     │   Updating   │     │
  ────────────► │ Idle  │────►│  PreActions  │────►│  Resources   │     │
                │       │     │              │     │              │     │
                └───────┘     └──────────────┘     └──────────────┘     │
                    ▲                                     │              │
                    │                                     ▼              │
                    │         ┌──────────────┐     ┌──────────────┐     │
                    │         │  Executing   │     │   Flushing   │     │
                    └─────────│ PostActions  │◄────│ Connections  │     │
                              │              │     │              │     │
                              └──────────────┘     └──────────────┘     │
                                                                        │
                              (if no actions, phases are skipped) ───────┘
```

### Phase Details

| Phase | Description |
|-------|-------------|
| **Idle** | Steady state. The controller reconciles the Service and syncs with the remote cluster every 60 seconds (anti-entropy). Transition events are recorded as `FailoverEvent` resources. |
| **ExecutingPreActions** | Runs pre-transition actions sequentially. Each action is persisted as `Running` before execution for crash safety. |
| **UpdatingResources** | Updates the managed Service and EndpointSlice to point to the new target address. Uses the **snapshotted** direction, not the live spec. |
| **FlushingConnections** | Signals the connection killer DaemonSet to destroy TCP connections to the old target IP. Polls for acknowledgments with a 60-second timeout. Skipped if the old address was a DNS name. |
| **ExecutingPostActions** | Runs post-transition actions sequentially, same as pre-actions. |

### Crash Safety

The controller is designed to survive crashes at any point:

- **Action state is persisted before execution** — if the controller crashes mid-action, it finds the action in `Running` state on restart and re-executes it
- **Jobs are idempotent** — the controller checks for existing Jobs before creating new ones
- **Direction is snapshotted** — the `TransitionStatus.TargetDirection` field records the intended direction at transition start, so flipping `spec.failoverActive` again mid-transition won't corrupt the in-flight transition
- **Addresses are snapshotted** — `OldAddressSnapshot` and `NewAddressSnapshot` are captured at transition start, ensuring the correct addresses are used even if the spec is modified mid-transition
- **Monotonic sequence numbers** — connection kill signals use incrementing sequence numbers to prevent replay after agent restart

## Service Management

The controller creates and manages a Kubernetes Service based on the target address type:

| Address Type | Service Type | Additional Resources |
|-------------|-------------|---------------------|
| DNS name | `ExternalName` | None |
| IP address | `ClusterIP` (no selector) | `EndpointSlice` with the target IP |

When switching between address types (e.g., DNS to IP), the controller deletes and recreates the Service since Kubernetes doesn't allow changing Service types in-place.

All managed resources carry an `OwnerReference` pointing to the `FailoverService` and a `app.kubernetes.io/managed-by: k8s-failover-manager` label. The controller refuses to modify resources not owned by the `FailoverService`.

## Anti-Entropy

Every 60 seconds, the controller:

1. Reconciles the managed Service to ensure it matches the expected state
2. Syncs with the remote cluster to detect and resolve conflicts
3. Updates the `failover_active` gauge metric

This ensures eventual consistency even if events are missed.

## Resource Ownership

The controller sets `OwnerReference` on:

- Managed `Service`
- Managed `EndpointSlice`
- Created `Job` resources (for action execution)
- Created `FailoverEvent` resources (for transition history)

This means:

- Deleting the `FailoverService` CR automatically cleans up all managed resources
- Changes to owned resources trigger reconciliation (the controller watches them)
- The controller won't modify resources it doesn't own
