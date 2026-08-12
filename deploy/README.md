# Deploying WVA

WVA is a KEDA **external scaler**: it decides how many replicas each llm-d model
needs and hands that to KEDA, which owns the HPA and does the scaling.

**Start with the [guides](../docs/guides/README.md).** Each is a complete path:

| | |
| --- | --- |
| [Install WVA in a namespace](../docs/guides/install-in-namespace/README.md) | the common case |
| [Install WVA for the whole cluster](../docs/guides/install-cluster-wide/README.md) | one controller for everything |
| [Add WVA to a running llm-d](../docs/guides/existing-llm-d/README.md) | llm-d already serving |
| [Cluster-admin setup](../docs/guides/admin-cluster-setup/README.md) | enabling a namespace's owner |
| [Bound every WVA by real GPUs](../docs/guides/admin-gpu-bounding/README.md) | the GPU budget |
| [Test against a full llm-d stack](../docs/guides/testing-with-llm-d/README.md) | kind, emulated GPUs |
| [Benchmark WVA](../docs/guides/benchmarking/README.md) | load and comparison |

## The four commands

| | |
| --- | --- |
| `make check-prereqs` | read-only: rights, namespace, Prometheus |
| `make setup-prereqs` | cluster admin, once per namespace |
| `make deploy-wva` | install; `INSTALL_PHASE=wva` for the controller alone |
| `make undeploy-wva` | remove |

`SCOPE=namespace` (default) or `cluster`. `ENVIRONMENT` is detected; set
`kubernetes` or `openshift` to force it. Everything else is optional —
[Configuration reference](../docs/deployment/configuration.md).

## Two things every path shares

**Nothing scales until a ScaledObject exists.** WVA has no watch and no listing:
it learns a workload exists only when KEDA calls it about one.

```bash
make scaledobjects-plan     # lists your model servers; applies nothing
make scaledobjects-apply
```

**Without a limiter, scaling is unbounded** — bounded only by each workload's
`maxReplicaCount`. [Bounding GPU usage](../docs/guides/admin-gpu-bounding/README.md)
is the one command that changes that, and it is an admin's to run.

> **Coming from llm-d's docs?** WVA ships **no CustomResourceDefinition** — a
> workload is registered by a KEDA ScaledObject naming WVA's external scaler — so
> any instruction to apply a `VariantAutoscaling` CRD, or to point an HPA at a WVA
> metric, predates that.
