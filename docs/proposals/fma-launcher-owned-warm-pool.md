# Proposal: launcher-owned GPUs, and no requesters

**Status:** design sketch, not implemented. Recorded 2026-08-18 so it is not
rediscovered. Origin: after measuring that every placement-based approach fails
for the same structural reason, the question was asked the other way round —
*do we need requesters at all?*

**The idea in one sentence.** Give a launcher Pod the GPUs it uses, keep a known
population of such launchers, and scale by **waking and sleeping instances inside
them** — instead of creating a requester Pod per replica and hoping the scheduler
hands it the GPU a sleeper already occupies.

**Read in this order:** "THE OPERATING MODEL: one shared pool, many models",
then "Lifecycle", then "What we must build, and what we reuse". Those three carry
the current design. Everything after them is earlier reasoning, kept because it
records why the alternatives were rejected -- their verdicts are superseded where
they disagree.

The short version: **one shared pool per accelerator type, not one per model.**
Each pool Pod owns a GPU and keeps several models asleep on it (~1.4 GiB each),
at most one awake. WVA wakes a model on scale-up -- serving in ~3 s -- and sleeps
it again when ordinary replicas take over, or holds it when they cannot arrive.
The same mechanism parks scaled-to-zero models. **No new controller**: the pool is
an ordinary Deployment, readiness gates traffic, and WVA drives it from its
existing loop.

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

## THE OPERATING MODEL: one shared pool, many models

**Per-model pools are not the goal and do not pay.** N models would need N warm
GPUs, which is the same as simply running them. The entire proposition is that
**K shared GPUs give fast-start coverage for N models** -- K=4 covering twenty
models is worth building; twenty pools of one is not.

**Several pools, one per accelerator type.** Each pool Pod holds one GPU and
several vLLM instances on it: at most one awake, the rest asleep at ~1.4 GiB
each. Any model in a Pod's warm set can be serving within ~2-3 s.

### The pool has two jobs, and they are the same mechanism

1. **A parking lot for scaled-to-zero models.** A parked model holds no replicas
   and no GPU of its own, yet returns to service in seconds because its weights
   sit asleep in the pool.
2. **A burst buffer for running models.** A spike is covered from the pool while
   ordinary replicas start.

Both are "wake an instance that is already there". Nothing distinguishes them
mechanically, which is why one pool serves both.

### Sizing, corrected for long drain

An earlier revision sized the pool as *concurrent spikes x ramp time*, assuming a
slot frees in ~41-90 s. **That assumption does not hold.** A slot may be held far
longer: no free GPUs cluster-wide, a node scale-up in progress, a sustained spike
rather than a burst, or a large model loading. So:

- **K is driven by concurrent sustained burst demand**, not by ramp time.
- **The pool can be exhausted**, which forces an explicit policy -- fall back to a
  cold start, queue, or preempt a lower tier.
- The pool is therefore **fast-start shared burst capacity that usually hands
  back**, rather than a bridge that always does.

It remains worth having: capacity arrives in ~3 s instead of ~41 s, shared across
every model on that accelerator. But the honest description is "K GPUs of elastic
burst headroom plus a parking lot", not "a cheap latency trick".

### The constraint that governs placement: awake-slot contention

Models co-resident on one card are cheap in memory (~1.4 GiB asleep) but they
**contend for the single awake slot**. While a Pod serves model X, models Y and Z
warm on that same card cannot be woken -- for the whole drain, which may be long.

So co-location is not free, and the instinct to pack densely is wrong. The warm
set assignment wants the opposite: **spread models that may spike together across
different cards, and co-locate models unlikely to spike simultaneously.** It is
an anti-affinity problem, not a packing problem.

There is a dial against it. "One awake per card" follows from
`--gpu-memory-utilization 0.95`. At ~0.45 a card could hold two awake instances,
halving each model's KV cache to double concurrency. For long drain that may be
the better trade, and it is a per-pool decision.

## Lifecycle: how several models come to be parked on one Pod

This is the part that has to be concrete, because it is where scale-up and
scale-down actually touch the pool.

### Parking, on scale-down to zero

A terminating replica cannot donate its warmth -- the process dies and the
weights go with it. Parking is therefore an **asynchronous re-creation** in the
pool:

1. WVA scales model X's Deployment to zero (its ordinary replicas terminate and
   release their GPUs).
2. WVA picks a pool Pod on the right accelerator with a free instance slot, and
   asks its launcher to `POST /v2/vllm/instances` for X.
3. That instance loads the model -- ~41 s, in the background, with nothing
   waiting on it -- and is then put to sleep.
4. X is now parked: ~1.4 GiB of a shared card, no GPU of its own, wakeable in
   ~2-3 s.

The cost of parking is paid once, off the critical path, at the moment demand has
already gone.

### Waking, on scale-up from zero

1. WVA wakes X's instance in the pool and labels that Pod into X's InferencePool
   -- serving in **~3 s**.
2. In parallel it scales X's ordinary Deployment up.
3. When the ordinary replicas are **serving**, WVA sleeps the pool instance and
   removes the label. X stays parked, ready for next time.
4. If the ordinary replicas never arrive (no GPUs), the pool keeps serving until
   `maxHoldSeconds`, then WVA decides whether to hold or release.

### How a Pod comes to hold several models

Two ways, and both are wanted:

