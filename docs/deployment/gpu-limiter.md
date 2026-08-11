# Bounding scaling: the GPU limiter

WVA scales without a GPU budget unless you give it one. This is how, and what has
to be true first.

> Part of the [WVA deployment guide](../../deploy/README.md).

## Turning it on

```bash
make deploy-wva-on-k8s WVA_LIMITER=gpu-inventory   # bound by GPUs actually free
make deploy-wva-on-k8s WVA_LIMITER=quota           # bound by declared caps
```

or later, by adding a `limiters:` entry to the `default` entry of the
scaling-policy ConfigMap — applied live, no restart. **Read the next section
first.**

## Why it ships off

The shipped configuration declares **no limiter**: a fresh install scales
unconstrained, and a scale-from-zero wake is published without a capacity check.
That default is deliberate. A GPU-aware optimizer allocates out of per-accelerator
pools, so a variant whose accelerator it cannot resolve is charged to no pool, gets
no budget, and **never scales up** — silently, because nothing errors. Enabling it
by default would freeze exactly the workloads that are least carefully configured.

## What has to be true first: every accelerator must resolve

WVA resolves a variant's accelerator from, in order:

1. a **GPU product key in the workload's `nodeSelector` or `nodeAffinity`** — the
   only source that works before any pod exists, and therefore the only one that
   works for a workload parked at zero;
2. the **node its pods are running on**, once it has ready pods.

On a **single-accelerator** cluster neither is needed: there is one pool, so an
unconstrained workload is deduced onto it. On a **heterogeneous** cluster it
matters, and not as bookkeeping — a pod with no GPU nodeSelector can be scheduled
onto *any* GPU node, so a workload that does not state its accelerator genuinely
has not chosen one. Pin it:

```yaml
# in the model server's pod template
spec:
  nodeSelector:
    nvidia.com/gpu.product: NVIDIA-A100-PCIE-80GB
```

```bash
kubectl get nodes -L nvidia.com/gpu.product          # what your nodes advertise
kubectl logs -n workload-variant-autoscaler-system   -l app.kubernetes.io/name=workload-variant-autoscaler | grep -i "Accelerator not resolved"
```

## Permission: nodes

The limiter reads **nodes** to learn what GPUs exist, and nodes are cluster-scoped.

WVA reads nodes on every cycle regardless of the limiter — a variant's accelerator
is resolved from the nodes its pods run on, and that identity is what the capacity
model keys learned per-replica capacity by. What the limiter changes is whether
that identity is also used to charge the variant to a GPU **budget**.

The consequence of a missing node permission therefore depends on the limiter:

| limiter | controller can list nodes | outcome |
| --- | --- | --- |
| `none` | no | degraded: accelerators stay unresolved, so metrics lose the accelerator label and the capacity model cannot reuse learned capacity across variants |
| `gpu-inventory` | no | **install refused.** Every variant would be charged to no pool, receive no budget and stop scaling up, silently |
| `gpu-inventory` | yes | as intended |

`make check-prereqs WVA_LIMITER=gpu-inventory` checks this before you install.

### If the limiter is turned on later

An admin can add a `limiters:` entry to the scaling-policy ConfigMap at any time,
and it is applied live — including to a controller that was installed without node
permission. That combination is the one failure with no natural symptom, so it is
reported three ways:

- an **error log**, naming what has stopped working;
- `wva_node_access_denied` set to **1**;
- the **`WVANodeAccessDenied`** alert (critical, fires after 2m) if you installed
  with `DEPLOY_ALERTING_RULES=true`.

## Checking

The install warns too: enabling `WVA_LIMITER=gpu-inventory` counts the distinct GPU
products the cluster advertises and tells you whether pinning is needed. An
unresolved variant is logged once per change and counted in
`wva_unattributed_gpus`. See
[GPU Capacity Accounting](../developer-guide/gpu-capacity-accounting.md).

