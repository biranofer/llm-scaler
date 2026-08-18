# What the FMA fork must fix

**Status:** problem statement, for `ev-shindin/llm-d-fast-model-actuation`
(branch `feat/reuse-by-model`)
**Audience:** whoever picks up the fork. Read this before writing code.

> **Updated 2026-08-18 — read this first.**
>
> - **Fix 1 is implemented and validated on cluster** (`aa072ef`, image
>   `ghcr.io/ev-shindin/dual-pods-controller:aa072ef`). Its root cause was NOT
>   what this document assumed: capacity is already checked, but a **port
>   conflict bypasses that check**, because the port comes from the ISC and is
>   therefore identical for the same model on a different GPU. Deployed on
>   pokprod: **0 destroy events, 2 declines, and the first observed
>   `warmAffinity` wake at 2 s**.
> - **Fix 1.5 is done and was measured insufficient.** Placement now works —
>   3/3 replicas landed beside sleepers, twice — and **still 0 woke**. Landing on
>   the right node is necessary and nowhere near sufficient: the reuse key is the
>   GPU UUID and a pool node has 7-8 of them. See
>   `../guides/fma/README.md#node-locality-is-necessary-and-nowhere-near-sufficient`.
> - **Fix 2 (deliberate provisioning) is bounded by arithmetic, not by FMA.**
>   Reliable wakes require covering *every free GPU on every node a requester can
>   reach*, and the free set belongs to other tenants.
> - Consequently the strategic direction has moved to
>   [`fma-launcher-owned-warm-pool.md`](fma-launcher-owned-warm-pool.md), which
>   removes the per-scale scheduling event rather than trying to win it. The
>   fixes below remain correct and worth having for the current architecture.

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

## Measured 2026-08-18: 0 woke, 3 rebuilt — with the pool VERIFIED warm

The decisive run, and it moves the root cause. Requesters spread one per node,
pool warmed by scaling up and back, gate passed (`warm_pool.sh verify` rc=0, 3
sleepers). Then 2 → 5:

```
2bjf2  pokprod-b93r43s3   64s  rebuilt
4sf2r  pokprod-b93r39s2   64s  rebuilt
rmwgm  pokprod-b93r43s1   95s  rebuilt
--> 0 woke, 3 rebuilt
```

**Two of the three landed on `b93r43*` nodes, which host no launcher at all.**
The launcher pool lives on `b93r38*`/`b93r39*`. Binding is node-local, so a wake
was impossible before GPU alignment was ever reached.

### Fix 1.5 CONFIRMED: 3 woke, 0 rebuilt — 2 s, 3 s, 3 s

Constraining the requester Deployment to the same five nodes the
`LauncherPopulationPolicy` pins launchers to, and changing nothing else:

```
9wcfg  pokprod-b93r39s1   2s  WOKE
jslr5  pokprod-b93r38s2   3s  WOKE
mcl96  pokprod-b93r39s0   3s  WOKE
--> 3 woke, 0 rebuilt
```

The constraint was verified applied before measuring
(`key=kubernetes.io/hostname op=In n_values=5`, spread present) — the previous
attempt's patch was malformed and silently measured the unconstrained case.

| run | requester constraint | result |
| --- | --- | --- |
| baseline | none | 0 woke / 3 rebuilt — 64 s, 64 s, 95 s |
| accidental | none (patch failed) | 1 woke / 2 rebuilt — 2 s vs 58 s |
| **Fix 1.5** | **pinned to the launcher node set** | **3 woke / 0 rebuilt — 2 s, 3 s, 3 s** |

**~90 s → ~3 s, by configuration, with no FMA code change.** This is the single
highest-yield change available and it should be applied before any fork work is
considered. The remaining fixes address what happens once placement is right —
they are no longer the first-order problem.

What this does NOT prove: that the constraint is safe at scale. Pinning
requesters to the launcher node set caps the model at `launcherCount × nodes`
replicas and concentrates load on those nodes. Sizing that pool becomes the
operating question, and it is an allocation decision — which is where WVA comes
in (see `fma-shared-warm-pool.md`).

### The natural experiment that proves it

A follow-up run intended to test Fix 1.5 **failed to apply its node constraint**
(`error decoding patch: unexpected end of JSON input`), so the requesters were
placed by the scheduler as usual. The result is more convincing than the intended
test would have been, because nothing was arranged:

```
j595g  pokprod-b93r39s1   2s   WOKE      <- a node that HAS a launcher
bfzhq  pokprod-b93r43s1   58s  rebuilt   <- no launcher on this node
zpw6x  pokprod-b93r43s3   58s  rebuilt   <- no launcher on this node
--> 1 woke, 2 rebuilt
```

**The single requester that happened to land on a launcher node woke in 2
seconds.** The two that landed elsewhere rebuilt in 58 s. Same cluster, same
pool, same moment — the only variable was the node.

Two things follow. Warm binding **works**, and is worth ~56 s when it happens; and
the reason it usually does not happen is placement, not the binding logic. A run
that constrains the requester to the launcher node set has still not been done —
the patch above was malformed — so **Fix 1.5 remains untested, but its premise is
now measured, and the fix itself is confirmed above.**

