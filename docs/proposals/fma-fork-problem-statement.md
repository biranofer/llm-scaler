# What the FMA fork must fix

**Status:** problem statement, for `ev-shindin/llm-d-fast-model-actuation`
(branch `feat/reuse-by-model`, currently empty)
**Audience:** whoever picks up the fork. Read this before writing code.

This exists because the evidence is spread across three documents and a
benchmark harness, and none of them states the defect in a form someone can act
on. Everything below was measured on pokprod; sources are cited so nothing has to
be taken on trust.

---

## The defect, in one sentence

**Scaling a requester up sometimes wakes a sleeping instance in 2–3 s and
sometimes builds a new one in 41–90 s, and nothing an operator or an autoscaler
controls decides which** — so actuation latency is unpredictable by an order of
magnitude, and the slow path silently destroys warm capacity on its way through.

## Why it happens

Three facts, each verified, that together make the outcome a coin toss:

1. **A sleeping instance is pinned to one GPU.** The instance ID is a hash
   including the GPU UUIDs, and `CUDA_VISIBLE_DEVICES` is fixed at process start
   — in the controller (`pkg/controller/dual-pods/inference-server.go:1917`) and
   in the launcher (`inference_server/launcher/launcher.py:187`). It cannot be
   re-pointed; see `fma-shared-warm-pool.md`.
2. **The requester's GPU is chosen by the device plugin, not by FMA.** Launcher
   pods request **0 GPU**, so the scheduler has no way to align a requester with
   the GPU a sleeper already holds. The two choices are made independently.
