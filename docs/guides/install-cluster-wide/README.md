# Install WVA for the whole cluster

## Overview

Installs one Workload-Variant-Autoscaler that sizes the llm-d model servers in
**every** namespace. WVA decides how many replicas each variant needs and hands
the decision to KEDA, which owns the HPA and does the scaling.

Use this when one team runs the cluster. Where namespaces belong to different
teams, prefer one controller per namespace — see
[Install WVA in a namespace](../install-in-namespace/README.md) — which keeps
failure domains, policy and upgrades separate.

Needs a cluster admin: the controller reads workloads in every namespace.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `SCOPE` | `namespace` — **set `cluster` for this guide** | `cluster` |
| `WVA_NS` | `workload-variant-autoscaler-system` | `wva-system` |
| `IMG` | the published image | `ghcr.io/you/wva:dev` |

Full list: [Configuration reference](../../deployment/configuration.md).

## Prerequisites

- cluster-admin rights
- KEDA — installed for you if the cluster has none
- Prometheus — detected
- llm-d model servers somewhere on the cluster

<!-- guide:prerequisites.check start -->
```bash
make check-prereqs SCOPE=cluster
```
<!-- guide:prerequisites.check end -->

## Installation

<!-- guide:deploy.all start -->
```bash
make deploy-wva SCOPE=cluster
```
<!-- guide:deploy.all end -->

One command: prerequisites and controller. Add `IMG=<your build>` to install an
unmerged branch — the default is a published image, which will not match a
branch's manifests.

### Register the workloads

**Nothing scales until a ScaledObject exists.**

<!-- guide:deploy.register start -->
```bash
make scaledobjects-plan
make scaledobjects-apply
```
<!-- guide:deploy.register end -->

At this scope the plan covers every namespace holding model servers.

## Verification

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -A
```
<!-- guide:verify.objects end -->

`CurrentMetrics` populated on the HPA means the whole chain works.

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
make undeploy-wva SCOPE=cluster
```
<!-- guide:cleanup.uninstall end -->

Prometheus, KEDA and the namespaces stay — they are shared, and this install may
not have created them. `UNDEPLOY_SHARED=true` removes them too.

## Next

- [Bounding GPU usage](../admin-gpu-bounding/README.md) — without a limiter,
  scaling is bounded only by each workload's `maxReplicaCount`
- [After the install](../../deployment/operations.md)
- [Install methods](../../deployment/install-methods.md) — GitOps, direct
  Kustomize, and what the script does
