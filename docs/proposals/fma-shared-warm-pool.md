# Plan: a reliable, shared FMA warm pool, with the policy in WVA

**Status:** plan
**Requires FMA code changes:** yes — this is the entry that does.
**Fork:** `ev-shindin/llm-d-fast-model-actuation`, branch `feat/reuse-by-model`
(created, no commits yet).
**Upstream posture:** work in the fork, no PR, until the WVA/FMA integration
produces results worth carrying.

---

## What this is, and why it is a separate document

Three FMA documents already exist here and none proposes changing FMA:

| document | what it does | FMA changes |
| --- | --- | --- |
| `fma-aware-attribution.md` | make WVA *measure* an FMA variant | none |
| `fma-warm-pool-wva.md` | keep a warm pool by *pricing* FMA demand | none — by design |
| `fma-upstream-requests.md` | six findings to file upstream | asks, not a plan |

This one states the goal those three work around: **one pool of launcher
capacity, shared across models, that absorbs spikes and makes scale-from-zero
fast — and that is reliable enough to plan against.** That needs a change inside
FMA, so it needs its own plan, a fork, and an upstream path.

## Terminology

Upstream's terms, used here so the eventual PR does not have to be translated.
Where this document has been using WVA-local shorthand, the mapping is:

| upstream (`docs/dual-pods.md`, `docs/launcher.md`) | shorthand used here |
| --- | --- |
| server-requesting Pod (of a Deployment) | **requester** |
| nominal server-providing Pod (of a `LauncherConfig`) | **launcher** / provider |
| vLLM **instance** — a subprocess inside a launcher | instance |
| unbound provider (asleep by definition) | **sleeper** |
| `LauncherPopulationPolicy` | the populator |
| `InferenceServerConfig` (ISC) | the ISC — source of the serving labels |

Two upstream invariants this plan must not contradict:

- **Bound ⇔ awake.** "An unbound server-providing Pod is asleep. The transitions
  happen while bound: wake up ASAP after binding, go to sleep just before
  unbinding."
- **The provider joins the InferencePool, not the requester** — serving labels
  come from the ISC's `modelServerConfig.labels`, applied at bind time. This is
  what makes a bound launcher an EPP endpoint and a sleeper invisible to routing.

## Why today's pool is not reliable

Not "not implemented" — **not reachable**. Sleeping instances are not fungible:
the instance ID is a hash that includes the **GPU UUIDs**
(`pkg/controller/dual-pods`, v0.6.4 and `main`). A requester is assigned its GPU
by the device plugin; launcher pods request **0 GPU**, so the scheduler cannot
align the two. On a mismatch FMA does not fall back — it destroys the sleeper:

```
Got GPU UUIDs                                              <- requester gets GPU-ceb79397
Ensured vLLM instance absent to reclaim launcher capacity  <- DELETES the sleeper
Selected launcher Pod, binding first
Created vLLM instance                                      <- fresh ~50s load
```

Measured on pokprod: a bind to a launcher asleep since 2026-08-14 took **59s**
against a **68s** cold build. Warm binding is additionally **node-local** — a
requester binds only to a launcher on its own node — so pinning launchers without
pinning requesters makes every scale-up cold.

Reliability, stated as the property that is missing: **a warm hit is currently an
accident of scheduling.** Nothing can predict one, so nothing can plan on one, so
the pool cannot be sized, priced or promised.

## WVA is the brain, FMA is the muscle

That is the whole architecture in a sentence, and it decides where each change
belongs.

**FMA is the muscle: it should own mechanism.** Making sleepers reusable and pool
state observable is generic capability — it helps every consumer, contains no WVA
concept, and is defensible upstream on its own merits. A change that encoded our
allocation policy inside FMA would be worse engineering and much harder to land.

**WVA is the brain: it should own allocation.** Deciding *which* models hold warm
slots, *how many*, and *when to spill demand onto a non-FMA variant* is a
cross-model allocation problem stated in tokens of supply and demand. WVA already
computes exactly that, per replica, which is why it needed the attribution work in
the first place.

The two fit because they are different kinds of work, not because either is
withheld. FMA moves fast; WVA decides what is worth moving.

**This is not about limiting who else can use FMA.** A better FMA is good for
every consumer, and the upstream changes here are deliberately generic so that a
KEDA scenario, a Grafana dashboard or an operator benefits too. The claim is
narrower and more durable: **WVA can use FMA more efficiently than a
threshold-driven autoscaler can**, because efficiency here means allocating a
scarce shared resource across models, and that needs a model of supply and demand
rather than a threshold to chase.

