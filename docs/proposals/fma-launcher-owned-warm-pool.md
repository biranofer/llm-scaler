# Proposal: launcher-owned GPUs, and no requesters

**Status:** design sketch, not implemented. Recorded 2026-08-18 so it is not
rediscovered. Origin: after measuring that every placement-based approach fails
for the same structural reason, the question was asked the other way round —
*do we need requesters at all?*

**The idea in one sentence.** Give a launcher Pod the GPUs it uses, keep a known
population of such launchers, and scale by **waking and sleeping instances inside
them** — instead of creating a requester Pod per replica and hoping the scheduler
hands it the GPU a sleeper already occupies.

**Read "RECOMMENDED: an elastic pool of GPU-holding launchers" first.** It
supersedes the two earlier framings kept below: applying GPU ownership to *all*
launchers discards the property FMA was designed for (warm processes costing no
accelerators), and the reserved-subset hybrid still treats the commitment as
permanent. Making the POOL the elastic unit removes both problems.

---

## The defect this removes

Every scale-up today is a **fresh scheduling event**. A requester Pod is created,
the scheduler picks a node, the device plugin hands it an arbitrary GPU, and only
*then* does the controller ask whether a sleeping instance exists for that
(model, GPU) pair. Reuse keys on the GPU UUID, so the answer is usually no.

The lottery is structural, not a tuning problem. Measured on pokprod
(`evgensh-wva-test`, Qwen3-0.6B, 2026-08-18):

| configuration | placement | result |
| --- | --- | --- |
| unconstrained | — | 0 woke / 3 rebuilt (64s, 64s, 95s) |
| hard node pin, node saturated | 1 node, all free GPUs used | **3 woke / 0 rebuilt** (2s, 3s, 3s) |
| `warmAffinity` (preferred podAffinity) | **3/3 on launcher nodes** | 0 woke / 3 rebuilt (43-70s) |
| `warmAffinity`, repeat | **3/3 on launcher nodes** | 0 woke / 3 rebuilt (44-79s) |
| `warmAffinity` + forked controller | **3/3 on launcher nodes** | 1 woke (2s) / 2 rebuilt |

Placement was **solved** and it did not help: replicas landed beside sleepers
every time. The wake still requires the *exact GPU*, and a pool node has 7-8 of
them. Coverage across the cluster at the time, from `warm_pool.sh coverage`:

```
covered 6 of 44 free GPUs        7 of 12 nodes at 0%
best node 50%, most nodes 17-33%
```

0/3, 0/3 and 1/3 is not bad luck. It is what 14% coverage looks like.

## Why every other route was rejected

Each of these was considered and ruled out on evidence, not taste:

- **Better placement** (node affinity, per-model labels, a scheduler-visible
  extended resource) only raises `P(land on the right node)`, which was already
  1. It cannot touch `P(handed a covered GPU)`, which is what decides a wake.
  An extended resource additionally makes the request *hard*, so an exhausted
  pool yields `Pending` rather than a slow start — strictly worse.
- **Respawn on whichever GPU the requester got, reusing host-resident weights.**
  Measured at **~41 s** end to end with the compile cache hitting (3 s of it) and
  weights read from the PVC in 2.8 s. The remaining ~32 s is process spawn, vLLM
  import, profiling, warmup and API startup — none of which a cache removes, all
  of which sleep mode skips. Independently reproduced by this session's rebuilds
  at 43-79 s. The plan's own criterion was "seconds delivers the pool; tens of
  seconds means reconsidering". It is tens.
- **Full coverage** (a sleeper on every free GPU) does give 100%, but the
  invariant is *every free GPU is covered* and **the free set is not ours**. When
  another tenant releases a GPU it becomes free and uncovered, and the next
  requester can be handed exactly that one. Coverage decays without anyone
  touching the pool.
- **DRA** (claim a specific GPU by UUID) is the correct generic answer and is
  **not available**: pokprod runs v1.32.9 with no `resource.k8s.io` APIs and no
  DRA driver. GPUs are whole-card there — `sharing-strategy: none`,
  `replicas: 1`, MIG capable but `strategy: single`.
