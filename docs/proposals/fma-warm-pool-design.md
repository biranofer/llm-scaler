# Design: a shared warm pool for fast multi-model actuation

**Status:** design, not implemented.
**Supersedes the exploration in** [`fma-launcher-owned-warm-pool.md`](fma-launcher-owned-warm-pool.md),
which records why every alternative was rejected and should be read only for that
reasoning.
**Prerequisites are unmet** — see [Prerequisites](#7-prerequisites-before-any-of-this-is-built).
Two of them can invalidate the design, so they come before implementation.

---

## 1. What this is

A small, shared population of GPU-holding Pods that keep several models resident
but asleep, so that capacity for any of them arrives in **seconds instead of
~41 s**. WVA wakes a model when it scales up, and puts it back to sleep once
ordinary replicas take over.

It serves two needs with one mechanism:

- **burst** — cover a spike while ordinary replicas start;
- **parking** — keep a scaled-to-zero model wakeable without holding a GPU for it.

**Why a shared pool rather than warm capacity per model:** N models would need N
warm GPUs, which is the same as simply running them. The proposition is that
**K shared GPUs give fast-start coverage for N models**.

### What it is not

Not a serving fleet. The pool covers the gap until ordinary replicas arrive, or
until a hold timeout expires. Ordinary Deployments continue to do the serving, and
if the pool is empty or has no warm copy of the model, behaviour degrades exactly
to what happens today: a cold start. Nothing goes `Pending`, so the pool can be
introduced incrementally and switched off without risk.

---

## 2. Architecture

**One pool = one ordinary Kubernetes Deployment.** No new controller, no new CRD.

| property | value | why |
| --- | --- | --- |
| GPUs per Pod | **exactly the TP size** of the models it serves | one awake instance then occupies the whole Pod, so one Pod hosts one *live* model — Pod readiness stays meaningful and one `llm-d.ai/model` label is unambiguous |
| instances per Pod | 1 awake + N asleep | `LauncherConfig.maxInstances` caps N |
| pool identity | (accelerator type, TP size, **sleep level**) | each is immutable per Pod, so each needs its own Deployment |
| pool size | `spec.replicas` | ordinary scaling; `kubectl scale` works |
| traffic switch | **Pod readiness** | asleep → not ready → out of the EndpointSlice. Kubernetes does the endpoint churn; nothing hand-rolls it |
| which model is live | `llm-d.ai/model` label | selects the InferencePool the Pod joins |

**Why one GPU (or one TP group) per Pod:** a 4-GPU Pod running four independent
TP=1 instances would need to express "model A serving, model B asleep" through a
single Pod readiness flag and a single model label. It cannot. One live model per
Pod keeps Services, EndpointSlices and InferencePool semantics untouched.

### Sleep level is a property of the pool

Pod resource requests are **immutable after admission**. A Pod that might hold a
level-1 model must reserve RAM for it *at creation*, before anything knows which
models will land there. So the level cannot be chosen per model at placement time.

| | **level-1 pool** | **level-2 pool** |
| --- | --- | --- |
| sleeping weights | offloaded to **host RAM** | **discarded**; re-read on wake |
| memory request | sized for the warm set | small, constant |
| warm set bounded by | **host RAM** | GPU residue and `--sleeper-limit` |
| wake cost | `weight / B_h2d` — storage-independent | `weight / B_storage` |
| suits | large models, or slow storage | small models, or fast storage |

**Level 2 is the default; level 1 is a workaround for slow storage.** As
`B_storage` approaches `B_h2d`, level 2 strictly dominates — the same wake time
without any of the RAM.

---

## 3. Configuration

### A pool

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: warm-pool-h100-tp1-l2
  labels:
    llm-d.ai/warm-pool: "true"                      # WVA discovers pools by this
    llm-d.ai/warm-pool-level: "2"                   # 1 or 2; immutable per pool
    llm-d.ai/accelerator: NVIDIA-H100-80GB-HBM3
spec:
  replicas: 4                                        # K — the pool size knob
  template:
    spec:
      containers:
        - name: launcher                             # the FMA launcher image, unmodified
          env:
            - name: VLLM_SERVER_DEV_MODE
              value: "1"                             # REQUIRED: /sleep and /wake_up exist only with this
          resources:
            limits:
              nvidia.com/gpu: 1                      # == TP size of this pool's models
              memory: 8Gi                            # level 2: buffers + process overhead only
          readinessProbe:                            # NEW: must reflect instance state, not launcher liveness
            exec:
              command: ["/bin/sh", "-c", "/app/ready.sh"]
          volumeMounts:
            - name: model-cache
              mountPath: /model-cache
      volumes:
        - name: model-cache
          persistentVolumeClaim:
            claimName: model-pvc
```

A level-1 pool differs in exactly two fields:

```yaml
    llm-d.ai/warm-pool-level: "1"
    memory: 64Gi        # <- the warm-set budget; WVA admits models against this
```

### Sizing

```
# level 1 — wake bounded by PCIe; RAM-hungry
pod memory  >=  SUM(weight size of every model in the warm set) + overhead
pod GPU     >=  awake instance + ~1.4 GiB per sleeper

# level 2 — RAM-cheap; wake bounded by the read path
pod memory  >=  small, ~MB per sleeper
pod GPU     >=  awake instance + ~1.4 GiB per sleeper

# the level itself is derived from a MEASURED bandwidth, not a size cutoff:
level 2  iff  weight_size / B_storage <= T_wake
```

**The Pod's memory request is the warm-set budget.** No `warmSlots` field: a byte
budget handles heterogeneous models naturally, where a slot count would either
waste capacity or overcommit it.

### Policy, per ScalingPolicy tier

```yaml
warmPool:
  eligible: true          # interactive tier yes, batch tier no
  maxSlots: 2             # cap per model, so one spike cannot consume the pool
  onExhausted: fallback   # fallback | queue | preempt-lower-tier
  maxHoldSeconds: 900     # release the slot even if no ordinary replica arrived
```

`onExhausted` and `maxHoldSeconds` exist because a slot may be held for a long
time — see [§4.5](#45-handover-from-pool-to-ordinary-replicas).

### Admission rule

WVA admits model M to pool P when:

```
P.accelerator matches M
P.gpuCount    == M.tensorParallelSize
GPU residue and --sleeper-limit permit one more sleeper on the chosen Pod

and either
    P.level == 2  and  weight(M) / B_storage <= T_wake
or  P.level == 1  and  warmBytes(Pod) + weight(M) + overhead <= P.memoryRequest
```

If no pool admits M, the outcome is a cold start, and it must be **visible**: a
`reason` on `wva_model_scaling_blocked` (e.g. `no_warm_pool_admits_model`),
following this repo's convention that new diagnostics become reasons rather than
new gauges, since WVA owns no API object to carry a status condition.

---

## 4. Sequencing, and what each step costs

Provenance matters here, so each figure is marked: **[M]** measured on pokprod,
**[P]** published by vLLM, **[?]** not measured.

### 4.1 Pool creation

| step | cost |
| --- | --- |
| Deployment created, Pods scheduled | seconds; **[?]** on a busy cluster, unbounded if no GPUs are free |
| image pull (launcher image, first time per node) | **[?]** typically tens of seconds |
| launcher process start, GPU discovery via `pynvml` | seconds **[?]** |
| **Pod Ready with an empty warm set** | **[?]** — but off any critical path |

A new pool Pod arrives **empty**. It becomes useful only as models are admitted to
it. This is a provisioning operation, not a serving one.

### 4.2 New model admission (also: parking)

Adding a model to a Pod is **one call to a running launcher** — nothing is scaled
or restarted.

| step | cost |
| --- | --- |
| `POST /v2/vllm/instances` — launcher forks a vLLM process | sub-second **[?]** |
| process start, vLLM import, API startup | ~29 s **[M]** (the residue of a 41 s cold start after compile and weight load) |
| weight load from `/model-cache` | ~2.8 s for ~1.2 GB **[M]**, i.e. ~430 MB/s — may have been a page-cache hit |
| `torch.compile` | 3.08 s **[M]** with the shared cache hitting; 12.35 s on a miss |
| engine init: profile, KV cache, warmup | 9.29 s **[M]** |
| `POST /sleep?level=N` | **[?]** |
| **total to warm-and-asleep** | **~41 s [M]**, entirely off the critical path |

**Parking a scaled-to-zero model is exactly this flow.** A terminating replica
cannot donate its warmth — the process dies with the weights — so parking is an
asynchronous re-creation, paid once, at the moment demand has already gone.

**Constraint:** the load needs transient GPU memory of roughly the full weight
size, while a sleeper rests at ~1.4 GiB **[M]**. On a card already serving at
`--gpu-memory-utilization 0.95` only ~4 GiB is free, so **models are added to
*idle* Pods**; a serving Pod has its warm set effectively frozen. Running pool Pods
at `0.85` leaves ~12 GiB free and lifts that restriction, at the cost of KV cache
for whatever is serving.

### 4.3 Wake — the step this whole design exists for

| | level 1 | level 2 |
| --- | --- | --- |
| Qwen3-0.6B | 0.26 s **[P]** | 0.85 s **[P]** |
| Phi-3-vision | 0.82 s **[P]** | 2.58 s **[P]** |
| observed on pokprod (level 1) | **2–3 s [M]** | — |
| scaling | `weight / B_h2d` | `weight / B_storage` |

Then the Pod must become Ready and reach the InferencePool: **[?]** the
label → EndpointSlice → InferencePool propagation is unmeasured and **may dominate
the wake itself**.

Against a cold start of ~41 s **[M]**, this is the ~15× saving that motivates
everything above.

### 4.4 Sleep

| step | cost |
| --- | --- |
| drain: mark not-ready, let in-flight requests finish | **[?]**, bounded by request duration |
| `POST /sleep?level=N` | **[?]** |
| Pod leaves the EndpointSlice | seconds **[?]** |

### 4.5 Handover from pool to ordinary replicas

1. WVA wakes the pool instance and labels the Pod in — serving in **~3 s [M]**.
2. In parallel, it scales the ordinary Deployment.
3. Ordinary replicas start — **~41 s [M]** each, **and unbounded if no GPUs are free**.
4. When they are **serving** (not merely `Ready`), WVA sleeps the pool instance.

**Hand over on *serving*, not `Ready`.** `readyReplicas` is not the same condition
as being in the EndpointSlice and reachable through the router;
`hack/benchmark/wait_serving.sh` exists because a 503 in that window destroyed
whole benchmark runs.

**The bridge needs GPUs on both sides at once.** On a full cluster the ordinary
replicas never schedule, the bridge never completes, and the pool silently becomes
permanent capacity. `maxHoldSeconds` bounds it; `onExhausted` decides what happens
next.

### 4.6 Pool scaling

| | cost and consequence |
| --- | --- |
| scale **up** | a new empty Pod, per §4.1; it fills by admission or by use |
| scale **down** | **destroys warm sets.** Prefer Pods whose models are warm elsewhere; never a Pod currently serving |

### 4.7 How a Pod comes to hold several models

- **Assigned** — WVA places a model when parking it or when it predicts a spike.
- **Accumulated** — a Pod that served model Y keeps Y asleep afterwards instead of
  deleting it, so the warm set drifts toward models actually used: an LRU cache of
  weights, filled by real traffic.

Eviction is the counterpart, and is policy: least recently used, lowest tier,
least likely to spike.

### Summary of the time budget

| operation | cost | on the critical path? |
| --- | --- | --- |
| wake a warm model | **0.26–3 s** [P]/[M] | **yes — the point of the design** |
| label → InferencePool propagation | **[?]** | **yes, and unmeasured** |
| sleep a model | **[?]** sub-second + drain | no |
| admit / park a model | **~41 s** [M] | no |
| create a pool Pod | **[?]** tens of seconds | no |
| ordinary replica start | **~41 s** [M], unbounded if GPU-starved | it is what the pool hides |

---

## 5. What is reused from FMA, and what is built

### Reused, unmodified

| component | role |
| --- | --- |
| `inference_server/launcher/launcher.py` (967 lines) | the multi-process vLLM host: create/delete instances, pin each to GPUs by translating `gpu_uuids` into `CUDA_VISIBLE_DEVICES` (`:176-191`), list them, and stream changes over a Kubernetes-watch-style NDJSON endpoint. **No Kubernetes client, no Pod concept, no requester vocabulary.** |
| `launcher/gputranslator.py` (247) | UUID↔CUDA-index mapping via `pynvml`; how a GPU-owning Pod discovers its own cards with no requester to report them |
| `dockerfiles/Dockerfile.launcher.*` | the launcher image |
| `LauncherConfig.maxInstances` | already means "instances per launcher Pod", i.e. one awake plus N asleep |
| vLLM `/sleep`, `/wake_up`, `/is_sleeping` | the actual mechanism, on each instance's own port — requires `VLLM_SERVER_DEV_MODE=1` |

**The launcher API has no sleep/wake endpoints and needs none** — sleep and wake
are calls to the vLLM instance itself, which is what FMA's controller already
does (`pkg/controller/dual-pods/inference-server.go:1565`, `:1780`, `:2053`).

### Not needed — the entire Kubernetes half of FMA

Because a pool is an ordinary Deployment:

- `dual-pods-controller` (~3.5k lines, the bulk of the Go) — nothing binds
  requesters to providers;
- `launcher-populator` and `LauncherPopulationPolicy` — the Deployment controller
  places Pods;
- requester binary, `pkg/spi`, `pkg/server/requester/{probes,proxy}` — no
  requesters exist;
- `pod-helper.go` and its `removeGPUResourceLimits` — **never called**; we author
  the pool Deployment and simply request the GPU.

**The FMA fork is therefore not on the critical path.** Commit `aa072ef` fixes a
reclaim path this architecture does not use. It stays correct for the *current*
deployment model, but nothing here waits on it.

### To be built

| piece | size | where |
| --- | --- | --- |
| pool Deployment manifests | config | GitOps, beside the models |
| **readiness gate reflecting instance state** | ~50 lines | in the Pod. Today's launcher readiness is `GET /v2/vllm/instances`, which answers 200 whenever the *launcher* lives, regardless of sleep. `pkg/server/requester/probes` is the existing pattern for a gated `/ready`. |
| pool discovery and warm-set state | small | WVA — `GET /v2/vllm/instances` on each pool Pod already returns exactly this, so no sidecar or annotation is needed |
| actuation: wake/sleep, patch the model label, gate readiness | small | WVA, in the loop and RBAC it already has |
| **allocation policy** — which models are warm where, spike anti-affinity, eviction, exhaustion, hold timeouts | the real work | WVA |

---

## 6. Constraints that shape the policy

**Awake-slot contention.** Models co-resident on a card are cheap in memory
(~1.4 GiB asleep) but **contend for the single awake slot**: while a Pod serves X,
its other warm models cannot be woken — for the whole hold, which may be long. So
the warm set wants **anti-affinity**: spread models that may spike together across
Pods, and co-locate models unlikely to spike simultaneously. Dense packing is the
wrong instinct.

**Large models may not pay.** At 70 B, level 1 costs ~20 s and ~140 GB of host RAM
per Pod, while level 2 costs minutes. A cold start or a permanently running replica
is likely better. This is a real boundary on applicability, independent of hardware.

**Attribution must not double-count.** During a handover, capacity is genuinely
doubled. If WVA counts pool instances as durable supply it will suppress the very
scale-up they exist to cover — the same failure mode as pending replicas counting
toward anticipated supply.

---

## 7. Prerequisites, before any of this is built

Two of these can invalidate the design, so they are not follow-ups.

1. **Does a woken engine return CORRECT output?** Open vLLM issues report that it
   may not: [#16234](https://github.com/vllm-project/vllm/issues/16234) (improper
   response after `/wake_up`) and
   [#17103](https://github.com/vllm-project/vllm/issues/17103) (meaningless
   outputs after sleep→wake). **Every measurement taken on pokprod was
   time-to-Ready; nothing verified output correctness.** A pool serving garbage in
   3 s is worse than one serving correctly in 41 s. Those issues predate the
   v0.26.0 engine in use and may be fixed — check, do not assume.
2. **Measure `B_storage` properly.** Drop caches, read a known-size weight file
   from `/model-cache`, and time it. The single 430 MB/s figure may have been a
   page-cache hit. This sets the level-1/level-2 crossover.
3. **Measure the label → EndpointSlice → InferencePool propagation delay.** It is
   on the critical path and may dominate a 3 s wake.
4. **Confirm several instances co-reside on one card** — one awake at
   `gpu-memory-utilization 0.95` plus N sleepers, with a clean swap between them.
   `--sleeper-limit` is 2 on pokprod, so this is cheap to try.
5. **For LWS only: does sleep/wake work across NODES?** vLLM claims TP and PP
   support, but the docs never mention Ray or cross-node, every published benchmark
   is single-node (largest: Qwen3-235B at TP=4 on one host), and
   [#21231](https://github.com/vllm-project/vllm/issues/21231) reports
   `--enable-sleep-mode` having no effect on Ray workers. If it does not work, LWS
   support ends there — a warm group would hold full GPU memory and keep exactly
   one model warm, which is plain over-provisioning.

---

## 8. References

**vLLM**

- [Sleep Mode](https://docs.vllm.ai/en/latest/features/sleep_mode/) — levels,
  `VLLM_SERVER_DEV_MODE=1`, `wake_up(tags=[...])`, and the level-1 CPU-memory
  requirement
- [Zero-Reload Model Switching with vLLM Sleep Mode](https://vllm-project.github.io/2025/10/26/sleep-mode.html)
  — the level-1/level-2 benchmark table
- Issues: [#16234](https://github.com/vllm-project/vllm/issues/16234),
  [#17103](https://github.com/vllm-project/vllm/issues/17103),
  [#21231](https://github.com/vllm-project/vllm/issues/21231)

**This repo**

- [`fma-launcher-owned-warm-pool.md`](fma-launcher-owned-warm-pool.md) — the
  exploration, and why each alternative was rejected on evidence
- [`fma-shared-warm-pool.md`](fma-shared-warm-pool.md) — measured sleeper
  economics, `--sleeper-limit`, and why allocation belongs in WVA
- [`fma-fork-problem-statement.md`](fma-fork-problem-statement.md) — the defects
  in the current dual-pods path, and Fix 1 as landed
- [`../guides/fma/README.md`](../guides/fma/README.md) — operating the current
  design, and `warm_pool.sh coverage` for the number that predicts a wake
