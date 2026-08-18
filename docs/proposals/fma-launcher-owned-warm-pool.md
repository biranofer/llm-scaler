# Proposal: launcher-owned GPUs, and no requesters

**Status:** design sketch, not implemented. Recorded 2026-08-18 so it is not
rediscovered. Origin: after measuring that every placement-based approach fails
for the same structural reason, the question was asked the other way round —
*do we need requesters at all?*

**The idea in one sentence.** Give a launcher Pod the GPUs it uses, keep a known
population of such launchers, and scale by **waking and sleeping instances inside
them** — instead of creating a requester Pod per replica and hoping the scheduler
hands it the GPU a sleeper already occupies.

**Read "THE OPERATING MODEL: the pool is a bridge, not a fleet" first**, then
"RECOMMENDED: an elastic pool of GPU-holding launchers". Together they supersede
the earlier framings kept further down.

The short version: the pool does not serve the load, it **covers the ramp**. WVA
wakes a pool instance on scale-up and sleeps it again once the ordinary replica
is serving, so the pool is sized by *concurrent spikes x ramp time* rather than
by peak capacity — and it degrades to today's slow start when empty.

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

## THE OPERATING MODEL: the pool is a bridge, not a fleet

This is the framing that makes the cost work, and it changes how the pool is
sized. Read it before the architecture below.

**Several pools, one per accelerator type, each holding warm instances of the
models that run on that type.** On a scale-up WVA wakes the corresponding
instances from the pool *and* scales the ordinary Deployment as usual. The pool
serves while the real replicas start; as each real replica begins **serving**,
WVA sleeps the pool instance and hands the traffic over. The slot returns to
standby, ready for the next spike.

### Why this changes the economics

Every other version of this design sized the pool against **load** — cover the
free GPUs, hold enough warm capacity to serve the peak. That is expensive, and it
is why "hold the GPUs" kept looking unaffordable.

As a bridge, the pool is sized against **ramp time**. A slot is occupied only for
the ~41-90 s a normal replica needs to become ready, then it sleeps and is
reusable:

```
pool size  ~=  concurrent spike arrivals  x  ramp time
```

not peak capacity. A handful of permanently-held slots can bridge many models'
spikes, serially. That is a small, affordable, fixed cost — the first version of
this proposal that plausibly survives a cost review.

### And it degrades to today's behaviour

If the pool is empty, or the model is not in that pool's warm set, the scale-up
is exactly what happens today: a slow start. Nothing goes `Pending`, nothing
breaks. That is the property the extended-resource idea could not offer, and it
means the pool can be introduced incrementally and switched off without risk.

It also fits WVA's existing shape: pool instances are a **transient** resource
WVA borrows and returns, not a serving fleet it has to own.

### Sequence

1. WVA decides 1 → 4.
2. Wake 3 pool instances of that model, apply the live label — traffic flows in
   **~3 s**.
3. In parallel, scale the ordinary Deployment 1 → 4.
4. As each real replica begins serving, sleep one pool instance and remove its
   label.

### Four sharp edges, in the order they bite

1. **Hand over on *serving*, not on `Ready`.** `readyReplicas` is not the same
   condition as being in the EndpointSlice and reachable through the router. This
   repo already paid for that distinction — `hack/benchmark/wait_serving.sh`
   exists because a 503 in exactly that window killed whole runs. The trigger
   must be the InferencePool's view.
2. **The bridge needs GPUs on both sides at once.** During handover the pool
   holds its GPUs *and* the new replicas hold theirs. On a full cluster the real
   replicas never schedule, the bridge never completes, and the pool stays awake
   — silently turning a burst buffer into permanent capacity. Needs an explicit
   timeout and a decision: keep serving from the pool, or give up and release.
3. **Attribution must not double-count.** Capacity is genuinely doubled for the
   bridge window. If WVA counts pool instances as durable supply it will suppress
   the very scale-up they exist to cover — the same failure mode as pending
   replicas counting toward anticipated supply.
4. **Pool composition is still the allocation question.** A model absent from a
   pool's warm set gets no bridge. Per-accelerator-type pools match the
   accelerator-aware modelling WVA already does, but *which* models each pool
   keeps warm is WVA's decision, and it is the same budgeting problem
   `fma-shared-warm-pool.md` describes.

### Where the controller lives

**Not in WVA.** Mechanism and policy split along the line FMA's own design rules
already draw ("FMA should expose mechanism and state"; allocation belongs to the
brain):