**So node placement fails first, and GPU alignment is the second-order problem.**
The `LauncherPopulationPolicy` pins launchers to a subset of nodes, while the
requester Deployment carries only `nvidia.com/gpu.product Exists` — no
nodeSelector, no affinity toward where warm capacity actually is. The scheduler
places requesters with **no knowledge of where sleepers live**, across a much
larger node set than the pool occupies.

This also shows the earlier "constrain both halves" mitigation is **not currently
in place** on this cluster: the requester Deployment was checked and has no node
constraint. Any measurement taken in this state is measuring the cold path by
construction.

## Why it happens

Four facts, each verified. The first is the one that fires first:

0. **The requester's NODE is chosen without reference to warm capacity.** The
   populator decides where launchers live; the scheduler decides where requesters
   live; nothing connects them. Measured above: 2 of 3 requesters landed on nodes
   with no launcher.

The remaining three then decide the outcome among the requesters that do land
somewhere useful:

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

### Fix 1.5 — Make requesters land where the warm capacity is

**Problem:** the highest-yield gap, and the cheapest to close. Launchers are
pinned to a subset of nodes; requesters are unconstrained. Measured: 0 of 3 woke
because 2 landed on nodes with no launcher.

**Change, in increasing order of ambition:**

- **(config, today, no code)** constrain the requester Deployment to the same node
  set as the `LauncherPopulationPolicy`. This is a nodeSelector/affinity edit and
  it is what upstream's own `ocp-wva-fma-hotstart` scenario does. It should be
  verified before any fork work, because it may recover most of the wake rate on
  its own.
- **(FMA)** have the dual-pods controller express warm capacity as scheduling
  input — node affinity injected onto the requester, or a scheduler hint — so
  placement follows sleepers rather than ignoring them.

**Why it comes before the GPU-alignment work:** a requester on a node with no
launcher cannot wake anything however well the GPUs line up. Fixing alignment
while placement is unconstrained fixes the second problem and leaves the first.

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

## Two defects that were already found and FIXED — do not re-file them

Both were diagnosed in earlier sessions and are repaired by
`make benchmark-fma-fixups`. Verified on pokprod 2026-08-18 as **no longer
present**: controller image `v0.6.4`, launcher ServiceAccount *can* patch pods,
zero 403s in the reflector, and no unbound launcher carrying a stale serving
label. They are recorded because the standup reintroduces them, and because
together they produce **no** numbers rather than bad ones:

```
launcher SA cannot patch pods (403, retried every 5s forever)
  -> a launcher whose requester was deleted keeps llm-d.ai/inferenceServing=true
  -> that dead endpoint stays in the InferencePool
  -> EPP dispatches to it; ~20% of requests return 503
  -> guidellm validates its backend once, dies, and no results.json is written
  -> every metric in the table reads "?"
```

1. **Launcher RBAC.** The `state-change-reflector` sidecar patches labels onto its
   own pod but is given the namespace `default` ServiceAccount. Check with
   `kubectl auth can-i patch pods --as=system:serviceaccount:<ns>:default -n <ns>`.
2. **Controller version — upstream #696.** `v0.6.0-alpha.13` drops a fresh
   reconcile notification when the item is already queued with a future
   `processAfter` from a rate-limited retry. Defect 1's 403s generate exactly
   those retries, so the unbind that would clear the stale label is swallowed.
   `FMA_VERSION` defaults to `v0.6.4`, which is fixed.

**So neither is the remaining problem.** What is left is the GPU-alignment
non-determinism above.

## How to measure a wake — the method matters, and the obvious way does not work

`kubectl scale` on the requester **does nothing**: the deployment is owned by a
KEDA ScaledObject (`<requester>-wva`, `external-push`, min 1 max 5) and the
derived HPA restores the replica count within seconds. An attempt on 2026-08-18
sat at `readyReplicas=1` for 4.6 minutes with no second pod ever created, and the
cause was this, not FMA.

Two further conditions must hold or a scale-up cannot wake anything, whatever the
code does:

- **A sleeper must exist on the node the requester lands on** — binding is
  node-local.
- **That sleeper must be keyed to the GPU the requester was allocated** — the
  instance ID hashes the GPU UUIDs.

The pool is `launcherCount × nodes`, so requesters must spread one per node or the
surplus uses on-demand launchers the populator then reaps as excess. Left alone,
3 of 4 requesters once packed onto a single node.

The working sequence, from `scratchpad/spread_and_warm.sh`:

1. Patch `topologySpreadConstraints` on the requester Deployment
   (`maxSkew: 1`, `kubernetes.io/hostname`, `DoNotSchedule`).
2. **Delete the ScaledObject**, or nothing below takes effect.
3. Warm: scale up to `nodes`, then back down — the surplus sleeps, keyed to GPUs
   the scheduler really handed out.
4. Gate on `WARM_POOL_NS=<ns> bash hack/benchmark/warm_pool.sh verify <n>`.
5. Scale up again and time each new replica from `creationTimestamp` to its
   `Ready` condition. **≤15 s means it woke; anything else rebuilt.**
6. Restore the ScaledObject.

Or use the supported harness, which does the timing part:

```bash
make benchmark-actuation BENCHMARK_NAMESPACE=<ns>      ACTUATION_TARGET=<deployment> ACTUATION_TRIALS=5
```

Baseline recorded on pokprod: **median 90 s, 0 of 6 woken.**

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
- [`../guides/fma/`](../guides/fma/) — operator-facing, including
  the warm-pool gate