- **Assigned.** WVA places a model's warm instance on a chosen Pod when parking
  it, or when it predicts a spike. This is the deliberate path and the one the
  allocation policy drives.
- **Accumulated.** A Pod that served model Y during a burst keeps Y's instance
  asleep afterwards rather than deleting it. The warm set therefore drifts toward
  models actually used -- an LRU cache of weights, filled by real traffic.

Eviction is the counterpart: when a Pod is at `maxInstances`, or the card's
sleeper budget is full, WVA deletes the least valuable instance to make room.
That decision -- least recently used, lowest tier, least likely to spike -- is
policy, and it is the same budgeting problem `fma-shared-warm-pool.md` describes.

### Why scaling the pool Deployment is safe

Scaling the pool **up** adds an empty Pod which fills over time, by assignment or
by use. Scaling it **down** destroys warm sets, so the victim choice matters --
prefer Pods whose models are also warm elsewhere, and never a Pod currently
serving. Both are ordinary Deployment operations otherwise; no special machinery.

## What we must build, and what we reuse

Working this through changes the answer sharply, and in our favour.

### Reused from FMA -- essentially just the launcher

| component | role here | verdict |
| --- | --- | --- |
| `inference_server/launcher/launcher.py` (967) | the multi-process vLLM host: create/delete instances, pin each to a GPU via `gpu_uuids` to `CUDA_VISIBLE_DEVICES`, list and watch them | **as is** |
| `launcher/gputranslator.py` (247) | lets the Pod discover its own GPUs through `pynvml`, with no requester to report them | **as is** |
| `dockerfiles/Dockerfile.launcher.*` | the image | **as is** |
| vLLM's own `/sleep`, `/wake_up`, `/is_sleeping` | the actual sleep and wake, reached on each instance's own port; needs `VLLM_SERVER_DEV_MODE=1` | **as is** |

### Not needed at all

**The entire Kubernetes half of FMA.** Because the pool is an ordinary Deployment:

- `dual-pods-controller` (~3.5k lines) -- nothing binds requesters to providers;
- `launcher-populator` and `LauncherPopulationPolicy` -- the Deployment
  controller places Pods, so nothing has to reconcile per-node launcher counts;
- `InferenceServerConfig` / `LauncherConfig` CRDs -- optional: the launcher's API
  takes a `VllmConfig` (options, `gpu_uuids`, env, annotations) directly, so WVA
  can supply it from its own model configuration;
- `pod-helper.go` and its `removeGPUResourceLimits` -- **we never call it**, since
  we author the pool Deployment ourselves and simply request the GPU.

**Consequence: the FMA fork is not on the critical path for this design.** Commit
`aa072ef` fixes the dual-pods reclaim path, which this architecture does not use.
It remains correct and useful for the *current* deployment model and should stay,
but nothing here waits on it.

### To be built

| piece | size | where |
| --- | --- | --- |
| **Pool Deployment manifest** -- launcher image, `nvidia.com/gpu: 1`, model-cache volume, readiness probe | config | GitOps, beside the models |
| **Readiness gate** reflecting instance state -- Pod ready only while an instance is awake and serving | ~50 lines, sidecar or exec probe | in the Pod. Today's launcher readiness is `GET /v2/vllm/instances` (`pod-helper.go:258-289`), which answers 200 whenever the *launcher* lives, regardless of sleep. The pattern exists at `pkg/server/requester/probes`. |
| **Pool discovery and state** -- find pool Pods by label, read their warm sets | small | WVA. No sidecar or annotation needed: `GET /v2/vllm/instances` on each pool Pod already returns exactly this. |
| **Actuation** -- wake/sleep an instance, patch `llm-d.ai/model`, gate readiness | small | WVA, in the loop and RBAC it already has |
| **Allocation policy** -- which models are warm where, spike anti-affinity, eviction, exhaustion behaviour, hold timeouts | the real work | WVA |

So the engineering is concentrated in WVA policy, the mechanism is reused, and
the only new artefact outside WVA is a readiness shim and a Deployment YAML.

### Configuration shape

```yaml
# One per accelerator type. Sized in replicas; that is the whole knob.
metadata:
  labels:
    llm-d.ai/warm-pool: "true"
    llm-d.ai/accelerator: NVIDIA-H100-80GB-HBM3   # no model label: it hosts several
spec:
  replicas: 4
```

Per tier, eligibility and the behaviours long drain forces:

```yaml
warmPool:
  eligible: true
  maxSlots: 2             # cap per model, so one spike cannot take the whole pool
  onExhausted: fallback   # fallback | queue | preempt-lower-tier
  maxHoldSeconds: 900     # give the slot back even if no ordinary replica arrived
```

`onExhausted` and `maxHoldSeconds` exist only because a slot may be held for a
long time; a bounded bridge would need neither.

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

## Appendix: full component survey of the FMA tree

*(Surveyed at `aa072ef`. Kept for its file-level detail, which is still accurate.
Its VERDICTS were written for the earlier architecture, where a controller
replaced the dual-pods controller and the launcher-populator was retained. Under
the current design -- pool as an ordinary Deployment -- less is needed than this
section claims: the populator, the LauncherPopulationPolicy and `pod-helper.go`
all fall away too. Where the two disagree, "What we must build, and what we
reuse" above is authoritative.)*


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