| | responsibility | home |
| --- | --- | --- |
| **Actuator** | make the live model on a Pod match what was asked: sleep one instance, wake another, swap the label | **FMA fork**, replacing the dual-pods controller |
| **Policy** | which models are live and warm where, pool size, when to hand over | **WVA today, the planner later** |

The contract is declarative and boringly Kubernetes-shaped — desired state in an
annotation, actual state in a label:

```
annotation  fma.llm-d.ai/desired-model: <isc-name>     # written by WVA/planner
label       llm-d.ai/model:             <isc-name>     # written by the actuator
```

The actuator's whole job is "make the label match the annotation". Anyone can
drive it by writing an annotation, with or without WVA.

Three reasons beyond purity: WVA is on a path toward replacement by the llm-d
planner, and a mechanism buried inside it retires with it; the actuator wants to
be event-driven on Pod and instance changes (the launcher already offers an
NDJSON watch stream) while WVA's loop is a 15 s optimizer behind KEDA polling;
and the actuator needs Pod CRUD in the launcher namespace, which is a wider
privilege than WVA otherwise holds.

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

## What of FMA is reusable, component by component

Surveyed at `aa072ef`. The headline: **the launcher half is already
requester-free and needs almost nothing done to it**, while the dual-pods
controller — about 3.5k lines, the bulk of the Go — is what goes.

### The finding that matters most

**The launcher has no sleep/wake endpoints, and does not need any.** Its API is
pure CRUD-plus-watch over *processes*. Sleep and wake are performed by the
controller calling **each vLLM instance's own OpenAI port directly**:

- `POST /wake_up` — `pkg/controller/dual-pods/inference-server.go:1565`
- `POST /sleep` — `inference-server.go:1780`
- `GET /is_sleeping` — `inference-server.go:2053`

Those endpoints exist only because the launcher Pod template forces
`VLLM_SERVER_DEV_MODE=1` (`pkg/controller/utils/pod-helper.go:328`) — load-bearing
and easy to lose.

So the central mechanism of this proposal — hold a GPU, keep several instances on
it, wake the one you want — **already works today and requires no launcher
change at all.** What is missing is only the controller that decides *which*.

### Reusable as is

| component | lines | why it survives |
| --- | --- | --- |
| `inference_server/launcher/launcher.py` | 967 | FastAPI multi-process vLLM host. **No Kubernetes client, no Pod concept, no requester vocabulary.** Forks instances as process groups, pins each to GPUs by translating `gpu_uuids` into `CUDA_VISIBLE_DEVICES` (`:176-191`) — it already knows how to place instances on specific cards. Offers a Kubernetes-watch-style NDJSON stream at `GET /v2/vllm/instances/watch?since=<rev>` with a 1000-event ring buffer, which is a better change primitive than polling. |
| `launcher/gputranslator.py` | 247 | UUID↔CUDA-index mapping via `pynvml`, plus a mock mode for CPU-only clusters. **This is how a GPU-owning launcher self-discovers its cards without a requester** — the gap left when the requester stops reporting them. |
| `pkg/controller/dual-pods/launcherclient.go` | 281 | Typed Go client for the launcher API. Zero requester concepts. Any replacement controller needs exactly this. |
| `cmd/launcher-populator` + `api/.../launcherpopulationpolicy_types.go` + `launcherconfig_types.go` | — | See below; the pool controller and its API. |
| `pkg/controller/generic/`, `pkg/controller/utils/generics.go`, `pkg/observability`, `pkg/common/flags.go`, `pkg/generated/**` | — | Plumbing: typed workqueues, informer scaffolding, metrics/pprof, generated clients. |
| chart RBAC + launcher-populator Deployment, `dockerfiles/Dockerfile.launcher.*` | — | Deployment surface for the half that stays. |

### Reusable with changes — and these are the real work

