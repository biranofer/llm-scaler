# Bounding scaling: the GPU limiter

WVA scales without a GPU budget unless you give it one. This is how, and what has
to be true first.

> Part of the [WVA deployment guide](../../deploy/README.md).

The shipped configuration declares **no limiter**: a fresh install scales
unconstrained, and a scale-from-zero wake is published without a capacity check.
That default is deliberate. A GPU-aware optimizer allocates out of per-accelerator
pools, so a variant whose accelerator it cannot resolve is charged to no pool, gets
no budget, and **never scales up** — silently, because nothing errors. Enabling it
by default would freeze exactly the workloads that are least carefully configured.

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

The install warns too: enabling `WVA_LIMITER=gpu-inventory` counts the distinct GPU
products the cluster advertises and tells you whether pinning is needed. An
unresolved variant is logged once per change and counted in
`wva_unattributed_gpus`. See
[GPU Capacity Accounting](../developer-guide/gpu-capacity-accounting.md).

