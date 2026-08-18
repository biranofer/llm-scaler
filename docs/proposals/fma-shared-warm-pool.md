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

**R1 — Fungible warmth.** Warm reuse must not require the requester to be handed
the one GPU a sleeper happens to sit on. Without this every other guarantee is
built on scheduler luck.

**The original phrasing of R1 — "re-point `CUDA_VISIBLE_DEVICES` at bind time" —
is not achievable, and step 0 has now established that.** See
[Step 0 answered](#step-0-answered). R1 is restated above
as the property required, not the mechanism assumed.

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
   re-pointed without a weight reload?~~ **Answered: no.** See below. R1 is
   restated; the fork work changes shape before it starts, which is exactly what
   this step was for.
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

## Step 0 answered

**A sleeping vLLM cannot be re-pointed at a different GPU without restarting the
process.** Established by reading the fork, not by speculation:

| path | where the device is fixed |
| --- | --- |
| separate provider pod | `CUDA_VISIBLE_DEVICES` written into the **pod spec** container env (`pkg/controller/dual-pods/inference-server.go:1917`) |
| launcher-hosted instance | `config.env_vars["CUDA_VISIBLE_DEVICES"]` set from `gpu_uuids` **before** `multiprocessing.Process` spawns the server (`inference_server/launcher/launcher.py:187`) |

In both cases the device is chosen at process start. An env var cannot be changed
under a running process, and a CUDA context is bound to its device at
initialisation, so "re-point at bind time" would mean killing and respawning the
server — which is the very thing sleep mode exists to avoid. The GPU-UUID hash in
the instance ID is therefore not an arbitrary choice: **it is an accurate
statement of what the process can actually serve.**

This is the value of running step 0 first. Had R1 been implemented as written, the
work would have been wasted, and the failure would have surfaced late — as a
mysterious wake that still took 50s.

### What replaces it

The goal is unchanged; the mechanism must be. Two directions, and the second is
the realistic one:

**(a) Align the GPU the other way.** Choose the sleeper first, then arrange for the
requester to be given *that* GPU. This keeps the process untouched, which is the
ideal outcome — but the requester's GPU comes from the device plugin, and
influencing that assignment from a controller is a Kubernetes-level fight
(device-plugin semantics, scheduler extenders) far larger than the change we
wanted.

**(b) Make warm reuse depend on host-resident weights, not on a GPU-pinned
process.** Respawn the instance on whichever GPU the requester was actually given,
reusing weights already resident in the launcher's host memory or page cache. The
cost becomes a process start plus a host→device copy, instead of a download and a
full load. This is generic, contains no WVA concept, and is a clean upstream
story: *reuse the weights cache, not the process.*

Direction (b) also explains the measurement that started all of this. The 494s
and ~50s figures are download-and-load costs. A host-cached respawn is a different
quantity entirely — and **nobody has measured it yet**, which makes it the next
question, in the same spirit as this one:

> How long does starting a vLLM instance take on a launcher that has already
> served that model, with weights still in host cache, on a different GPU?

If that is seconds, direction (b) delivers the pool. If it is tens of seconds, the
shared pool is worth much less than assumed and this plan should be reconsidered
rather than pushed. **Measure before implementing** — the same discipline that
just saved the R1 work.

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