| component | change needed |
| --- | --- |
| **`pkg/controller/utils/pod-helper.go:345-352`** | `removeGPUResourceLimits` **zeroes `nvidia.com/gpu` on the launcher Pod**. This single function is the code expression of "the requester holds the GPU". **Deleting it is the change that makes this whole proposal possible.** |
| **`pkg/controller/launcher-populator/`** (~1.9k) | **This is already the "scale the pool" controller.** Desired count comes 100% from LauncherPopulationPolicy objects and never from requester Pods. Its only requester touch is defensive — `isLauncherBoundToServerRequestingPod` marks a launcher un-deletable. Swap that for "has a live instance" and it does the job. |
| `api/.../inferenceserverconfig_types.go` | Its `labels`/`annotations` are documented as applied to the providing Pod **"while bound"**. Redefine to **"while live"** — that *is* the label swap this design turns on. Everything else (port, options, env) is already "which model do I want an instance of". |
| `api/.../launcherpopulationpolicy_types.go` | Doc-only. It currently says the launcher count is the larger of what the policy says and **what the requesters need**; that second clause disappears, making the policy the *sole* source of pool size. A simplification. |
| `api/.../launcherconfig_types.go` | Nothing substantive — `maxInstances` already means "how many instances fit in one launcher Pod", which is exactly one awake plus N asleep. |
| `inference-server.go:804-1080` (~280) | The launcher-selection and LRU-reclaim policy, including the port-conflict fix from `aa072ef`. The *policy* — which sleeping instance to wake or evict — survives; its *inputs* must be re-plumbed away from requester Pods. |
| `launcher_pod_notifier.py` | The sidecar that publishes instance state onto the Pod as an annotation so an informer sees it. Mechanism is right, name is dual-pods branded. The launcher's own `/watch` may be the better primitive. |
| `pkg/api/interface.go`, `pkg/controller/common/interface.go` | Keep `SleepingLabelName`, `SleepState`, the launcher identity labels and `LauncherServicePort`; drop the binding vocabulary. |

### Not needed — requester-specific

`cmd/dual-pods-controller`, and **`pkg/controller/dual-pods/{controller.go,inference-server.go}` (~3.5k lines, the bulk of the Go code)**; `cmd/requester` and `cmd/test-requester`; `pkg/spi`; `pkg/server/requester/{probes,proxy}` and most of `coordination`; the dual-pods chart template; the requester validating-admission-policy and examples; `Dockerfile.requester`; `inference_server/benchmark/` as written.

### What this says about the size of the job

The valuable half — a per-node multi-process vLLM host that can pin instances to
GPUs, sleep and wake them, and stream state changes — **exists, is tested, and is
already free of the requester concept.** The pool controller exists too and is
nearly free of it.

What has to be built is a controller that decides which instance is live on each
card and swaps the label accordingly, and that is a much smaller thing than what
it replaces. The one-line change that unlocks it is deleting
`removeGPUResourceLimits`.

**Open question this survey settles:** the launcher already translates
`gpu_uuids` to `CUDA_VISIBLE_DEVICES` per instance, so a launcher Pod granted its
own GPUs can certainly place instances on specific cards among them. What remains
untested is only whether *several instances co-resident on one card* behave —
which `--sleeper-limit` (2 on pokprod) already permits and which is cheap to try.

## Open questions, in the order they should be answered

1. **Do several instances co-reside on ONE card, one awake and the rest asleep?**
   This is now the only load-bearing untested assumption. Placement *onto a
   chosen card* is settled — `launcher.py:176-191` already translates `gpu_uuids`
   into `CUDA_VISIBLE_DEVICES` per instance — and `--sleeper-limit` (2 on
   pokprod) already permits two sleepers per GPU. What has not been observed is
   the full pattern on a single card: awake instance at
   `gpu-memory-utilization 0.95` plus N sleepers at ~1.4 GiB each, and a clean
   swap between them. Cheap; test it first.
2. **How fast is the swap?** Sleep A, wake B, relabel. Expected ~2-3 s from the
   wake measurements, but sleep-then-wake as a single operation has never been
   timed, and the label→EndpointSlice→InferencePool propagation adds an unknown
   tail. That tail, not the wake, may dominate.
3. **Does WVA's actuation path fit?** It already computes desired replicas per
   variant and speaks the KEDA external-scaler protocol. Mapping "desired = 3"
   onto "wake these three instances, relabel those Pods" needs an actuator that
   addresses instances rather than Deployments.
4. **Which models stay warm on which cards, and who is evicted?** The allocation
   problem, and the reason this belongs with WVA. `fma-shared-warm-pool.md` §6.2
   already sketches the pricing.
5. **What is the cost model?** Holding a pool of GPUs versus paying ~41 s of cold
   start per spike. A business input, not a measurement — but the one that
   decides whether any of this is worth building.

**Settled by the component survey**, and previously listed here as open: a
launcher Pod can place instances on specific cards among those it holds, and the
endpoint question is answered by one GPU per launcher Pod — one live model per
Pod keeps Pod-level readiness and ordinary label selection intact.

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