3. **Binding is node-local.** A requester binds only to a launcher on its own
   node, so a sleeper on another node cannot help however idle it is
   (`discovery-fma-warm-bind-is-node-local`, and the guide's "Warm pools are not
   what they look like").

A wake therefore happens only when the device plugin *happens* to hand the
requester the same GPU that a sleeper on that same node *happens* to hold.

## The part that makes it worse than a miss

On a mismatch FMA does not leave the sleeper alone and build elsewhere. It
**destroys it**:

```
Got GPU UUIDs                                              <- requester gets GPU-ceb79397
Ensured vLLM instance absent to reclaim launcher capacity  <- DELETES the sleeper
Selected launcher Pod, binding first
Created vLLM instance                                      <- full build
```

So a miss costs the slow path **and** removes the warm capacity that a later,
better-aligned requester would have hit. Warm capacity decays through use.

## The evidence

| observation | value | source |
| --- | --- | --- |
| bind to a sleeper made by an earlier requester | **2 s**, 3 s on repeat | `fma-warm-pool-wva.md` §2 |
| bind to a sleeper made by the launcher-populator | **494 s** — sleeper destroyed, rebuilt | ibid. |
| requester that had to build a launcher | 49 s / 68 s / 150 s | ibid. |
| bind to a launcher asleep for two days | **59 s**, against a 68 s cold build | `discovery-fma-warm-bind-is-node-local` |
| `make benchmark-actuation`, 6 trials | **median 90 s, 0 of 6 woken** | `docs/guides/fma/README.md` |
| cold start, ISC config, compile cache HIT | **~41 s** to engine ready | `fma-shared-warm-pool.md` |
| GPU memory held by a sleeping instance | **~1.4 GiB** | ibid. |
| instances permitted per launcher pod | `maxInstances: 4` | live `LauncherConfig` |
| sleeping instances permitted per GPU | `--sleeper-limit=2` | live controller args |

The two rows that matter most for the fix are the last three: **a sleeper is
cheap (1.4 GiB) and the launcher has room (4 instances, 2 sleepers per GPU).**
The reclaim is therefore not forced by capacity in the common case — it happens
anyway.

## What the fork should change

In priority order. The first is small, self-contained, and defensible upstream on
its own.

### Fix 1 — Do not destroy a sleeper to make room that already exists

**Problem:** on a GPU mismatch, the controller reclaims an existing sleeping
instance before creating the new one, even when `maxInstances` and
`--sleeper-limit` leave headroom.

**Change:** reclaim only when capacity is genuinely exhausted. If the launcher can
hold another instance, create the new one and leave the sleeper.

**Why it is worth doing first:** it converts a destructive miss into a harmless
one. Warm capacity then accumulates instead of decaying, which raises the hit
rate for every subsequent request without changing how binding works. It needs no
new API, no scheduler interaction, and no WVA concept.

**How to verify:** create an instance on a launcher that already holds a sleeper
for a different GPU, and assert the sleeper survives. The pattern is in
`fma-shared-warm-pool.md` §4 — two instances co-resident, `total=2 running=2`.

### Fix 2 — Let warm capacity be provisioned deliberately

**Problem:** sleepers created by the launcher-populator are keyed to GPUs no
requester will be handed, so they are never woken. Measured: one sat idle through
every test and the requester that bound to it still paid 494 s. Warm capacity is
therefore a by-product of real allocations and cannot be pre-created.

**Change options**, in increasing order of difficulty:

- **(a) Choose the sleeper first.** Have the binding pick a launcher that already
  holds a reusable instance, then arrange for the requester to receive that GPU.
  This is the real fix and the expensive one: the requester's GPU comes from the
  device plugin, so it needs the requester to stop requesting a GPU, or a
  scheduler-level mechanism.
- **(b) Make the populator create sleepers on GPUs that requesters actually
  receive**, rather than arbitrary ones — narrowing the mismatch rather than
  removing it.

### Fix 3 — Stop the populator reaping warm capacity

**Problem:** the populator deletes any launcher above `launcherCount` per node as
an "excess launcher pod", warm instance included, roughly 20 s after a
scale-down. Durable warm capacity is only `launcherCount × nodes`.

**Change:** `minLauncherCount` / `maxLauncherCount` on `LauncherPopulationPolicy`,
so warm capacity can be retained deliberately. This is upstream ask 2 of
`fma-warm-pool-wva.md`.

### Fix 4 — Make `--sleeper-limit` per pool, not controller-global

Upstream ask 4. Needed before a shared pool can be sized per tenant rather than
per cluster.

## What the fork should NOT change

- **Do not try to make the instance ID GPU-independent.** The GPU UUID in that
  hash is an accurate statement of what the process can serve; a sleeping vLLM
  cannot be moved to another device without restarting. Measured; see
  `fma-shared-warm-pool.md`.
- **Do not re-investigate sleep mode.** It works: vLLM `/is_sleeping` agrees with
  the `dual-pods.llm-d.ai/sleeping` label on every launcher checked, and a sleeper
  really does release its weights (1.4 GiB retained).
- **Do not add WVA concepts.** Which models deserve warm slots is an allocation
  decision and belongs in WVA. FMA should expose mechanism and state.

## Separate live bugs, worth reporting regardless

- The launcher's `state-change-reflector` runs as the namespace `default`
  ServiceAccount and cannot patch its own pod — `403`, retried every 5 s forever.
- The populator's ServiceAccount cannot watch nodes.
- `dual-pods.llm-d.ai/sleeping` disagrees with the launcher's own instance list
  (upstream ask 2 of `fma-upstream-requests.md`): a launcher labelled
  `sleeping=true` reported one running instance.

## How to reproduce any of this

```bash
# actuation latency, no load generator or Prometheus involved
make benchmark-actuation BENCHMARK_NAMESPACE=<ns> \
     ACTUATION_TARGET=<deployment> ACTUATION_TRIALS=5

# is the pool warm, and on which GPUs?
WARM_POOL_NS=<ns> bash hack/benchmark/warm_pool.sh report
WARM_POOL_NS=<ns> bash hack/benchmark/warm_pool.sh verify [n]
```

A run against a pool that is not warm silently measures the cold path, so gate on
`verify` before trusting any number.

## See also

- [`fma-shared-warm-pool.md`](fma-shared-warm-pool.md) — the measurements, and why
  the pool is an allocation problem for WVA rather than a fork problem
- [`fma-warm-pool-wva.md`](fma-warm-pool-wva.md) — the per-model pool that needs
  no FMA change at all
- [`fma-upstream-requests.md`](fma-upstream-requests.md) — six findings with
  measurements, for filing upstream
- [`../guides/fma/README.md`](../guides/fma/README.md) — operator-facing, including
  the warm-pool gate
