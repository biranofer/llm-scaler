# Bridge a scale-up with a warm pool

A scale-up is slow because a new replica has to load a model before it can serve.
A warm pool holds a small number of Pods with models already loaded and asleep,
and lends one to a variant that is scaling up. The borrowed Pod joins the
model's InferencePool, serves while the ordinary replica loads, and is handed
back the moment that replica is ready.

The pool is **insurance, not capacity**. It is sized by how often you spike and
how long a replica takes to start, never by peak load.

**It is not free.** Every pool Pod holds its accelerators continuously, whether
lending or idle, and those accelerators count against your namespace's quota
like any other workload. A pool of N Pods lowers your maximum fleet by N. That
is a cost decision, which is why WVA never creates a pool for you.

Two properties are worth knowing before you size one:

- **A pool Pod has to match the model's shape, in three ways.** The fit check
  declines per variant and says which one failed:

  | must match | declined with | set by |
  | --- | --- | --- |
  | GPUs per Pod — the Pod needs at least as many as the engine | `needs N GPUs, this Pod holds M` | `create --gpus` |
  | how those GPUs are **divided** — 16 over two Pods and over four are the same count and different engines | `spans N Pod(s), this warm unit is M` | `create --group-size` |
  | the accelerator itself — a warm copy is only reusable on the one it was loaded on | accelerator mismatch | `create --accelerator` (a nodeSelector) |

  A Pod cannot change node or device count, so a model that fails any of these
  will *never* be warmed by that pool — it is not a wait. Nothing refuses a
  mixed pool, and it does not fail loudly: the reserve is counted pool-wide, so
  part of it cannot serve the model you are holding it for.
- A pool whose replica count does not **exceed** its reserve can never warm
  anything at all. See [Sizing](sizing.md#sizing).

## Where the rest of this guide is

| | |
| --- | --- |
| [install.md](install.md) | Installing and creating a pool |
| [sizing.md](sizing.md) | Sizing and tuning a pool |
| [multi-pool.md](multi-pool.md) | Several pools, and what each holds |
| [groups.md](groups.md) | Pools of groups: engines that span machines |
| [operating.md](operating.md) | Operating a pool: scraping, checks, troubleshooting |

## Should this model be warmed here at all?

Before pools, one question: can this cluster warm this model, and would it gain
anything? Four thresholds decide it and all four move with the hardware, so it
reads the nodes rather than quoting one fleet's numbers.

```bash
deploy/warmpool.sh sizing --params 744B --dtype fp8
```

For a cluster you cannot reach -- choosing hardware rather than using it --
describe a node instead:

```bash
deploy/warmpool.sh sizing --params 744B --dtype fp8   --gpus-per-node 8 --gpu-mem-gib 141 --ram-gib 2016
```

It answers four questions:

| Question | Why it decides |
|---|---|
| Does the model fit one node? | If not, its engine spans Pods, and it needs a pool of **groups** rather than of Pods — see [pools of groups](groups.md#when-the-engine-spans-machines-pools-of-groups). |
| Can host RAM hold a level-1 sleeper at all? | Below this, warming is impossible here. |
| Can it hold **more than one**? | The whole question. A pool holding one model is an idle replica: same accelerators, no requests answered. |
| Is cold start dominated by reading weights, or by fixed startup? | Decides whether faster storage helps, or whether nothing does. |

Two results tend to surprise: **host RAM, not GPU memory, is what rules warming
out**, and a model split across MORE nodes is often more poolable, because the
per-node sleeper burden falls while total RAM rises.

## Which pools do you need?

Before creating anything, ask what the namespace actually wants. A pool serves
exactly one **(accelerator, GPUs-per-replica)** shape, because neither is
negotiable at run time: a warm copy is only reusable on the GPU it was loaded
on, and a model needing more devices than a Pod holds cannot start in one. Every
other difference between models — size, traffic, policy — a pool absorbs.

```bash
deploy/warmpool.sh plan -n <namespace>
```

It groups the namespace's model ScaledObjects by that pair, says which of them
could share one pool, and prints a `create` line for each group. It also names
models that select a pool nobody declared, which is worse than selecting none
because it reads as configured.
