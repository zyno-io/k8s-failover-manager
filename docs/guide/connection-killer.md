# Connection Killer

When traffic switches from an IP-based target to a new address, existing TCP connections to the old IP can persist, causing clients to hang on dead backends. The connection killer solves this by destroying stale TCP sockets across all nodes in the cluster.

## How It Works

The connection killer uses a two-component architecture:

### Controller Side (Signaler)

During the `FlushingConnections` phase, the controller:

1. Discovers all ready connection-killer agent pods
2. Writes a kill signal to a `connection-kill-signal` ConfigMap with the target IP and a monotonic sequence number
3. Polls the ConfigMap for acknowledgments from each node
4. Waits until all nodes have acknowledged (or times out after 60 seconds)

### DaemonSet Side (Agent)

Each agent pod runs on a Linux node with `hostPID: true` and the `NET_ADMIN` + `SYS_ADMIN` capabilities. It:

1. Watches the ConfigMap for kill signals addressed to its node using the Kubernetes Watch API (reconnects automatically on watch errors)
2. Checks the monotonic sequence number to skip already-processed signals
3. Enumerates all network namespaces on the host by scanning `/proc/*/ns/net`
4. Enters each unique namespace using `setns(2)` with OS thread locking
5. Uses a `NETLINK_SOCK_DIAG` socket to enumerate all TCP sockets
6. Sends `SOCK_DESTROY` for each socket matching the target destination IP
7. Writes an acknowledgment back to the ConfigMap with the kill count

## When Connections Are Killed

Connection killing **only occurs** when:

- The old target address (before the transition) was an **IP address**
- The connection killer is **enabled** in the Helm values
- At least one agent pod is **running**

If the old address was a DNS name (ExternalName Service), connection killing is skipped since DNS-based services don't hold direct TCP connections through the cluster network.

## ConfigMap Protocol

The controller and agents communicate through a shared ConfigMap named `connection-kill-signal` in the controller's namespace.

### Kill Signal Keys

Format: `kill_{nodeName}_{namespace}_{name}`

```json
{
  "ip": "10.0.1.100",
  "seq": 3,
  "token": "failover-abc123",
  "node": "worker-1"
}
```

### Acknowledgment Keys

Format: `ack_{nodeName}_{namespace}_{name}`

```json
{
  "seq": 3,
  "token": "failover-abc123",
  "node": "worker-1",
  "killed": 42,
  "errors": 0
}
```

The `seq` field is monotonically increasing per key, preventing the agent from replaying completed kills after a restart. On startup, the agent seeds its sequence counter from existing ACK entries.

## Network Namespace Handling

Kubernetes pods run in isolated network namespaces. To kill connections in **all** pods on a node, the agent:

1. Scans `/proc/*/ns/net` to find all network namespace file descriptors
2. Deduplicates by inode number (many processes share the same namespace)
3. Enters each unique namespace using the `setns` syscall
4. Kills matching connections using netlink
5. Restores the original namespace

This requires `hostPID: true` (to see all processes) and `SYS_ADMIN` capability (to call `setns`).

## Netlink Socket Destruction

Within each network namespace, the agent:

1. Opens a `NETLINK_SOCK_DIAG` socket
2. Sends a `SOCK_DIAG_BY_FAMILY` dump request to enumerate all TCP sockets in all states
3. Filters sockets matching the target destination IP (IPv4 or IPv6)
4. Sends a `SOCK_DESTROY` message for each matching socket
5. Waits for an ACK from the kernel

This is a Linux-specific feature using the `inet_diag` subsystem. The agent sets a 10-second receive timeout for the dump and a 5-second timeout per destroy ACK.

## Timeout and Force-Advance

If not all agents acknowledge within 60 seconds, the transition **halts**. This prevents completing a transition when some nodes might still have stale connections.

To manually advance past a stuck connection kill phase, use the force-advance annotation:

```bash
kubectl annotate failoverservice my-service \
  k8s-failover.zyno.io/force-advance=true
```

See [Troubleshooting](../operations/troubleshooting.md) for more details.

## Cleanup

The controller cleans up ConfigMap entries for a FailoverService in two cases:

- **After each transition completes** — kill/ack entries for the completed transition are removed
- **When a FailoverService is deleted** — all entries belonging to that FailoverService are removed (best-effort)

This prevents the ConfigMap from growing unbounded over time.

## DaemonSet Configuration

The connection killer DaemonSet is configured in the Helm values:

```yaml
connectionKiller:
  enabled: true
  image:
    repository: ghcr.io/zyno-io/k8s-failover-manager-connection-killer
    tag: latest
  resources:
    limits:
      cpu: 100m
      memory: 64Mi
    requests:
      cpu: 10m
      memory: 32Mi
```

The DaemonSet runs with:

- `hostPID: true` — access all process network namespaces
- `NET_ADMIN` capability — destroy TCP sockets via netlink
- `SYS_ADMIN` capability — enter other network namespaces via `setns`
- `nodeSelector: kubernetes.io/os: linux` — only Linux nodes
- Tolerates all taints — runs on every Linux node in the cluster
- Runs as root (UID 0)

## Disabling Connection Killing

Set `connectionKiller.enabled: false` in the Helm values to disable the DaemonSet. When no agents are running, the controller still enters the `FlushingConnections` phase but immediately marks connection killing as skipped and advances to the next phase.
