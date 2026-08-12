# Install WVA in a namespace

## Overview

Installs the Workload-Variant-Autoscaler into one namespace, where it sizes the
llm-d model servers in that namespace and nothing else. WVA decides how many
replicas each variant needs and hands the decision to KEDA, which owns the HPA
and does the scaling.

This is the common path. It needs a cluster admin for one command, once — see
[Cluster-admin setup](../admin-cluster-setup/README.md) — after which the
namespace's owner installs and upgrades the controller themselves.

## Configuration

Everything is detected. Set these only to override what the preflight reports.

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | the namespace running llm-d, discovered | `llm-d-optimized-baseline` |
| `IMG` | the published image | `ghcr.io/you/wva:dev` |
| `PROMETHEUS_URL` | detected; Thanos on OpenShift | `http://prom.monitoring.svc:9090` |

Full list: [Configuration reference](../../deployment/configuration.md).

## Prerequisites

- KEDA — installed for you if the cluster has none
- Prometheus — detected
- llm-d model servers in the namespace
- a cluster admin has run [`make setup-prereqs`](../admin-cluster-setup/README.md) for it

<!-- guide:env.static.namespace start -->
```bash
export NAMESPACE=<your-namespace>
```
<!-- guide:env.static.namespace end -->

<!-- guide:prerequisites.check start -->
```bash
make check-prereqs
```
<!-- guide:prerequisites.check end -->

Read-only. It reports the namespace it resolved, whether that namespace holds
model servers, and the Prometheus it found. **An empty namespace is a warning
worth reading**: a controller pointed at the wrong one installs cleanly, reports
healthy and scales nothing.

## Installation

<!-- guide:deploy.controller start -->
```bash
make deploy-wva INSTALL_PHASE=wva
```
<!-- guide:deploy.controller end -->

If the admin step has not happened, this stops and names every missing object.

### Register the workloads

**Nothing scales until a ScaledObject exists** — WVA is only ever asked about
workloads KEDA calls it about.

<!-- guide:deploy.register start -->
```bash
make scaledobjects-plan
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=/tmp/wva-scaledobject-plan.XXXX
```
<!-- guide:deploy.register end -->

`scaledobjects-plan` writes an editable table, one row per model server, and
applies nothing. `scaledobjects-apply` applies exactly what you leave in it.

## Verification

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -n ${NAMESPACE}
```
<!-- guide:verify.objects end -->

The HPA is KEDA's. **`CurrentMetrics` populated means the whole chain works**:
KEDA called WVA, WVA decided, KEDA got the answer.

<!-- guide:verify.decisions start -->
```bash
kubectl logs -n ${NAMESPACE} deploy/wva-controller-manager | grep scaling-decision
```
<!-- guide:verify.decisions end -->

```
scaling-decision {"modelID":"meta/llama","decisions":[{"name":"llama-decode-wva","curr":1,"tgt":3,"action":"scale-up"}]}
```

## Upgrading

Re-run the install step with a newer `IMG`. No admin needed.

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
kubectl delete scaledobject --all -n ${NAMESPACE}
make undeploy-wva
```
<!-- guide:cleanup.uninstall end -->

Delete the ScaledObjects too, as above, unless you are reinstalling: their
trigger points at a scaler that no longer exists, so KEDA keeps the HPA and keeps
calling nothing — and a workload parked at zero can never be woken.

## Next

- [After the install](../../deployment/operations.md)
- [Bounding GPU usage](../admin-gpu-bounding/README.md) — an admin's command; a
  controller is otherwise bounded only by each workload's `maxReplicaCount`
- [Scaling policy](../../developer-guide/scaling-policy-config.md)
