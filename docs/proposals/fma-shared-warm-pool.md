# Plan: a shared, reusable FMA warm pool

**Status:** plan
**Requires FMA code changes:** yes — this is the entry that does.
**Fork:** `ev-shindin/llm-d-fast-model-actuation`, branch `feat/reuse-by-model`
(created, no commits yet).

---

## What this is, and why it is a separate document

Three FMA documents already exist in this repo and none of them proposes changing
FMA:

| document | what it does | FMA changes |
| --- | --- | --- |
| `fma-aware-attribution.md` | make WVA *measure* an FMA variant | none |
| `fma-warm-pool-wva.md` | keep a warm pool by *pricing* FMA demand | none — by design |
| `fma-upstream-requests.md` | six findings to file upstream | asks, not a plan |

This one states the goal those three work around: **one warm pool of launcher
capacity, shared across models, that absorbs spikes and makes scale-from-zero
fast.** That requires a change inside FMA, so it needs its own plan, a fork, and
an upstream path.

## The blocker, stated exactly

Sleeping instances are **not fungible**. FMA reuses a sleeping instance only on an
exact instance-ID match, and that ID is a hash that **includes the GPU UUIDs**
(`pkg/controller/dual-pods`, present in v0.6.4 and on `main`).

A requester is assigned its GPU by the device plugin. Launcher pods request **0
GPU**, so the scheduler cannot align the two. On a mismatch FMA does not fall
back — it destroys the sleeper and builds a fresh instance:

```
Got GPU UUIDs                                              <- requester gets GPU-ceb79397
Ensured vLLM instance absent to reclaim launcher capacity  <- DELETES the sleeper
Selected launcher Pod, binding first
Created vLLM instance                                      <- fresh ~50s load
```

Measured on pokprod (2026-08-16): a genuine bind to a launcher that had been
asleep since 2026-08-14 took **59s**, against a cold build of 68s. The warm pool
cost a full GPU per slot and returned almost nothing.

Two consequences worth being blunt about:

- **A pool shared across models is not merely unimplemented, it is unreachable.**
  Warmth cannot be pooled when each sleeper is bound to one GPU that one
  scheduler decision has to reproduce.
- **`fma-warm-pool-wva.md` is a workaround for this bug, not a solution to it.**
  Its §4 restricts the design to sleepers a variant produced itself, precisely
  because those are the only reusable ones. It is correct and it is worth
  shipping — but it can never serve a spike in model B from warmth model A left
  behind, which is the thing actually wanted.

Warm binding is also **node-local**: a requester binds only to a launcher on its
own node. Pinning launchers without pinning requesters turns every scale-up into
a cold load. That is a configuration trap, already documented, and it does not go
away by itself — a fungible pool still has to be reachable from where requesters
land.

## Goal

One pool of launcher capacity per accelerator type, drawn on by every model in
the namespace:

- A spike in any model can be served from any free warm slot, subject to the
  model's weights being loadable there.
- **Scale-to/from-zero is a first-class user of the pool, not an edge case.** A
  parked model is the cheapest possible tenant of warm capacity: it holds no
  requester, no GPU through a requester, and no replicas, yet it can return to
  service in seconds. That combination is only available with FMA, and it is the
  strongest argument for the pool paying for itself.

## The change in FMA

Upstream ask 1 of `fma-warm-pool-wva.md` §11, stated as the work item:

> Key instance reuse on **(model, options)** rather than on a hash that includes
> GPU UUIDs, and re-point `CUDA_VISIBLE_DEVICES` at bind time.

Sleepers then become interchangeable within a model across GPUs on reachable
nodes, which is the property the pool needs. It is also the change that makes
FMA's own measured best case — a 2s bind — the normal case rather than an
accident of the scheduler handing back the same GPU.

Open design questions for the fork, to be answered in code rather than here:

- **Does re-pointing suffice at the vLLM level?** A sleeping vLLM has weights
  offloaded; whether the device can be swapped under it without a reload is the
  crux, and it decides whether this is a small change or a large one. **Settle
  this first** — the rest of the plan is contingent on it.
- **Is the instance-ID hash load-bearing elsewhere?** It appears in labels,
  annotations and the launcher API. Changing what it covers may ripple.
- **What happens to a sleeper whose GPU was taken by another tenant?** Today
  reclamation is implicit in the mismatch path; with reuse decoupled from the
  GPU, eviction becomes an explicit policy.

## Sequence

1. **Answer the vLLM question** (fork spike, no PR). If the device cannot be
   re-pointed under a sleeping instance, this plan changes shape and the rest is
   moot — so nothing else starts until it is answered.
2. **Implement reuse-by-model** on `feat/reuse-by-model`.
3. **Test with WVA** on the kind emulator first, then pokprod. The measurement
   that matters is the one already taken, repeated: bind to a sleeper created by
   a *different* requester, on a *different* GPU. Success is seconds, not ~50s.
   `hack/benchmark/warm_pool.sh verify` already computes the free-pool predicate.
4. **Land the WVA side** — `fma-warm-pool-wva.md`'s pricing law, which needs no
   change to work against a fungible pool and gets strictly better with one.
5. **PR upstream**, with the before/after numbers from step 3. The case is
   already written up in `fma-upstream-requests.md`; a PR carrying a measured
   2s-vs-59s comparison is a different proposition from an issue describing one.

Steps 1–2 are FMA work in the fork. Step 4 is WVA work and is **not blocked** by
any of it.

## What WVA must get right regardless

These hold whether or not the fork change lands, and two are already covered:

- **A parked variant's launchers must not be attributed to it** — otherwise a
  parked model reads as serving and scale-from-zero refuses to wake it. Guarded
  by the pairing hop; pinned by `test/e2e/fma_parking_test.go`.
- **Warm launchers must survive parking.** Same suite asserts it, because a fix
  for the point above that deleted the launchers would satisfy it and destroy
  the pool.
- **The pool's GPUs must become visible to the API server.** Unresolved, and the
  highest-severity item in `fma-upstream-requests.md`: nine launchers held nine
  GPUs while the namespace was charged for one. A shared pool makes this worse,
  not better — it is exactly the configuration where nobody owns the GPUs.

## Status of the fork

`feat/reuse-by-model` exists on `ev-shindin/llm-d-fast-model-actuation` with **no
commits**. Nothing has been changed, tested, or filed. This document exists so
that fact is recorded rather than rediscovered.

## See also

- [`fma-warm-pool-wva.md`](fma-warm-pool-wva.md) — the no-FMA-change pricing
  design, and §6.4 on how scale-to-zero feeds and drains the pool
- [`fma-upstream-requests.md`](fma-upstream-requests.md) — the six findings, with
  measurements
- [`fma-aware-attribution.md`](fma-aware-attribution.md) — how WVA measures an
  FMA variant at all
