# WVA guides

Well-lit paths for installing and operating the Workload-Variant-Autoscaler, in
the shape of [llm-d's own guides](https://github.com/llm-d/llm-d/tree/main/guides):
each guide is two files — a machine-readable `guide.yaml` and a human-readable
`README.md` — and each is complete on its own.

Source the shared environment before running guide commands. It carries versions
and image references; the namespace belongs to the guide you are following:

```bash
source docs/guides/env.sh
```

## Installing

| guide | for | who runs it |
| --- | --- | --- |
| [Install WVA in your namespace](install-in-namespace/README.md) | **the common path** — you own a namespace and want your models autoscaled | you, after one command from an admin |
| [Install one WVA for the whole cluster](install-cluster-wide/README.md) | one controller managing every namespace | cluster admin |
| [Add WVA to a running llm-d](existing-llm-d/README.md) | llm-d is already up; add the autoscaler to it | whoever owns that install |

## Operating (cluster admin)

| guide | for |
| --- | --- |
| [Cluster-admin setup](admin-cluster-setup/README.md) | let a namespace's owner install and upgrade WVA themselves |
| [Bounding GPU usage](admin-gpu-bounding/README.md) | make every WVA on the cluster respect a real GPU budget |

## Two things every guide shares

**Nothing scales until a ScaledObject exists.** WVA has no watch and no listing —
it learns a workload exists only when KEDA calls it about one. Until then the
controller runs, reports healthy, and scales nothing.

**Without a limiter, scaling is unbounded.** A fresh install scales to each
workload's `maxReplicaCount` with no check against real GPUs. That default is
deliberate; [Bounding GPU usage](admin-gpu-bounding/README.md) is the one command
that changes it.

## Reference

| page | covers |
| --- | --- |
| [Configuration](../deployment/configuration.md) | every environment variable `install.sh` reads |
| [After the install](../deployment/operations.md) | what to watch, and first-line troubleshooting |
| [The GPU limiter](../deployment/gpu-limiter.md) | why policy lives where it does, and the accelerator precondition |

## Editing a guide

The bash blocks between `<!-- guide:… start -->` and `<!-- guide:… end -->` are
**generated from `guide.yaml`** — edit the YAML, then:

```bash
make guides-render     # rewrite the README blocks
make guides-check      # CI: fail if a README is out of date
```

Prose outside the markers is preserved byte for byte. The point is that the
commands a reader copies and the commands a tool would run are the same strings:
this repo has twice shipped documentation that drifted from what it described —
a benchmark that installed a different binary than it claimed, and a guide that
told people to apply a CRD that does not exist.
