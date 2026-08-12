# Install WVA in a namespace

## Overview

This guide installs the Workload-Variant-Autoscaler into one namespace, where it
sizes the llm-d model servers running there and nothing else. WVA decides how
many replicas each variant needs from saturation and cost, and hands the decision
to KEDA, which owns the HPA and does the scaling.

It is the common path, and it works the same whether llm-d is already serving or
you have just deployed it. A cluster admin runs one command first, once — see
[Cluster-admin setup](../admin-cluster-setup/README.md) — after which the
namespace's owner installs and upgrades the controller themselves.

## Prerequisites

- llm-d model servers in the namespace
- KEDA, installed for you if the cluster has none
- a Prometheus scraping those model servers

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

## Installation Instructions

### 1. Cluster admin: prepare the namespace

**This step needs cluster-admin rights** — everything after it does not.

<!-- guide:deploy.prereqs start -->
```bash
make setup-prereqs
```
<!-- guide:deploy.prereqs end -->

Once per namespace, not once per upgrade. It creates the namespace, the
cluster-scoped RBAC and the ServiceMonitor: the objects a namespace admin is not
allowed to create. See [Cluster-admin setup](../admin-cluster-setup/README.md)
for what each one is and why it needs an admin.

Already done for your namespace? `make check-prereqs` above names anything still
missing; if it names nothing, go to step 2.

### 2. Install the controller

<!-- guide:deploy.controller start -->
```bash
make deploy-wva INSTALL_PHASE=wva
```
<!-- guide:deploy.controller end -->

If step 1 has not happened, this stops and names every missing object.

### 3. Register the workloads

Nothing scales until a ScaledObject exists: WVA is only ever asked about
workloads KEDA calls it about.

<!-- guide:deploy.register start -->
```bash
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml: apply: yes|no|adopt, the modelID, the replica bounds
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```
<!-- guide:deploy.register end -->

`scaledobjects-plan` finds the model servers and writes one entry each, applying
nothing:

```yaml
plan:

  - apply: "yes"                        # yes | no | adopt
    namespace: llm-d-sim
    kind: Deployment
    name: dev-model-decode
    modelID: "meta/llama"               # required — what the container serves
    minReplicas: 1
    maxReplicas: 10
    variantCost: "10.0"
    inferencePool: "optimized-baseline" # informational
```

Edit it; `scaledobjects-apply` does exactly what you left in it. Every field is
explained in the comments the file is written with, so there is nothing to look
up. `apply: adopt` is for a workload something else already scales: it repoints
that object at WVA instead of adding a second, because two ScaledObjects on one
target is two HPAs writing the same replica count. A workload whose model could
not be read is written as `no` with the reason, and never created without one.

## Verification

### 1. Check KEDA received the decision

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -n ${NAMESPACE}
```
<!-- guide:verify.objects end -->

The HPA is KEDA's. `CurrentMetrics` populated means the whole chain works: KEDA
called WVA, WVA decided, KEDA got the answer.

### 2. Read the decisions

<!-- guide:verify.decisions start -->
```bash
kubectl logs -n ${NAMESPACE} deploy/wva-controller-manager | grep scaling-decision
```
<!-- guide:verify.decisions end -->

```
scaling-decision {"modelID":"meta/llama","decisions":[{"name":"llama-decode-wva","curr":1,"tgt":3,"action":"scale-up"}]}
```

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
kubectl delete scaledobject --all -n ${NAMESPACE}
make undeploy-wva
```
<!-- guide:cleanup.uninstall end -->

Delete the ScaledObjects as well, unless you are reinstalling: their trigger
points at a scaler that no longer exists, so KEDA keeps the HPA and keeps calling
nothing — and a workload parked at zero can never be woken.

## Configuration

Optional. Everything below is detected; set one only to override what the
preflight reported.

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | the namespace running llm-d, discovered | `llm-d-optimized-baseline` |
| `IMG` | the published image | `ghcr.io/you/wva:dev` |
| `PROMETHEUS_URL` | detected; Thanos on OpenShift | `http://prom.monitoring.svc:9090` |

Full list: [Configuration reference](../../deployment/configuration.md).

## Next

- [After the install](../../deployment/operations.md)
- [Bound every WVA by real GPUs](../admin-gpu-bounding/README.md) — otherwise
  scaling is bounded only by each workload's `maxReplicaCount`
- [Scaling policy](../../developer-guide/scaling-policy-config.md)
