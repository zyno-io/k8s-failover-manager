# Getting Started

## Prerequisites

- Kubernetes 1.28+
- Helm 3.x
- `kubectl` configured to access your cluster

## Installation

### Install with Helm

Add the Helm chart and install the operator:

```bash
helm install k8s-failover-manager ./charts/k8s-failover-manager \
  --namespace k8s-failover-manager-system \
  --create-namespace \
  --set failoverConfig.clusterRole=primary \
  --set failoverConfig.clusterId=cluster-1 \
  --set failoverConfig.remoteKubeconfigSecret=remote-kubeconfig \
  --set failoverConfig.remoteKubeconfigNamespace=k8s-failover-manager-system \
  --set image.repository=ghcr.io/zyno-io/k8s-failover-manager \
  --set connectionKiller.image.repository=ghcr.io/zyno-io/k8s-failover-manager-connection-killer
```

On the failover cluster, install with:

```bash
helm install k8s-failover-manager ./charts/k8s-failover-manager \
  --namespace k8s-failover-manager-system \
  --create-namespace \
  --set failoverConfig.clusterRole=failover \
  --set failoverConfig.clusterId=cluster-2 \
  --set failoverConfig.remoteKubeconfigSecret=remote-kubeconfig \
  --set failoverConfig.remoteKubeconfigNamespace=k8s-failover-manager-system \
  --set image.repository=ghcr.io/zyno-io/k8s-failover-manager \
  --set connectionKiller.image.repository=ghcr.io/zyno-io/k8s-failover-manager-connection-killer
```

### Verify Installation

```bash
kubectl get pods -n k8s-failover-manager-system
```

You should see the controller manager pod running, and a connection-killer pod on each Linux node.

## Quick Start: DNS Failover

The **same `FailoverService` resource is applied identically to both clusters**. Each cluster's operator reads the section relevant to its configured role (`primary` or `failover`) and manages the local Service accordingly.

Create the following `FailoverService`:

```yaml
apiVersion: k8s-failover.zyno.io/v1alpha1
kind: FailoverService
metadata:
  name: my-service
  namespace: default
spec:
  serviceName: my-app-alias
  ports:
    - name: http
      port: 80
      protocol: TCP
  failoverActive: false

  # The primary cluster's addressing and actions.
  # - primaryModeAddress: where the Service points when this cluster is active
  # - failoverModeAddress: where the Service points when this cluster is standby
  primaryCluster:
    primaryModeAddress: "app.primary.example.com"
    failoverModeAddress: "10.0.1.100"

  # The failover cluster's addressing and actions.
  # Same structure — the operator on the failover cluster reads this section.
  failoverCluster:
    primaryModeAddress: "10.0.2.100"
    failoverModeAddress: "app.failover.example.com"
```

Apply it to **both** clusters:

```bash
# On the primary cluster
kubectl apply -f failoverservice.yaml

# On the failover cluster (switch context first)
kubectl apply -f failoverservice.yaml
```

Check the managed Service:

```bash
kubectl get svc my-app-alias
```

On the primary cluster, the Service is created as an `ExternalName` pointing to `app.primary.example.com` (from `primaryCluster.primaryModeAddress`). On the failover cluster, it becomes a `ClusterIP` with an EndpointSlice pointing to `10.0.2.100` (from `failoverCluster.primaryModeAddress`). When failover is triggered, each cluster switches to its respective `failoverModeAddress`.

## Complete Example: MySQL Failover with Actions

Here is a more realistic example that uses actions to orchestrate a MySQL failover across two clusters. Again, this **same YAML is applied to both clusters**:

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
    # In primary mode, the Service resolves to the primary MySQL host.
    # In failover mode (standby), it points to an internal IP instead.
    primaryModeAddress: "mysql-primary.example.com"
    failoverModeAddress: "192.168.50.60"

    # Actions the primary cluster runs when failover is triggered
    onFailover:
      preActions:
        - name: drain-primary
          http:
            url: "http://mysql-primary:9090/drain"
            method: POST
      postActions:
        - name: scale-down-workers
          scale:
            kind: Deployment
            name: mysql-workers
            replicas: 0

    # Actions the primary cluster runs when failing back to normal
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
    # In primary mode (standby), the Service points to an internal IP.
    # In failover mode (active), it resolves to the failover MySQL host.
    primaryModeAddress: "10.20.30.40"
    failoverModeAddress: "mysql-failover.example.com"

    # Actions the failover cluster runs when it becomes active
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

Each cluster's operator only executes the actions defined under its own section. When failover is triggered, the primary cluster drains connections and scales down workers, while the failover cluster promotes its replica and sends a notification — all coordinated through the state machine.

## Triggering Failover

To activate failover:

```bash
kubectl patch failoverservice my-service \
  --type merge -p '{"spec":{"failoverActive":true}}'
```

The operator will:

1. Execute any configured pre-actions
2. Update the managed Service to point to the failover address
3. Kill stale TCP connections (if the old address was an IP)
4. Execute any configured post-actions

## Failing Back

To return to primary:

```bash
kubectl patch failoverservice my-service \
  --type merge -p '{"spec":{"failoverActive":false}}'
```

## Checking Status

```bash
kubectl get failoverservice my-service
```

```
NAME         SERVICE        FAILOVER ACTIVE   PHASE   ACTIVE CLUSTER   GENERATION   AGE
my-service   my-app-alias   false                      primary          1            5m
```

For detailed status:

```bash
kubectl get failoverservice my-service -o yaml
```

Key status fields:

- `status.activeCluster` — which cluster is currently serving traffic
- `status.transition.phase` — current transition phase (empty when idle)
- `status.conditions` — `ServiceReady`, `RemoteSynced`, `TransitionInProgress`, etc.

## Next Steps

- [Architecture](./architecture.md) — understand the state machine and component design
- [Actions](./actions.md) — configure pre/post transition hooks
- [Remote Clusters](./remote-clusters.md) — set up multi-cluster state sync
- [Helm Chart Reference](../reference/helm.md) — all configuration options