- **A device-agnostic wake in vLLM** would delete the problem, and the pinning is
  a *process* property rather than a weights property: sleep level 1 offloads
  weights to host RAM, but the process keeps a CUDA context bound to the device
  in `CUDA_VISIBLE_DEVICES`, set per instance before spawn
  (`inference_server/launcher/launcher.py:187`) and baked into the Pod spec by
  the controller (`pkg/controller/dual-pods/inference-server.go:1917`). A CUDA
  context cannot migrate. So "sleep to host, wake on any device" is a **vLLM**
  feature request, not an FMA one, and it does not exist.

## The proposal

**Launchers own their GPUs; instances are the unit of scale.**

1. A launcher Pod requests `nvidia.com/gpu: N` and holds those N cards for its
   lifetime. Today it requests **none** — `pkg/controller/utils/pod-helper.go`
   strips the device-plugin resources from limits, requests and Pod overhead, and
   sets `NVIDIA_VISIBLE_DEVICES=all`. The *requester* holds the GPU, and that is
   the requester's entire reason to exist.
2. Instances are created on those GPUs once and thereafter **slept and woken in
   place**. The GPU for an instance is decided at creation and never re-rolled.
3. Scaling means "wake instance X" / "sleep instance Y", not "create a Pod and
   see what the scheduler does".

Because no scheduling happens per scale event, GPU identity stops being a
lottery. There is nothing left to cover, align, or hope for.

The mechanism already exists: instances are created and deleted through the
launcher API (`/v2/vllm/instances`), and sleep/wake are HTTP calls to the
instance — which is exactly what `wakeUp` does today (`POST /wake_up` against
`podIP:serverPort`). Nothing new is needed to drive it.

## Why FMA was built the other way round, in its own words

This is not an oversight to correct; it is a trade `docs/dual-pods.md` made
deliberately, and the reasoning is worth stating because it bounds any change.

Kubernetes assumes a Pod is both a description of workload and a set of OS
processes. A launcher breaks that: it is "a flexible **platform** rather than a
unit of workload", and dual pods exists "as a way of bridging that mismatch". So
the roles were split — the requester **describes the workload** and therefore
carries its resource requirements; the launcher **runs the processes**. The
enabling trick is stated plainly:

> we can construct Pods that have access to all of the accelerators on their node
> while being accounted --- in the Kubernetes scheduler and kubelet --- as
> consuming none of them.

The objective is that warm processes cost nothing. The launcher-populator places
a launcher on **every eligible node**; if each held accelerators, standing FMA up
would reserve an entire cluster's GPUs before serving one token. Zero-GPU
launchers are what make ubiquitous pre-warming affordable.

**And the lottery is written into that same paragraph:**

> The dual pods implementation gives to the process that actually runs vLLM an
> environment variable setting that directs vLLM and the nvidia runtime to use
> **the accelerators that were chosen by the Kubernetes scheduler and kubelet for
> the server-requesting Pod**.

The GPU is chosen by the scheduler, per requester, at scale-up time. A sleeping
instance is pinned to a GPU chosen in a *previous* such decision, and nothing
makes the scheduler repeat it. Reuse needs the same card drawn twice.

So the design keeps Kubernetes as the accelerator allocator — ordinary
scheduling, quota and accounting all keep working — and the price is that FMA
does not get to choose the GPU. Everything measured above is that price.

|  | zero-GPU launchers (today) | GPU-owning launchers |
| --- | --- | --- |
| Warm capacity costs | **nothing** | a held GPU |
| Launchers can be everywhere | **yes** | only where you pay |
| Who picks the GPU | Kubernetes, per scale-up | you, once at creation |
| Wake reliability | lottery | **deterministic** |

## RECOMMENDED: an elastic pool of GPU-holding launchers, one GPU each

This supersedes both framings below. It keeps the determinism of launcher-owned
GPUs without the permanent cost, and it dissolves the endpoint problem instead of
solving it.

**The shape.** A pool of launcher Pods, each requesting exactly **one** GPU and
hosting several instances on it: one awake, the rest asleep. Scale the POOL to
change GPU capacity. Within a Pod, change which model is live by sleeping one
instance, waking another, and swapping the `llm-d.ai/model` label.

Why one GPU per Pod: only one instance per GPU can be awake at a realistic
`gpu-memory-utilization` anyway, so one Pod hosts one live model. Pod-level
readiness therefore stays meaningful and Services, EndpointSlices and the
InferencePool keep working with no new machinery — they select Pods, and the
label says which pool this Pod currently belongs to.

