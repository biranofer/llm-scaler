# Workload-Variant-Autoscaler Deployment Guide

> **Note:** This guide is developer/operator-oriented and covers deploying WVA from source. If you are an end user looking to install WVA as part of llm-d, see the [llm-d workload-autoscaling guide](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/README.wva.md) instead.

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
[One WVA per cluster](../docs/deployment/existing-cluster.md#one-wva-per-cluster-or-one-per-namespace--never-two-managing-the-same-workloads).

Pass the same `WVA_NS` and `WVA_SCOPE` to `undeploy-wva-on-*`: an uninstall
resolves the overlay exactly as the install did, so a mismatch leaves behind
precisely the resources the other overlay owns.

Requirements are checked, not listed - `make check-prereqs` runs the same check the
install runs and reports the specific tool, the version found and the version
needed.
