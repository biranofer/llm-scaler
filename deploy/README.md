# Workload-Variant-Autoscaler Deployment Guide

> **Note:** This guide is developer/operator-oriented and covers deploying WVA from source. If you are an end user looking to install WVA as part of llm-d, see the [llm-d workload-autoscaling guide](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/README.wva.md) instead.

WVA is a KEDA **external scaler**. It decides how many replicas each llm-d model
needs and hands that decision to KEDA over gRPC; KEDA owns the HPA and actuates it.

## Quickstart

```bash
make check-prereqs        # tools, versions, cluster reachability - read-only
make deploy-wva-on-k8s    # cluster-scoped, default namespace, no GPU limiter
```

### Then give WVA something to manage

**A ScaledObject is the registration.** WVA has no watch and no listing - it only
learns about workloads KEDA calls it about, so until one exists it will scale
nothing, quietly. Installing WVA is not the last step.

Let it find your model servers and show you what it would create:

```bash
make scaledobjects-plan          # lists them; applies nothing
make scaledobjects-apply         # creates one per model server
```

`scaledobjects-plan` writes an editable table - set the first column to `yes`/`no`,
correct a model, change the replica bounds - and `make scaledobjects-apply
WVA_DEFAULT_SO_PLAN=<file>` applies exactly what you left in it. Add
`WVA_DEFAULT_SO_NS=all` to cover every namespace instead of just `$LLMD_NS`. See
[Default ScaledObjects](../docs/deployment/configuration.md#default-scaledobjects).

Or write your own:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: llama-8b-scaler
  namespace: llm-inference
spec:
  scaleTargetRef: { apiVersion: apps/v1, kind: Deployment, name: llama-8b }
  minReplicaCount: 1
  maxReplicaCount: 10
  triggers:
    - type: external-push
      name: wva-external-scaler
      metadata:
        scalerAddress: wva-external-scaler.workload-variant-autoscaler-system.svc.cluster.local:9090
        modelID: meta/llama-3-8b     # the only field you must supply
        scalingPolicy: interactive   # optional: a named policy tier
```

`modelID` is the only required field. The accelerator, the role, GPUs per replica
and the InferencePool are **derived** from the workload, so they cannot drift from
reality.

## Guides

| guide | covers |
| --- | --- |
| [Installing WVA](../docs/deployment/installation.md) | the install decisions, both methods, platform notes |
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

Pass the same `WVA_NS` and `WVA_SCOPE` to `undeploy-wva-on-*`: an uninstall
resolves the overlay exactly as the install did, so a mismatch leaves behind
precisely the resources the other overlay owns.

Requirements are checked, not listed - `make check-prereqs` runs the same check the
install runs and reports the specific tool, the version found and the version
needed.