**What this buys that nothing else here does.** Repurposing a GPU from model A to
model B in ~2-3 s: sleep A, wake B, swap the label. That is not merely a fast
scale-up, it is fast *reallocation* — which is what a multi-model autoscaler
actually needs and cannot do today at any speed.

| concern | mechanism |
| --- | --- |
| GPU capacity | pool size — an ordinary Deployment scaled by HPA/KEDA/WVA |
| Which model is live on a card | sleep/wake + `llm-d.ai/model` label swap, ~2-3 s |
| Which models stay warm on a card | the sleeping set, bounded by `--sleeper-limit` and memory |
| Endpoint membership | ordinary label selection, unchanged |
| GPU accounting | correct by construction: the Pod requests what it uses |

Because the GPU commitment is elastic at the POOL level while fixed from any
individual instance's point of view, the earlier objection to launcher-owned
GPUs — that you lose scale-to-zero — does not apply. Nothing is scheduled per
scale event, so there is no lottery; and nothing is held that the pool size does
not justify.

Requesters, dual-pod binding, the reflector labels, the GPU-UUID component of the
instance ID and the entire reclaim path all become unnecessary.

**The sharp edges, which are dials rather than gaps:**

1. **The warm set per card is small.** At `gpu-memory-utilization 0.95` a card
   fits the awake instance plus roughly two sleepers (~1.4 GiB each) — about
   three models. More requires lowering utilisation, trading KV-cache space for
   warm breadth.
2. **`--sleeper-limit` caps it** (default 1, set to 2 on pokprod) and is
   controller-global. It is the one flag that must change.
3. **A model outside a Pod's warm set is still a ~41 s cold start**, so *which*
   models are warm on *which* cards is the central policy question.
4. **Scale-down must choose a victim** holding warm instances someone may want.
5. **Weights come from the PVC the first time** a model appears on a card, so
   warm sets accumulate rather than existing at pool creation.

Items 3, 4 and 5 are allocation decisions in tokens of demand and cost — WVA's
job, not FMA's, consistent with `fma-shared-warm-pool.md`.

**Note on implementation.** A launcher cannot acquire a GPU after the fact: Pod
resource requests are immutable, in-place resize (KEP-1287) is alpha on v1.32 and
covers only CPU and memory, and device-plugin resources cannot be resized at all.
So GPU-holding launchers are *created* that way — a second LauncherConfig whose
podTemplate requests one GPU. `LauncherPopulationPolicy.countForLauncher` is
already a list of `{launcherConfigName, launcherCount}`, so one policy can
maintain both an ordinary and a reserved population without an API change.

## The hybrid, which is probably the right shape

An all-or-nothing inversion throws away the property the original design was
built for. It is not necessary. Keep **zero-GPU launchers as the default
everywhere** — cheap, ubiquitous, exactly as designed — and additionally run a
small number of **GPU-owning launchers as a reserved warm pool**.

- Up to K instances wake deterministically in 2-3 s, because those GPUs are held
  and their instances are woken by choice rather than by scheduling.
- Demand beyond K falls back to today's elastic behaviour, cold starts included.
- K is a knob with a meaning a person can reason about: *how much am I willing to
  pay for guaranteed sub-3-second capacity?*

That is an allocation question in tokens of demand and cost, which is what WVA
exists to answer, and it needs no change to how ordinary launchers work. It also
degrades honestly: exceeding the reserve is slow, not broken.

## What is given up

*(This section and the two above it describe the earlier all-or-nothing framing.
They are kept because the reasoning still bounds the design; where they conflict
with the RECOMMENDED section, the RECOMMENDED section wins. In particular, point
3 below does NOT apply to the elastic pool, which releases GPUs by scaling down.)*

1. **The Deployment scaling interface.** HPA and KEDA scale Deployments;
   "wake instance #3" is not a replica count. Less painful here than elsewhere:
   WVA is already a KEDA external scaler on `feat/wva-external-scaler`, so it is
   positioned to actuate this directly.
2. **Endpoint membership.** The launcher is currently the serving endpoint (it
   carries `llm-d.ai/inferenceServing`), with one awake instance per Pod. A
   launcher owning N GPUs could serve N awake instances on N ports of one Pod IP
   — but Services, EndpointSlices and the InferencePool all select **Pods**, with
   Pod-level readiness. Per-instance endpoints inside one Pod is real work.
