# WVA-induced FMA warm pools

**Status:** proposal
**Requires FMA code changes:** none. One configuration prerequisite (§5).

## 1. Summary

FMA can make a replica ready in **3 seconds** instead of ~80 by waking a sleeping
vLLM rather than building one. Today that almost never happens, so FMA is slower
than no FMA and loses its own benchmark.

This proposes that WVA keep a *warm pool* — sleeping instances available to be
woken — between a **min** and **max** free count, purely by choosing how much
demand to place on FMA variants versus regular ones. No new actuation: WVA
already decides variant mix. The mechanism is a price on the FMA variant that
rises as free warmth is consumed.

Nothing in FMA changes. WVA never writes an FMA custom resource.

## 2. What was measured

On pokprod, `Qwen/Qwen3-0.6B`, one FMA variant, 2026-08-16:

| event | ready after |
|---|---|
| requester bound to a sleeper **created by an earlier requester** | **2s**, and **3s** on a repeat |
| requester bound to a sleeper **created by the launcher-populator** | 494s (rebuilt) |
| requester that had to build a new launcher | 49s / 68s / 150s |
| cold decode pod (no FMA) | 92s |

Both extremes are the same code path taking a different branch. The variance is
not noise: it is whether the instance was reusable.

## 3. Why the obvious approach does not work

The intuitive design — "pre-create sleeping instances for every mapped model" —
cannot work as stated. FMA reuses a sleeping instance only on an **exact
instance-ID match**, and that ID is a hash that **includes the GPU UUIDs**
(`pkg/controller/dual-pods`, present in v0.6.4 and on `main`). So a sleeper is
reusable only by a requester the scheduler happened to hand *that same GPU*.

Sleeping instances are therefore **not fungible**. The dual-pods controller says
as much when it gives up:

```text
Ensured vLLM instance absent to reclaim launcher capacity
Selected launcher Pod, binding first
Created vLLM instance          <- full model load
```

That is what happened to the 494s case: an idle, sleeping, correctly-labelled
launcher was destroyed and rebuilt because its instance was keyed to a different
GPU. Sleep mode itself is healthy — vLLM `/is_sleeping` agrees with the
`dual-pods.llm-d.ai/sleeping` label on every launcher we checked.

## 4. Why a WVA-induced pool sidesteps it

> **Sidesteps, not solves.** This design lives with non-fungibility by relying
> only on sleepers a variant produced itself, so it can never serve a spike in
> model B from warmth model A left behind. A pool genuinely *shared* across
> models needs the FMA change in
> [`fma-shared-warm-pool.md`](fma-shared-warm-pool.md). The two compose: the
> pricing law below works unchanged against a fungible pool and gets strictly
> better with one.

If the pool is populated **as a side-effect of WVA's own scale-downs**, every
sleeper in it was created by a requester that really did reserve that GPU — so
every sleeper is of the reusable kind. The unusable ones are exactly the
pre-created ones this design never relies on.

The warm pool is then not a provisioned resource but a **hysteresis buffer of
recently-freed allocations**: WVA scales an FMA variant down, the instances
sleep keyed to GPUs it just released, and a later scale-up on those nodes
reclaims them. Observed twice on the same launcher (`zd562`), 2s and 3s.

This is what makes zero FMA change possible. It is the whole idea.

## 5. Configuration prerequisite in FMA (config only, no code)

```text
launcherCount x |nodes(enhancedNodeSelector)|  >=  warmPool.max
```

`LauncherPopulationPolicy.spec.countForLauncher[].launcherCount` is **per node**
(the populator logs `node=... desired=N` per node key, despite the CRD
description saying "total"). It deletes anything above that count as an *excess
launcher pod*, warm instances included — measured, ~20s after a scale-down.

So durable warm capacity is `launcherCount x nodes`, and WVA's `max` must not
exceed it. Sleepers on on-demand launchers evaporate and must not be counted on.

Related, also configuration: `sleeperLimit` (dual-pods controller setting, not a
CRD field) bounds sleeping instances **per accelerator** with LRU eviction, and
therefore caps how many models can be simultaneously warm on one GPU.

## 6. Design

### 6.1 The signal

For a warm pool `P`, **free(P)** = launcher pods belonging to `P` whose instances
are *all* asleep, and whose pod is Running and Ready.

Read as a plain LIST of pods with `app.kubernetes.io/component=launcher` and
`dual-pods.llm-d.ai/sleeping=true`, once per decision interval. A LIST, not a
watch: this preserves WVA's call-driven, no-cluster-watch design.
`hack/benchmark/warm_pool.sh verify` already computes this predicate.

