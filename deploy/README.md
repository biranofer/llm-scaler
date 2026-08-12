# Workload-Variant-Autoscaler Deployment Guide

> **This guide is authoritative for installing WVA.** llm-d's own
> [workload-autoscaling guide](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/README.wva.md)
> covers where WVA sits in llm-d, but check it against this page before following
> its install steps: WVA ships **no CustomResourceDefinition** — a workload is
> registered by a KEDA ScaledObject naming WVA's external scaler — and any
> instruction to apply a `VariantAutoscaling` CRD, or to point an HPA straight at
> a WVA metric, predates that.

WVA is a KEDA **external scaler**. It decides how many replicas each llm-d model
needs and hands that decision to KEDA over gRPC; KEDA owns the HPA and actuates it.

## Pick your path

Each path is complete on its own — start at one and follow it to the end.

| I want to… | path | who runs it |
| --- | --- | --- |
| **autoscale the models in my namespace** | [Install WVA in your namespace](../docs/deployment/install-in-namespace.md) | you, after one command from an admin |
| run one WVA for every namespace | [Install one WVA for the whole cluster](../docs/deployment/install-cluster-wide.md) | cluster admin |
| add WVA to a cluster already running llm-d | [Adding WVA to a running llm-d](../docs/deployment/existing-cluster.md) | whoever owns that install |

**Most people want the first one.** It is the shape a multi-tenant cluster
actually has, and it needs a cluster admin for exactly one command, once per
namespace.

### Cluster-admin paths

| I want to… | path |
| --- | --- |
| set up a namespace so its owner can install WVA themselves | [Cluster-admin setup](../docs/deployment/admin-cluster-setup.md) |
| make every WVA respect a real GPU budget | [Bounding GPU usage](../docs/deployment/admin-gpu-bounding.md) |

## The two things every path has in common

**1. Nothing scales until a ScaledObject exists.** WVA has no watch and no
listing — it only ever learns about a workload when KEDA calls it about one. Until
then the controller runs, reports healthy, and scales nothing.

```bash
make scaledobjects-plan     # lists your model servers; applies nothing
make scaledobjects-apply    # creates one ScaledObject per model server
```

The plan is an editable table: flip the first column to `yes`/`no`, correct a
model, change the bounds, then apply exactly what you left in it with
`WVA_DEFAULT_SO_PLAN=<file>`. See
[Default ScaledObjects](../docs/deployment/configuration.md#default-scaledobjects).

**2. Without a limiter, scaling is unbounded.** A fresh install scales to each
workload's `maxReplicaCount` and no further check is made against real GPUs. That
default is deliberate — see [Bounding GPU usage](../docs/deployment/admin-gpu-bounding.md)
for the one command that changes it, and why it is an admin's to run.

## How the install is split

Three phases, split where the *permissions* split. A namespace's owner can hold
the last one forever without ever holding cluster-scoped rights:

| phase | who | command |
| --- | --- | --- |
| 1. check | anyone | `make check-prereqs-<scope>-on-<platform>` |
| 2. prerequisites | **cluster admin**, once per namespace | `make setup-prereqs-<scope>-on-<platform> WVA_NS=<ns>` |
| 3. the controller | namespace owner | `make deploy-wva-<scope>-on-<platform> WVA_NS=<ns>` |

`<scope>` is `cluster` or `namespace`; `<platform>` is `k8s` or `openshift`.

Phase 3 refuses, naming every missing object, if phase 2 has not run. Each
install's cluster-scoped objects are suffixed with a hash of its namespace, so two
installs on one cluster can never take each other's.

`make deploy-wva-on-k8s` / `deploy-wva-on-openshift` still do all three at once,
which is the right thing when you are an admin installing for yourself.

## Reference

| page | covers |
| --- | --- |
| [Configuration reference](../docs/deployment/configuration.md) | every environment variable `install.sh` reads |
| [After the install](../docs/deployment/operations.md) | verification, what to watch, first-line troubleshooting |
| [The GPU limiter](../docs/deployment/gpu-limiter.md) | why policy lives where it does, and the accelerator precondition |
| [Scaling policy](../docs/developer-guide/scaling-policy-config.md) | thresholds, analyzers and policy tiers |

## What the install needs from you

| you provide | why |
| --- | --- |
| a Prometheus URL | WVA reads model-server metrics from it. `PROMETHEUS_URL=…`; an existing one is detected where it can be |
| KEDA | it owns the HPA and calls WVA. The installer adds it if the cluster has none |
| model servers labelled `llm-d.ai/inferenceServing=true` | how `scaledobjects-plan` finds what to register |
| `IMG=<image>` for an unreleased build | the default is the published image, which will not match an unmerged branch's manifests |