`fma-aware-attribution.md` §9 shows why that difference is structural rather than
asserted. The working KEDA-on-FMA scenario scales the requester Deployment on
**EPP pool saturation** — a signal measured at the pool, which never reads a
per-replica engine metric. That is what makes it robust against FMA's attribution
problems, and it is the same property that leaves it with nothing to say about
which of several models should hold the last warm slot. Both loops are legitimate;
they answer different questions.

So the honest summary:

> Upstream gets a pool that works, for everyone. WVA gets a pool that is
> *allocated*, because it is the component that can size warmth in tokens.

## What reliability requires

Four properties. The first two need FMA; the last two are WVA's and are not
blocked.

**R1 — Warm coverage, not fungible warmth.** A single sleeper cannot be made
fungible (measured, below). The property that matters is instead that a requester
LANDS on a GPU holding a sleeper for its model — achieved by keeping several
sleepers co-resident rather than by re-pointing one. `--sleeper-limit` is the
knob; ~1.4 GiB per sleeper is the price. See
[Measured on pokprod](#measured-on-pokprod-2026-08-18).

**The original phrasing of R1 — "re-point `CUDA_VISIBLE_DEVICES` at bind time" —
is not achievable**, and the goal it was reaching for is met a different way: by
co-resident sleepers rather than a movable one.

**R2 — Pool state that can be read before deciding.** Free sleepers per (model,
accelerator, and the node set a requester could actually land on), exported by
FMA. WVA can approximate this today with a pod LIST — `free(P)` in
`fma-warm-pool-wva.md` §6.1 — but a LIST cannot see *reachability*, and
node-locality means an unreachable sleeper is not free capacity. This is upstream
ask 3 of that document, promoted here from nice-to-have to a reliability
requirement.

**R3 — A wake is never worse than no-FMA.** The decision must not *depend* on a
warm hit. If the pool misses, the variant takes the ordinary cold path and WVA's
sizing must already have assumed it could. A design that is fast when warm and
wrong when cold is not reliable; it is lucky.

**R4 — Every wake is classified.** Warm hit or cold build, with the reason, as a
metric. "Reliable" is a measurable claim or it is a hope — and the 59s-vs-68s
measurement above only exists because someone looked. Without R4 a regression
in R1 is invisible: the system keeps working, just slowly.

R3 and R4 are WVA-side and should land **first**, because they are what make the
FMA work measurable when it arrives.

## Sequence

Deliberately ordered so that nothing is filed upstream before there is a result
worth filing.

0. ~~**Answer the contingent question**: can a sleeping vLLM have its device
   re-pointed without a weight reload?~~ **Answered: no** — and three further
   rounds of measurement followed, which between them replaced the fork plan with
   a configuration-and-policy plan. See
   [Measured on pokprod](#measured-on-pokprod-2026-08-18).
1. **WVA: R4, then R3.** Classify every FMA wake and stop assuming warm. No FMA
   dependency, useful immediately, and it establishes the baseline the later
   comparison needs.
2. **Fork: R1** on `feat/reuse-by-model`. Kept minimal and generic — no WVA
   concept enters FMA.
3. **Test the pair on WVA.** Kind emulator for mechanics, pokprod for the number
   that matters: bind to a sleeper created by a *different* requester on a
   *different* GPU. Success is seconds, not ~50s. `hack/benchmark/warm_pool.sh
   verify` already computes the free-pool predicate.
4. **Fork: R2**, once step 3 shows R1 works and the LIST approximation is the
   thing limiting us.
5. **WVA: the allocation policy** — `fma-warm-pool-wva.md`'s pricing law, which
   works unchanged against a fungible pool and gets strictly better with one,
   plus cross-model allocation and the spill-to-non-FMA rule.
6. **Only then, upstream.** R1 and R2 as separate PRs, each defensible alone,
   each carrying the before/after measurement from step 3. An issue describing a
   59s warm bind is a report; a PR carrying 59s → seconds is a case.

Steps 1 and 5 are WVA work and are **not blocked** by any of the fork work.

## Measured on pokprod, 2026-08-18

Three rounds of measurement, each of which changed the plan. Kept in full because
two of them corrected an earlier conclusion in this same document.

### 1. A sleeping instance cannot change GPU (step 0) — stands

`CUDA_VISIBLE_DEVICES` is fixed at process start, in both spawn paths: the
controller writes it into the pod-spec container env
(`pkg/controller/dual-pods/inference-server.go:1917`), and the launcher writes it
into `config.env_vars` before `multiprocessing.Process`
(`inference_server/launcher/launcher.py:187`). An env var cannot change under a
running process and a CUDA context binds at initialisation.

**So the GPU-UUID hash in the instance ID is accurate, not arbitrary.** A single
sleeper is not fungible, and no amount of re-keying makes it so. This conclusion
survived everything below.

### 2. Storage is not the cost — so "reuse host-resident weights" is not the fix

`/model-cache` is **`model-pvc`, ReadWriteMany, IBM Spectrum Scale**, mounted by
every launcher on every node, so there is no per-launcher download at all.
Reading the whole 1.5 GB `model.safetensors`:

| launcher | resident instances | pass 1 | pass 2 |
| --- | --- | --- | --- |
| pfw8b | 0 | 2753 ms | 2800 ms |
| hhj4j | 1 | 3484 ms | 2922 ms |
| zhs9q | 0 | 2771 ms | 2902 ms |
| zhwmp | 1 | 2807 ms | 2759 ms |

~2.8 s, identical whether the launcher has served the model or not. Weight I/O is
a small and already-shared part of a cold start.

### 3. What a cold start is actually made of, and what a sleeper actually costs

Created a second instance on an idle GPU of an otherwise-quiet node, timed it,
then deleted it. From its own log:

```
init engine (profile, create kv cache, warmup model) took 18.75 s (compilation: 12.35 s)
torch.compile took 12.35 s in total
Graph capturing finished in 3 secs, took 0.43 GiB
```

| quantity | measured |
| --- | --- |
| engine init, end to end | **~19 s** (not the ~50 s assumed) |
| of which `torch.compile` | **12.35 s** |
| of which CUDA graph capture | 3 s |
| weight read from the PVC | ~2.8 s |
| **GPU memory held by a SLEEPING instance** | **~1.4 GiB** |
| GPU memory held while awake (`--gpu-memory-utilization 0.30`) | 25 GiB |

Two corrections to earlier drafts of this document fall out:

- **A sleeper does not hold its GPU.** An earlier reading of `nvidia-smi` totals
  suggested it held ~81 GiB; that was other tenants' memory on a shared cluster.
  The container sees only its own PID: **1386 MiB**. `is_sleeping` returns true
  and the weights really are offloaded.
- **The cold start is dominated by compilation, not by loading.** `torch.compile`
  alone is 12.35 s of ~19 s. Note `/model-cache/vllm` (32 MB) and
  `/model-cache/triton` (19 MB) are compile caches **already on the shared PVC**,
  so this figure is with a warm compile cache — which is likely why earlier
  measurements of a fresh model saw ~50 s and this saw ~19 s.

### 4. Multiple instances per launcher: confirmed, and it is the answer

The cluster already runs `--sleeper-limit=2` (sleeping servers permitted **per
GPU**) and `maxInstances: 4` (instances per launcher pod). Creating a second
instance alongside an existing sleeper **succeeded**, and both coexisted:

```
total=2 running=2
  IsOqTGFHyahCVv running ['GPU-fc3875f6-...']   <- the pre-existing sleeper
  efcf6278-7f55- running ['GPU-7af4810a-...']   <- created for this test
```

## What this means for a pool serving several models

**It is achievable today, by configuration, and needs no FMA code change.** The
error in the earlier draft was attacking the wrong mechanism: the goal is not to
make one sleeper serve many models, but to keep **several sleepers, one per
model, co-resident** — so whichever model spikes finds its own warm instance
already there.

The economics now have numbers:

- A sleeper costs **~1.4 GiB** of GPU memory. On an 80 GiB accelerator, memory is
  nowhere near the binding constraint.
- The binding constraint is **`--sleeper-limit`**, a controller flag, default 1,
  set to 2 here — and it is **controller-global rather than per pool**, which is
  upstream ask 4 of `fma-warm-pool-wva.md`.
- What a warm hit saves is **~19 s of engine init**, most of it compilation — not
  weight loading, which is shared and cheap.
- Only one instance per GPU can be *awake* at a realistic
  `gpu-memory-utilization`, so a GPU covers N models warm and serves one at a
  time.

The GPU-alignment constraint from step 0 does not go away, but it stops being
fatal: if a GPU holds sleepers for N models instead of 1, a requester landing
there hits warm for any of those N. **Raising `sleeper-limit` raises the hit
rate directly**, and its cost is 1.4 GiB per model per GPU.

That turns the problem into exactly the one WVA is built for: **a budget of
sleeper slots per GPU, and more models than slots.** Which models hold warm slots
on which GPUs, and which sleeper is evicted when another model needs one, is a
cross-model allocation question in tokens of demand. A pool-saturation threshold
cannot express it; WVA's per-replica sizing can.

## Revised recommendation

**Do not fork FMA.** Not because the pool is unreachable — it is reachable — but
because the mechanism is a flag and an allocation policy, both of which sit
outside FMA's code.

1. **WVA-side, unblocked:** treat sleeper slots as an allocatable resource. Read
   the pool (`component=launcher` + `sleeping=true`), decide which models hold
   slots, and price FMA demand as `fma-warm-pool-wva.md` §6.2 already describes.
2. **Configuration, no code:** raise `--sleeper-limit` to the number of models a
   GPU should cover. Measure the hit rate against it.
3. **Upstream, small and generic:** per-pool rather than controller-global
   `sleeper-limit` (ask 4), and pool state as a metric (ask 3). Both are
   defensible alone and neither encodes a WVA concept.

### Still to measure

- **Do two sleepers for DIFFERENT models coexist on ONE GPU?** Only one model
  (`Qwen3-0.6B`) is on this PVC, so the co-residency test above used two GPUs.
  `sleeper-limit=2` is per GPU and permits it, but it has not been demonstrated.
- **Hit rate as a function of `sleeper-limit`**, which is the number that decides
  whether this is worth operating.
- **Cold start with a COLD compile cache**, to size what the shared
  `/model-cache/vllm` is already saving.

## Scale-to/from-zero is a first-class tenant

Not an edge case, and worth stating because it is the strongest argument the pool
has. A parked model holds no requester, no replicas, and no GPU through a
requester, yet with warm capacity it returns to service in seconds. That
combination does not exist without FMA.

Three things must hold, and two are already covered:

- **A parked variant's launchers must not be attributed to it** — otherwise the
  model reads as *serving*, and scale-from-zero declines to wake a model whose
  decode is already covered: parked, unwakeable, reported healthy. Guarded by the
  pairing hop.
- **Warm launchers must survive parking.** A fix for the point above that deleted
  them would satisfy it and destroy the pool. `test/e2e/fma_parking_test.go`
  asserts both together for that reason.
- **Warmth decays while parked, and nothing bounds the decay.** The populator
  reaps launchers it considers excess, so a model parked for minutes wakes warm
  and one parked overnight wakes cold. `retentionPeriod` chooses when a model
  parks; nothing chooses how long its warmth survives. Until upstream ask 2
  (`minLauncherCount`) lands there is no knob — so **no FMA wake SLO should be
  quoted for a long-parked model**, and R4 is what will show the difference.

## Known gap found while testing this

`resolveTarget` caches the pod→scale-target chain **indefinitely**
(`internal/collector/locator/locator.go:124`). If a launcher keeps a stale
`dual` label naming a deleted requester, the cached chain keeps resolving and the
launcher stays attributed — the parked-reads-as-serving failure above, made
permanent for that pod name rather than brief.

In normal operation FMA clears the label on unbind, so the common path is safe
and this is not a live outage. It is the crashed-dual-pods-controller case, and
it is worth either invalidating on partner `NotFound` or bounding the chain
cache. Recorded here rather than fixed, because it is independent of this plan.

## Status of the fork

`feat/reuse-by-model` exists with **no commits**. Nothing implemented, tested or
filed. Recorded so it is not rediscovered a third time.

## See also

- [`fma-warm-pool-wva.md`](fma-warm-pool-wva.md) — the no-FMA-change pricing
  design, and §6.4 on how scale-to-zero feeds and drains the pool
- [`fma-upstream-requests.md`](fma-upstream-requests.md) — the six findings, with
  measurements
- [`fma-aware-attribution.md`](fma-aware-attribution.md) — how WVA measures an
  FMA variant at all, and §9 on why the KEDA path is immune to all of it