Sleeping-but-not-Ready is deliberately excluded. It is not warm capacity; it is a
pod that happens to be idle.

### 6.2 The control law

WVA keeps `free(P)` inside `[min, max]` by pricing the FMA variant:

```text
price_fma(free) :
    free  >  max   ->  well below the regular variant   # surplus warmth: spend it
    min..max       ->  monotone decreasing in free
    free  <  min   ->  above the regular variant        # protect the reserve
```

The existing optimizer then does the work with no new branch:

- warmth plentiful -> FMA is cheapest -> demand lands on FMA -> `free` falls
- warmth scarce    -> FMA priced above regular -> new demand goes to the regular
  variant -> FMA replicas drain as load falls -> instances sleep -> `free` rises

Cheap FMA consumes warmth; expensive FMA accumulates it. **The price is the
actuator.**

This is preferred over an explicit "if free <= min then scale up the other
variant" rule because it has no step change at the boundary, cannot oscillate
across it, and degrades gracefully: with no regular variant to shift to, the
price simply rises and nothing else happens.

### 6.3 What this is not

WVA does not create instances, choose GPUs, write FMA custom resources, or
manage launcher pods. It observes one number and adjusts one cost.

### 6.4 Scale-to-zero is this design's largest producer, and its sharpest edge

§4 rests on the pool being a side-effect of WVA's own scale-downs. **Parking is
the limiting case of a scale-down**: it releases every GPU a variant holds at
once, so a parked FMA variant produces sleepers keyed to all of them — the
largest single deposit into the pool that can occur. Waking it again is the 2s
path this proposal exists to reach, and scale-from-zero is where a 2s-versus-80s
difference is most visible to a user, because nothing is serving in the meantime.

So the two features compose in the intended direction. Three consequences follow,
and none of them is obvious from either design read alone:

**A parked variant's launchers must not be attributed to it.** They keep running
and keep answering `/metrics` with zeros — a sleeping launcher reports
`vllm:num_requests_running 0`, not nothing. Attributed, a parked model would read
as serving, and the scale-from-zero engine declines to wake a model whose decode
is already covered: parked, unwakeable, reported healthy. The pairing hop's
partner-must-resolve guard is what prevents it
(`fma-aware-attribution.md` §1), and `test/e2e/fma_parking_test.go` pins it.

**Warmth decays, so a wake budget derived from the 2s case is wrong for a
long-parked model.** A sleeper survives only while its launcher does, and the
populator reaps launchers it considers excess (upstream ask 2). A model parked
for minutes likely wakes in seconds; one parked overnight has had its sleepers
reclaimed and rebuilt, and wakes on the 494s path. This is the interaction to
measure before quoting any FMA wake SLO: **`retentionPeriod` chooses how long a
model waits before parking; nothing chooses how long its warmth survives
afterwards.** Until upstream ask 2 lands there is no knob for it.

**`free(P)` counts warmth that a parked variant can no longer use.** The signal
is per pool, but a sleeper is reusable only by a requester handed the same GPU
(§3). A parked variant contributes to `free(P)` while being, for its own next
wake, no more likely to reclaim its own sleepers than any other variant is. The
price therefore reads the pool as more useful than it is, in proportion to how
much of it belongs to parked variants. Bounded and self-correcting — the mistake
is in the direction of spending warmth, not hoarding it — but it should be
measured in phase 1 rather than assumed.

## 7. Configuration surface

Placed against the layering in `wva-keda-external-scaler.md` §7.5, whose rule is
that **budget-scope knobs live in the default/namespace layer, not in a reusable
policy**. Warm capacity is a shared, namespace-scoped resource — like GPU budget
— so its bounds belong there, not in a tier.

```text
ScaledObject metadata     per-target: which pool this target draws from, base cost
  ▼ overrides
named ScalingPolicy       per-tier: how much this tier values warmth (curve shape)
  ▼ overrides
namespace default CM      per-tenant: warmPool min/max            <-- the bounds
  ▼ overrides
global default CM         cluster: fallback bounds, defaultPolicy
```

### 7.1 ScaledObject metadata (per target)

```yaml
triggers:
  - type: external-push
    metadata:
      scalerAddress: wva-external-scaler.ns.svc.cluster.local:9090
      modelName: qwen-3-0.6b
      inferencePool: chat-pool        # EXISTING: routing/grouping (InferencePool)
      launcherPool: fma-qwen          # NEW: FMA LauncherConfig this target binds into
      variantCost: "4.0"              # EXISTING: base cost, before the warmth price
```

