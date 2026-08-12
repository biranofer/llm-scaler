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

## Which install are you doing?

| your cluster | start here |
| --- | --- |
| nothing yet, or a test cluster | [Installing on a new cluster](../docs/deployment/new-cluster.md) — WVA, Prometheus, KEDA, namespaces |
| **already running llm-d** | [Adding WVA to a running llm-d](../docs/deployment/existing-cluster.md) — controller only, pointed at the Prometheus you have |

The difference matters: the new-cluster path deploys a Prometheus and an llm-d
namespace. On a cluster that already has both, that leaves you with a second
Prometheus WVA is not reading and an empty namespace that looks like the place to
deploy models.

## Quickstart (new cluster)

```bash
make check-prereqs        # tools, versions, cluster reachability - read-only
make deploy-wva-on-k8s    # cluster-scoped, default namespace, no GPU limiter
```

## Every deployment option, as one command

Each row is a complete install. Pick the row that matches who you are and what the
cluster already has; the rest of this guide explains the pieces.

| I am… | command | what it does |
| --- | --- | --- |
| a cluster admin, one WVA for everything | `make deploy-wva-on-k8s` | manages **every** namespace. Creates cluster-scoped RBAC. The usual choice. |
| a cluster admin, one WVA per team | `make deploy-wva-on-k8s WVA_SCOPE=namespace WVA_NS=team-a WVA_ADMIN_GRANTS=true` | manages **one** namespace. Separate failure domains per team; keeps authenticated metrics and node access. |
| a cluster admin, keeping the controller out of the team's reach | `make deploy-wva-on-k8s WVA_SCOPE=namespace WVA_NS=wva-team-a WVA_WATCH_NS=team-a WVA_ADMIN_GRANTS=true` | controller **runs in** `wva-team-a`, **manages** `team-a`. The team cannot edit the controller, so limits placed on them hold. |
| a **namespace admin**, no cluster rights | `make deploy-wva-on-k8s WVA_SCOPE=namespace WVA_NS=team-a` | **Kubernetes only.** Creates **no cluster-scoped object**, so you can run it yourself. No `gpu-inventory` limiter, no authenticated metrics, no EPP metrics. |
| adding WVA to a cluster that already has llm-d | `make deploy-wva-on-k8s PROMETHEUS_URL=https://prom.monitoring.svc:9090` | controller only. The cluster's Prometheus, KEDA and CRDs are used as they are. |
| bounding scaling by real GPUs | add `WVA_LIMITER=gpu-inventory` | allocates from per-accelerator pools. Needs node read; the install fails without it. |
| bounding scaling by declared caps | add `WVA_LIMITER=quota` | bounds from config. Needs no cluster-scoped access. |
| removing it | `make undeploy-wva-on-k8s` | removes WVA's own objects. Prometheus, KEDA, EPP and the namespace stay — they are shared, and this install may not have created them. Add `UNDEPLOY_SHARED=true` to remove them too. |

Two things every row has in common:

- **Nothing scales until a ScaledObject exists** — see the next section.
- **Without a limiter, scaling is unbounded.** `WVA_LIMITER` is how you bound it,
  and [the GPU limiter](../docs/deployment/gpu-limiter.md) explains who is allowed
  to set it.

Add `-e openshift` (or use `make deploy-wva-on-openshift`) on OpenShift, where
`WVA_SCOPE` defaults to `namespace`. **That default is not the self-service row
above**: on OpenShift the namespace-scoped overlay still creates 3 ClusterRoles
and 5 ClusterRoleBindings, because the platform's monitoring wiring
(`cluster-monitoring-view` for Thanos and for user-workload Prometheus) is
cluster-scoped and the controller cannot reach Prometheus without it. There,
namespace scope buys blast-radius reduction, not installability by a tenant.

`make check-prereqs` will tell you which you have: it renders the overlay your
install would apply and asks whether you may create each kind in it.

### Installing WVA is not the last step

**A ScaledObject is the registration.** WVA has no watch and no listing - it only
learns about workloads KEDA calls it about, so until one exists it scales nothing,
quietly. Look at what it would create, then create it:

```bash
make scaledobjects-plan     # lists your model servers; applies nothing
make scaledobjects-apply    # creates one ScaledObject per model server
```

The plan is an editable table - set the first column to `yes`/`no`, correct a
model, change the replica bounds - and `make scaledobjects-apply
WVA_DEFAULT_SO_PLAN=<file>` applies exactly what you left in it. Also:
`WVA_DEFAULT_SO_NS=all` for every namespace, `WVA_DEFAULT_SO_ADOPT=true` to take
over ScaledObjects that already exist, `WVA_DEFAULT_SO_TEMPLATE=<file>` for your
own shape. See
[Default ScaledObjects](../docs/deployment/configuration.md#default-scaledobjects).

## Guides

| guide | covers |
| --- | --- |
| [Installing on a new cluster](../docs/deployment/new-cluster.md) | the full stack: install decisions, both methods, platform notes |
| [Adding WVA to a running llm-d](../docs/deployment/existing-cluster.md) | controller only; pointing it at your Prometheus, and connecting your workloads |
| [Configuration reference](../docs/deployment/configuration.md) | every environment variable `install.sh` reads |
| [GPU limiter](../docs/deployment/gpu-limiter.md) | bounding scaling, and the accelerator-resolution precondition |
| [After the install](../docs/deployment/operations.md) | verification, what to watch, first-line troubleshooting |
| [Troubleshooting](../docs/developer-guide/troubleshooting.md) | deeper diagnosis, including scale-from-zero |
| [Testing](../docs/developer-guide/testing.md) | unit, envtest and e2e suites |

Platform specifics: [Kubernetes](kubernetes/README.md) &middot;
[OpenShift](openshift/README.md) &middot; [kind (local)](kind-emulator/README.md)

## What the install needs from you

| decision | variable | default |
| --- | --- | --- |
| namespace | `WVA_NS` | `workload-variant-autoscaler-system` |
| scope | `WVA_SCOPE` | `namespace` on OpenShift, `cluster` elsewhere |
| GPU limiter | `WVA_LIMITER` | `none` |
| scale-to-zero | `ENABLE_SCALE_TO_ZERO` | `true` |
| default ScaledObjects | `WVA_DEFAULT_SO` | `false` (`plan` / `edit` / `true`) |
| deploy Prometheus | `DEPLOY_PROMETHEUS` | `true` — set `false` on a cluster that has one |
| Prometheus address | `PROMETHEUS_URL` | the one this install deploys — **required** when it does not |

**One WVA per cluster, or one per namespace.** The install refuses to sit next to
an existing one: two unpartitioned controllers both manage every unlabelled
workload and both write a decision for the same ScaledObject, so the replica count
becomes whichever wrote last. Installing into the same `WVA_NS` is an upgrade and
is allowed; to run several deliberately, give each a `CONTROLLER_INSTANCE`. See
[One WVA per cluster](../docs/deployment/existing-cluster.md#how-many-wvas-a-cluster-has).

Pass the same `WVA_NS` and `WVA_SCOPE` to `undeploy-wva-on-*`: an uninstall
resolves the overlay exactly as the install did, so a mismatch leaves behind
precisely the resources the other overlay owns.

Requirements are checked, not listed - `make check-prereqs` runs the same check the
install runs and reports the specific tool, the version found and the version
needed.
