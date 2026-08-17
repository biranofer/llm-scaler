# WVA guides

Each guide takes one reader from nothing to a working install. Follow one; they
do not need to be combined.

```bash
source docs/guides/env.sh
```

## Installing

| guide | for |
| --- | --- |
| [Install WVA in a namespace](install-in-namespace/README.md) | **start here.** One team, one namespace — whether or not llm-d is already serving |
| [Install WVA for the whole cluster](install-cluster-wide/README.md) | one controller for every namespace |

## Cluster administration

| guide | for |
| --- | --- |
| [Cluster-admin setup for a namespace](admin-cluster-setup/README.md) | let a namespace's owner install WVA themselves |
| [Bound every WVA by real GPUs](admin-gpu-bounding/README.md) | scaling is otherwise bounded only by `maxReplicaCount` |

## Advanced

| guide | for |
| --- | --- |
| [Scale a model to zero, and get it back](scale-to-zero/README.md) | release an idle model's accelerators — and check it can wake before it parks |
| [Test against a full llm-d stack](testing-with-llm-d/README.md) | llm-d + WVA on kind, emulated GPUs, no hardware |
| [Benchmark WVA](benchmarking/README.md) | drive load through a real stack and compare runs |
| [Autoscale a Fast Model Actuation stack](fma/README.md) | FMA runs the engine in a pod no ScaledObject owns |

## Reference

| page | covers |
| --- | --- |
| [Configuration](../deployment/configuration.md) | every variable the installer reads |
| [After the install](../deployment/operations.md) | what to watch, first-line troubleshooting |
| [Install methods](../deployment/install-methods.md) | GitOps, direct Kustomize, what the script does |
| [The GPU limiter](../deployment/gpu-limiter.md) | where policy lives, and the accelerator precondition |

## Editing a guide

Bash blocks between `<!-- guide:… start -->` markers are generated from
`guide.yaml`. Edit the YAML, then:

```bash
make guides-render     # rewrite the blocks
make guides-check      # CI: fail if a README has drifted
```

Prose outside the markers is preserved. The commands a reader copies are the
commands the YAML declares, so a guide cannot drift from what it documents.