`launcherPool` is deliberately **not** called `pool`: `inferencePool` already
means the GIE InferencePool, which is routing and unrelated. The value is a
`LauncherConfig` name; the model->pool mapping otherwise already exists as
`InferenceServerConfig.spec.launcherConfigName` and can be derived instead of
declared, at the cost of a read.

Absent `launcherPool`, a target is simply not an FMA variant and is priced
normally. That keeps this entirely opt-in.

### 7.2 Named ScalingPolicy (per tier)

Tier-level *behaviour*, not capacity. An `interactive` tier should pay more to
keep warmth than a `batch` tier that does not care about a 90s ramp.

```yaml
warmth:
  enabled: true
  # Multiplier applied to variantCost as free(P) falls from max to min.
  # 1.0 at/above max, rising to priceAtMin at/below min.
  priceAtMin: 3.0
  curve: linear        # linear | quadratic
```

### 7.3 Namespace / global default ConfigMap (the bounds)

```yaml
warmPools:
  fma-qwen:
    min: 2        # reserve: never price warmth away below this
    max: 5        # ceiling: must be <= launcherCount x nodes (see §5)
```

`min` and `max` are counts of **free launcher pods**, which is the quantity
operators can see directly (`warm_pool.sh verify`) and the one the populator
bounds. They are *targets*, not guarantees (§9).

### 7.4 Why not on the ScalingPolicy

Two tiers sharing a pool would otherwise be able to declare conflicting bounds
for the same physical capacity, with no principled resolution. Capacity is a
property of the namespace and its nodes, not of a tier — the same reason
`enableLimiter` and GPU budget already live in that layer.

## 8. Observability

- `wva_warm_pool_free{pool}` — the observed signal.
- `wva_warm_pool_bounds{pool,bound}` — resolved min/max, since layered config is
  hard to debug without a "which value won" readout.
- `wva_variant_warmth_price{namespace,target}` — the applied multiplier, so a
  surprising variant mix can be explained.

Benchmarks already report **warm binds / cold builds / median replica start**
(`hack/benchmark/postprocess.py`), which is how a regression here becomes
visible instead of merely slow.

## 9. Limits, stated up front

- **Best-effort, not guaranteed.** A freed GPU is usually re-handed to the next
  requester on that node, which is why the same launcher woke twice. On a shared
  cluster another tenant can take it in between, stranding the sleeper. `min` is
  a target.
- **Pools are shared across models.** One model's sleepers can crowd out
  another's; `sleeperLimit` bounds sleepers per accelerator. If that matters,
  bounds must be per pool and the model->pool mapping becomes a fairness
  decision.
- **Cold models never self-warm.** A model gets a sleeper only after being
  served and scaled down once. There is no pre-warming without a real allocation.
- **Warmth is not durable.** Nothing keeps a sleeper alive for as long as its
  model stays parked, so a long-parked variant wakes cold however healthy the
  pool looked when it parked (§6.4).
- **Warmth costs GPUs.** A sleeping instance still occupies its accelerator's
  slot. `max` is a spending limit, not free headroom.

## 10. Phasing

1. Observe only: export `wva_warm_pool_free`, change no decisions. Confirms the
   signal is stable and that free capacity behaves as described.
2. Price it: apply the multiplier, bounds from the namespace layer.
3. Per-tier curves, once there is evidence tiers actually want different ones.

## 11. Upstream asks (independent of this design)

Worth filing regardless, because they would make warm pools *deliberate* rather
than emergent:

1. Reuse keyed on (model, options) with `CUDA_VISIBLE_DEVICES` re-pointed at
   bind time, instead of hashing GPU UUIDs into instance identity. Without this,
   a sleeper cannot be provisioned on purpose. Evidence: 2s wake versus 494s
   rebuild on an *available* sleeper.
2. `minLauncherCount` / `maxLauncherCount` on `LauncherPopulationPolicy`, so the
   populator stops reaping warm capacity as "excess".
3. Export free-launchers-per-pool as a metric, so consumers need no pod LIST.
4. Per-pool `sleeperLimit` rather than controller-global.

Two live bugs found while measuring, unrelated to this design but worth
reporting: the launcher's `state-change-reflector` runs as the namespace
`default` ServiceAccount and cannot patch its own pod (403, retried every 5s
forever); and the populator's ServiceAccount cannot watch nodes.
