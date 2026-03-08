# What is k8s-failover-manager?

k8s-failover-manager is a Kubernetes operator that manages automated DNS-based service failover between two clusters — a **primary** and a **failover**. It provides:

- **Traffic switching** via Kubernetes Service manipulation (ExternalName for DNS targets, ClusterIP + EndpointSlice for IP targets)
- **Ordered transition hooks** — run HTTP calls, Kubernetes Jobs, scale workloads, wait for readiness, and coordinate with the remote cluster before and after switching traffic
- **Connection draining** — a DaemonSet agent kills stale TCP connections on all nodes using Linux netlink socket destruction
- **Multi-cluster state sync** — push-based synchronization with timestamp-based conflict resolution ensures both clusters agree on the active state
- **Transition history** — `FailoverEvent` resources record every transition with outcome, timing, and action results
- **Observability** — Prometheus metrics for transitions, actions, connection kills, and sync operations

## How It Works

The operator watches a `FailoverService` custom resource. When you change `spec.failoverActive`, it runs through a state machine:

```
Idle → ExecutingPreActions → UpdatingResources → FlushingConnections → ExecutingPostActions → Idle
```

Each phase is crash-safe — the controller persists state before executing actions and resumes correctly after restarts.

## Dual Addressing Model

Each cluster has two addresses configured:

- `primaryModeAddress` — used when the cluster is serving traffic in primary mode
- `failoverModeAddress` — used when the cluster is serving traffic in failover mode

This allows each cluster to expose different endpoints depending on whether failover is active. For example, a primary cluster might use a DNS name in normal operation but an IP address during failover (when it becomes the standby).

## Use Cases

- **Database failover** — Promote a read replica to primary, drain connections, notify monitoring systems
- **Regional failover** — Switch traffic between clusters in different regions during outages
- **Maintenance windows** — Gracefully move traffic to a standby cluster, perform maintenance, then fail back
- **Disaster recovery** — Automated or manual failover with coordinated pre/post actions across both clusters