3. **True scale-to-zero of GPU cost.** Holding GPUs means never handing them
   back. Today's design does release them — which is precisely why another tenant
   can take one and break coverage.

## The trade, stated honestly

> **Reliable warmth requires holding the GPU. A GPU cannot simultaneously be free
> for other tenants and reserved for your sleeper.**

Today's architecture tries to have both and gets neither cleanly: it releases the
GPU to Kubernetes while retaining ~1.4 GiB on it invisibly. That is upstream's
own complaint ("warm instances hold GPUs Kubernetes cannot see"), and it was
observed live — node `pokprod-b93r39s1` showed **0 free GPUs and 1 sleeper**, i.e.
a sleeper squatting on a card already allocated to someone else.

This proposal refuses the fudge and pays the cost explicitly. In exchange the
wake becomes deterministic.

That trade is **right for production llm-d**, where the GPUs are yours, holding
them is normal, and predictable wake latency is the product. It is wrong for a
shared research cluster, where the appeal of releasing GPUs between spikes is
exactly what makes warmth unreliable.

## What survives of FMA

Roughly half.

- **Keep:** the launcher and its instance lifecycle — a per-node process manager
  that owns GPUs and can create, sleep and wake vLLM instances. That is the part
  that delivers 2-3 s wakes, and it is genuinely valuable.
- **Drop:** requesters, the dual-pod binding, the state-change reflector labels,
  the populator's launcher-count juggling, and the stale-binding failure modes.
  That machinery exists to reconcile two Pods representing one thing, and nearly
  every defect found in this area lives there: reclaiming a sleeper over an
  inference-port conflict, GPU-hash mismatches, stale `dual` labels surviving an
  unbind, and WVA attribution dropping launchers because they belong to a
  LauncherConfig rather than a Deployment.

Allocation — which models hold warm instances on which GPUs — then sits in WVA,
where `fma-shared-warm-pool.md` already argues it belongs.

## Open questions, in the order they should be answered

1. **Can a launcher Pod granted `nvidia.com/gpu: N` place instances on specific
   cards among its N?** It should: `CUDA_VISIBLE_DEVICES` inside the container
   maps to the allocated devices, and `launcher.py` already does UUID→index
   translation per instance. **This is inference from reading, not a measurement,
   and it is the load-bearing assumption of the whole design.** Cheap to test;
   test it first.
2. **How are N awake instances in one Pod exposed as endpoints?** The honest
   options are one instance awake per launcher (simple, wasteful), a headless
   Service with per-port endpoints, or one launcher Pod per GPU (which restores
   Pod-level readiness and is probably the pragmatic answer — a launcher Pod
   holding exactly one GPU, with M sleeping instances for M models on it).
3. **Does WVA's actuation path fit?** It already computes desired replicas per
   variant and speaks the KEDA external-scaler protocol. Mapping "desired = 3"
   onto "wake these three instances" needs an actuator that addresses instances,
   not Deployments.
4. **What is the cost model?** Holding a GPU continuously versus paying ~41 s of
   cold start per spike. This is the number that decides whether the trade is
   worth making, and it is a business input, not a measurement.

Option 2's third variant deserves emphasis: **one launcher Pod per GPU, holding
several sleeping instances for several models**, keeps Pod-level readiness and
endpoint semantics intact while still removing the requester. It may be the
smallest change that gets the whole benefit.

## See also

- [`fma-shared-warm-pool.md`](fma-shared-warm-pool.md) — the measured economics
  of sleepers (~1.4 GiB each), `--sleeper-limit` as the binding constraint, and
  why allocation belongs in WVA
- [`fma-fork-problem-statement.md`](fma-fork-problem-statement.md) — what the
  fork must fix, including Fix 1 (do not destroy a sleeper that only conflicts on
  the inference port), implemented and validated on cluster
- [`fma-upstream-requests.md`](fma-upstream-requests.md) — the findings filed
  against FMA, including warm instances holding GPUs Kubernetes cannot see
- [`../guides/fma/README.md`](../guides/fma/README.md) — operating the current
  design, and `warm_pool.sh coverage` for measuring what predicts a wake
