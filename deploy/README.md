# Workload-Variant-Autoscaler Deployment Guide

WVA is a KEDA **external scaler**. It works out how many replicas each llm-d model
needs and hands that decision to KEDA over gRPC; KEDA owns the HPA and does the
scaling. WVA never writes to your workloads — its only write is Events.

## Before you start

Three things about the cluster. The installer handles the first two where it can:

| | required | who provides it |
| --- | --- | --- |
| **KEDA** | yes — WVA is a KEDA scaler, so without it nothing scales at all | the installer adds it if the cluster has none. On OpenShift it is platform-managed and never installed by us |
| **Prometheus** | yes — WVA reads model-server metrics from it | **detected.** You do not pass a URL. On OpenShift it is the platform's Thanos Querier, at a fixed address |
| **model servers** labelled `llm-d.ai/inferenceServing=true` | yes | llm-d's own install does this |

Check all of it, read-only, before you commit to anything:

```bash
make check-prereqs-namespace-on-k8s WVA_NS=<your-namespace>
```

It renders the manifests this install would apply, asks the API server whether you
may create each kind, and prints the Prometheus it found.

### Which namespace is which

The single most common confusion, so plainly:

| variable | means | default |
| --- | --- | --- |
| `WVA_NS` | where the **controller runs** | `workload-variant-autoscaler-system` |
| `WVA_WATCH_NS` | which namespace it **manages** | the one it runs in |
| `WVA_DEFAULT_SO_NS` | where `scaledobjects-plan` **looks for model servers** | what this install can reach |
| `LLMD_NS` | only for `install-epp.sh` and the sample paths. **It does not tell WVA anything** | `llm-d-optimized-baseline` |

**Yes, `WVA_NS` can be your llm-d namespace** — that is the normal shape: the
controller runs alongside the models it manages. Set `WVA_NS=<your-llm-d-namespace>`
and you are done.

**Several namespaces running llm-d?** A namespace-scoped controller manages exactly
one, so pick one of:

- **one WVA per namespace** — repeat the namespace path for each. They are
  independent: separate failure domains, separate policy, separate upgrades.
- **one cluster-scoped WVA** — [installs once](../docs/deployment/install-cluster-wide.md)
  and manages every namespace.

They do share the GPUs, whichever you choose. Bounding that is
[an admin's job](../docs/deployment/admin-gpu-bounding.md).

## Pick your path

Each path is complete on its own — start at one and follow it to the end.

| I want to… | path | who runs it |
| --- | --- | --- |
| **autoscale the models in my namespace** | [Install WVA in your namespace](../docs/deployment/install-in-namespace.md) | you, after one command from an admin |
| run one WVA for every namespace | [Install one WVA for the whole cluster](../docs/deployment/install-cluster-wide.md) | cluster admin |
| add WVA to a cluster already running llm-d | [Adding WVA to a running llm-d](../docs/deployment/existing-cluster.md) | whoever owns that install |

**Most people want the first.** It is the shape a multi-tenant cluster actually
has, and it needs an admin for exactly one command, once per namespace.

For cluster admins:

| I want to… | path |
| --- | --- |
| let a namespace's owner install WVA themselves | [Cluster-admin setup](../docs/deployment/admin-cluster-setup.md) |
| make every WVA respect a real GPU budget | [Bounding GPU usage](../docs/deployment/admin-gpu-bounding.md) |

## Two things every path shares

**1. Nothing scales until a ScaledObject exists.** WVA has no watch and no
listing — it learns a workload exists only when KEDA calls it about one. Until
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
workload's `maxReplicaCount`, with no check against real GPUs. That default is
deliberate — see [Bounding GPU usage](../docs/deployment/admin-gpu-bounding.md)
for the one command that changes it, and why it is an admin's to run.

## How the install is split

Three phases, split where the *permissions* split, so a namespace's owner can hold
the last one forever without ever holding cluster-scoped rights:

| phase | who | command |
| --- | --- | --- |
| 1. check | anyone | `make check-prereqs-<scope>-on-<platform>` |
| 2. prerequisites | **cluster admin**, once per namespace | `make setup-prereqs-<scope>-on-<platform> WVA_NS=<ns>` |
| 3. the controller | namespace owner | `make deploy-wva-<scope>-on-<platform> WVA_NS=<ns>` |

`<scope>` is `cluster` or `namespace`; `<platform>` is `k8s` or `openshift`.

Phase 3 refuses, naming every missing object, if phase 2 has not run. Each
install's cluster-scoped objects carry a hash of its namespace, so two installs on
one cluster can never take each other's.

`make deploy-wva-on-k8s` / `deploy-wva-on-openshift` still do all three at once,
which is right when you are an admin installing for yourself.

## Reference

| page | covers |
| --- | --- |
| [Configuration reference](../docs/deployment/configuration.md) | every environment variable `install.sh` reads |
| [After the install](../docs/deployment/operations.md) | verification, what to watch, first-line troubleshooting |
| [The GPU limiter](../docs/deployment/gpu-limiter.md) | why policy lives where it does, and the accelerator precondition |
| [Scaling policy](../docs/developer-guide/scaling-policy-config.md) | thresholds, analyzers and policy tiers |

> **Coming from llm-d's own docs?** Its
> [workload-autoscaling guide](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/README.wva.md)
> places WVA in llm-d, but check its install steps against this guide first: WVA
> ships **no CustomResourceDefinition** — a workload is registered by a KEDA
> ScaledObject naming WVA's external scaler — so any instruction to apply a
> `VariantAutoscaling` CRD, or to point an HPA straight at a WVA metric, predates
> that.
