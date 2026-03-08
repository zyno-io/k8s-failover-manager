---
layout: home

hero:
  name: k8s-failover-manager
  text: Automated Service Failover for Kubernetes
  tagline: A Kubernetes operator that manages DNS-based service failover between clusters with zero-downtime traffic switching, connection draining, and multi-cluster state synchronization.
  actions:
    - theme: brand
      text: Get Started
      link: /guide/getting-started
    - theme: alt
      text: Architecture
      link: /guide/architecture
    - theme: alt
      text: GitHub
      link: https://github.com/zyno-io/k8s-failover-manager

features:
  - title: Automated Failover
    details: Toggle failover with a single field change. The operator handles traffic switching, pre/post actions, connection draining, and state synchronization automatically.
  - title: Multi-Cluster Sync
    details: Push-based state synchronization between primary and failover clusters with timestamp-based conflict resolution ensures both clusters stay in agreement.
  - title: Connection Draining
    details: A DaemonSet agent destroys stale TCP connections across all network namespaces on every node using Linux netlink, preventing clients from hanging on dead backends.
  - title: Extensible Actions
    details: Run HTTP webhooks, Kubernetes Jobs, scale Deployments/StatefulSets, wait for readiness, and coordinate with the remote cluster — all as ordered pre/post transition hooks with configurable retries and failure handling.
---
