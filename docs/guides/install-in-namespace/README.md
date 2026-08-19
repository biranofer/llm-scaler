# Install WVA in a namespace

## Overview

This guide installs the Workload-Variant-Autoscaler into one namespace, where it
sizes the llm-d model servers running there and nothing else. WVA decides how
many replicas each variant needs from saturation and cost, and hands the decision
to KEDA, which owns the HPA and does the scaling.

It is the common path, and it works the same whether llm-d is already serving or
you have just deployed it. A cluster admin runs one command first, once — see
[Cluster-admin setup](../admin-cluster-setup/) — after which the
namespace's owner installs and upgrades the controller themselves.

## Prerequisites

- llm-d model servers in the namespace
- KEDA, installed for you if the cluster has none
- a Prometheus scraping those model servers

If you want a model to scale **from zero**, its EPP must run with the
`flowControl` feature gate: that gate is what publishes the queue depth, and at
zero replicas there are no model-server metrics, so it is the only evidence that
anyone is asking for the model. WVA enables it on an EPP it installs itself; an
EPP that came from an llm-d guide has it off unless you turned it on. WVA reads
the queue by scraping the EPP, and falls back to reading the same metric from
Prometheus when it cannot — so a Prometheus that already scrapes your EPP covers
it. Without the gate, neither path has anything to read, and scale-from-zero
never fires while ordinary scaling works.

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
allowed to create. See [Cluster-admin setup](../admin-cluster-setup/)
for what each one is and why it needs an admin.

Already done for your namespace? `make check-prereqs` above names anything still
missing; if it names nothing, go to step 2.

### 2. Install the controller

<!-- guide:deploy.controller start -->
```bash
make deploy-wva
```
<!-- guide:deploy.controller end -->

It installs the controller only, and says so: creating cluster-scoped objects is
not something a namespace owner can do, so that half is the one step 1 covers. If
step 1 has not happened, this stops and names what is missing.

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

> **Running Fast Model Actuation?** The plan can carry a second entry for the
> same model — the FMA half — switched off by default, and an entry can arrive as
> `apply: no` until the launcher pods are scraped. See
> [WVA with Fast Model Actuation](../fma/).

## Verification

### 1. Check KEDA received the decision

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -n ${NAMESPACE}
```
<!-- guide:verify.objects end -->

A working registration looks like this:

```text
NAME                                        SCALETARGETKIND      SCALETARGETNAME    MIN  MAX  READY  ACTIVE  TRIGGERS
scaledobject.keda.sh/dev-model-decode-wva   apps/v1.Deployment   dev-model-decode   1    10   True   True    external-push

NAME                                                  REFERENCE                     TARGETS     MINPODS  MAXPODS  REPLICAS
horizontalpodautoscaler.autoscaling/keda-hpa-dev-...  Deployment/dev-model-decode   1/1 (avg)   1        10       1
```

Three things to read, in this order:

- **`READY True`** — KEDA reached WVA and got a metric spec back. The HPA is
  KEDA's; it creates one per ScaledObject.
- **`TARGETS` showing a number** — the decision is flowing. `1/1 (avg)` is WVA
  saying one replica is the right size.
- **`ACTIVE True`** — there is traffic. `Unknown` on a workload nobody is
  calling is normal, and not a fault.

`TARGETS` reading **`cpu: <unknown>/80%`** means the opposite of it looks: KEDA
could not fetch the metric spec from WVA and fell back to a CPU metric, so the
workload is not being scaled by WVA at all. `READY False` accompanies it. The
usual cause is a trigger naming a scaler it cannot reach, which is what a
ScaledObject written for a different install does — the shipped samples name the
default namespace, and a namespace-scoped install is not there. Repair it with:

```bash
make scaledobjects-repoint
```

It takes no arguments — it finds the install that is running and points the
object at it. It rewrites `scalerAddress` only, leaves your `modelID`,
`variantCost` and bounds untouched, and is idempotent. See
[First-line troubleshooting](../../deployment/operations.md#first-line-troubleshooting)
for the other causes.

### 2. Read the decisions

<!-- guide:verify.decisions start -->
```bash
kubectl logs -n ${NAMESPACE} deploy/wva-controller-manager | grep scaling-decision
```
<!-- guide:verify.decisions end -->

```text
scaling-decision {"modelID":"meta/llama","decisions":[{"name":"llama-decode-wva","curr":1,"tgt":3,"action":"scale-up"}]}
```

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
make undeploy-wva
```
<!-- guide:cleanup.uninstall end -->

This removes the ScaledObjects it created, and KEDA restores each workload to the
replica count it had before WVA sized it. Objects you adopted are left alone and
listed by name — they were not this installer's to delete — so repoint or remove
them yourself: their trigger now calls a scaler that is gone, KEDA keeps the HPA,
and a workload parked at zero can never be woken.

`UNDEPLOY_SCALEDOBJECTS=false` keeps everything, for reinstalling in place.

## Configuration

Optional. Everything below is detected; set one only to override what the
preflight reported.

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | the namespace running llm-d, discovered | `llm-d-optimized-baseline` |
| `IMG` | the image CI builds from main | `ghcr.io/you/wva:dev` |
| `PROMETHEUS_URL` | detected; Thanos on OpenShift | `http://prom.monitoring.svc:9090` |

Full list: [Configuration reference](../../deployment/configuration.md).

## Next

- [After the install](../../deployment/operations.md)
- [Bound every WVA by real GPUs](../admin-gpu-bounding/) — otherwise
  scaling is bounded only by each workload's `maxReplicaCount`
- [Scaling policy](../../developer-guide/scaling-policy-config.md)
