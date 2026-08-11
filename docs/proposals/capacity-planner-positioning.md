# Cluster Capacity Planner — Positioning & Proposal

> **Status:** Draft for discussion (untracked working doc) · **Audience:** SIG-Autoscaling / architecture
> **Repo (proposed):** `llm-d-cluster-capacity-planner` · **Date:** 2026-08-02 (rev 2026-08-03)

---

## 1. TL;DR

**Cluster Capacity Planner** is a forecast- and cost-driven **capacity-planning advisor** for llm-d. It answers a question no component in the ecosystem answers today:

> *Given where traffic is heading and what it costs, how should a fixed GPU pool be divided between **interactive** and **batch** inference so each class meets its SLO / deadline at minimum real dollars — and when should batch work drain into the spare capacity interactive leaves idle?*

It is **not** an autoscaler. It **recommends** a plan (Phase 1) and later **maintains** the structural allocation (Phase 2). It sits *above* the runtime autoscaler (WVA/KEDA) and the batch dispatcher (`llm-d-batch-gateway`), setting the envelope they operate within. It consumes existing llm-d building blocks (benchmark profiles, OpenCost cost metrics, vLLM/EPP telemetry) rather than replacing them.

**Why now:** every layer of llm-d today is *reactive* — routing reacts to pod state, autoscaling reacts to saturation, batch reacts to congestion. There is no *forward-looking* planning layer. Meanwhile the single biggest cost lever in LLM serving — filling the valleys of interactive demand with deadline-tolerant batch work — is monetized today only by hyperscalers (the "50%-off-for-24h" batch APIs) and exists in self-hosted stacks only as research.

**Complementary, not a rewrite, and provider-neutral.** Cluster Capacity Planner sits *above* llm-d's existing batch runtime (`llm-d-batch-gateway` / `llm-d-async`) — it adds the missing planning layer, it does not replace them (§6.1). And it is indifferent to who owns the GPUs: enterprises self-hosting llm-d capture the win as lower cost; cloud/inference providers building on llm-d capture it as a resellable discounted batch tier (§7.3–7.4).

---

## 2. The problem

### 2.1 Everything in llm-d is reactive; nothing plans ahead

| Layer | Component | Reacts to |
|---|---|---|
| Request routing | Inference Gateway + EPP | Current per-pod state (queue, KV, prefix) |
| Runtime scaling | WVA → HPA/KEDA | Current saturation / queue depth |
| Batch dispatch | `llm-d-batch-gateway` + `llm-d-async` | Current congestion (AIMD on 429/5xx) |

No component forecasts arrival rate, sizes a fleet to a predicted load + SLO, or decides how to *partition* capacity between workload classes. That forward-looking sizing layer is the gap.

### 2.2 Interactive and batch have opposing objectives — and must share GPUs

- **Interactive** — latency-first (TTFT/TPOT). Small batches, headroom kept deliberately idle, KV reserved. You *pay for latency* → low utilization → expensive per token.
- **Batch** — throughput-first, deadline-tolerant. Large batches, high GPU-memory utilization, queueing tolerated. You *pay for throughput* → high utilization → cheap per token.

Because their optima conflict, they cannot simply share one auto-scaled pool. Deciding the **split** and each class's **serving config** is a design/allocation decision — exactly the kind of thing you *plan* and then implement, not something a reactive loop should discover on the fly.

---

## 3. What Cluster Capacity Planner is — and is not

It lives at a different **altitude** from an autoscaler. That distinction is what keeps it from being "another WVA."

| | **Cluster Capacity Planner** | **Autoscaler (WVA / KEDA)** |
|---|---|---|
| Question | "What *should* the fleet look like?" | "Match the fleet to *right now*." |
| Timescale | Hours–days (provisioning horizon) | Seconds–minutes (control loop) |
| Loop | Open-loop: forecast → recommend | Closed-loop: observe → actuate |
| Output | A **plan** (partition, configs, GPU counts) | A **scale action** (desired replicas) |
| Consumer | Humans / GitOps / provisioning / CI | HPA/KEDA → Deployment `scale` |
| Failure mode | A stale recommendation | An outage / thrash |

**Layering:**

```
Cluster Capacity Planner  ──▶ sets the PARTITION + per-class config + min/max envelope + batch schedule
   │ hours–days, forecast + cost driven, structural
   ▼
Autoscaler (WVA/KEDA) ──▶ flexes replicas WITHIN each partition to track live load
   │ seconds–minutes, reactive
   ▼
EPP / batch-gateway ──▶ routes requests / dispatches batch WITHIN the allocation
```

The planner never emits desired-replicas on a control loop, so it does not compete with or duplicate the autoscaler. It hands the autoscaler a bounded envelope to operate in.

---

## 4. The unifying idea: valley-filling

The interactive and batch halves are not two features — they are one optimization seen from both sides:

- **Interactive** produces a **forecast with troughs** — idle GPU-hours between demand peaks.
- **Batch** is a **deadline-elastic backlog** that can be *poured into those troughs* instead of demanding dedicated GPUs.

So Cluster Capacity Planner runs a single **valley-filling** allocation: size the fixed pool so interactive SLOs hold at the peaks and batch deadlines are met by draining backlog into the valleys — bursting to spot / off-peak capacity only when the valleys aren't enough.

```
GPU-hours
 │        interactive forecast (peaks)
 │      ╱╲        ╱╲
 │     ╱  ╲      ╱  ╲        ← reserve for interactive SLO
 │____╱    ╲____╱    ╲____
 │████│    │████│    │████   ← batch backlog fills the valleys (deadline-bounded)
 └───────────────────────── time
```

This is why adding batch planning *strengthens* the concept rather than diluting it: both classes are served by the same fixed pool, and the plan is the schedule that keeps both whole.

---

## 5. Cost as the objective function — OpenCost

The partition decision is *minimize $ subject to per-class SLOs / deadlines*. OpenCost supplies the dollars — and, crucially, **OpenCost 1.121.0 already ships llm-d-specific inference-cost metrics**:

- `llm_cost_per_million_tokens{model_name, cost_basis, ...}` — **input/output split**, reported on two bases:
  - `cost_basis="allocation"` — fully-loaded (you pay for reserved GPUs even when idle).
  - `cost_basis="usage"` — compute during active inference only.
- `llm_total_hourly_cost`, `node_gpu_hourly_cost`, and the `/allocation` API (`gpuCost`, `gpuCostIdle`, `gpuHours`; aggregate by `label:`).

**The allocation-vs-usage gap *is* the interactive-vs-batch economics** — it directly quantifies the idle/utilization premium latency-first serving pays. The planner sizes the *partition* against `allocation` (you pay for what you reserve) and judges *config efficiency* against `usage`.

**Notes / gotchas:**
- WVA does **not** consume OpenCost — it uses a static relative `llm-d.ai/variant-cost` scalar (default `10.0`). Real-dollar planning is therefore an open niche. The planner can optionally *write* an OpenCost-derived relative cost back into `variant-cost` so the autoscaler below it makes cost tie-breaks on real prices.
- On-prem GPU price is a `$0.95/hr` placeholder; validate `node_gpu_hourly_cost` before trusting (see `opencost#3781` underpricing bug).
- Idle attribution is a *policy choice*; charge idle to interactive (`idleByNode=true`) so its true premium is visible.
- `llm-d-incubation/llm-d-inference-cost` is an **empty reserved repo** — coordinate before naming a cost component.

---

## 6. Batch-inference planning — the primary white space

A survey of the field (llm-d + K8s schedulers + Ray + cloud + research) shows the market splits into four buckets, **all of which execute or react — none plans batch to a deadline**:

| Layer | Who | What they do | Gap |
|---|---|---|---|
| Runtime execution / harvesting | vLLM, Ray Data LLM, ConServe, Valve, Sarathi-Serve, SpotServe | Continuous batching, chunked prefill, harvest idle GPUs, survive spot | Runs work; never sizes to a deadline |
| Quota / admission queueing | Kueue, Volcano, YuniKorn, KAI, Run:ai | Order + admit jobs over *existing* capacity; node growth delegated to Karpenter/CA | No completion-deadline (EDF) sizing |
| Cost-aware instance selection | SkyPilot, Karpenter, Cast AI | Pick cheapest pool/spot once work exists | Blind to backlog + deadline + off-peak |
| Productized black-box batch | OpenAI / Bedrock / Vertex / Azure Batch | Sell the *outcome*: 24h window → 50% off via opaque internal valley-filling | Customer sizes **nothing**; no planner exposed |

### 6.1 We build on llm-d's existing batch stack — we don't replace it

llm-d already ships real batch machinery, and Cluster Capacity Planner is **complementary** to it:

- **`llm-d-batch-gateway`** — OpenAI-compatible `/v1/batches`, AIMD adaptive concurrency (additive +1 on success, halve on HTTP 429/≥500), saturation-gated admission (dispatch budget `D = 1 − saturation`, admit above baseline `B`).
- **`llm-d-async`** — queue-based dispatch of individual requests behind admission gates.

Together they own the **runtime** — dispatching batch work and limiting interference with interactive traffic. What they do **not** do is forecast, size capacity to a deadline, or valley-fill. Their `completion_window` is a **kill-switch**, not an objective: undispatched work at expiry is dropped to `error.jsonl` as `batch_expired`; deadline-aware sizing appears in their design docs but is **unbuilt**.

**The seam is clean and already half-wired.** Cluster Capacity Planner sizes the batch partition and sets the concurrency ceilings / dispatch budget (`GlobalConcurrency`, `PerModelMaxConcurrency`, baseline `B`); the gateway fills that envelope at runtime and feeds backlog metrics back. We add the **planning layer above proven execution** — not a competing batch system. (This is why the "no one plans batch" claim is about the *planning* layer specifically; the *execution* layer in llm-d is real and reused.)

### 6.2 The combination nobody ships

**Nobody ships the combination Cluster Capacity Planner owns:**

> Given a **backlog of N requests + a completion deadline T**, size the GPUs (accounting for prefill/decode + heterogeneous SKUs) to finish on time, and **place that work into spare/off-peak/spot capacity** at minimum real cost.

The only prior art is research: **Pollux / Aladdin** (goodput-/SLO-based GPU sizing), **Chiron / Chronus** (deadline-driven backlog autoscaling), **ConServe / Valve / Harvest-VMs** (valley-filling substrate), **SpotServe / SkyServe** (spot). None is packaged as a Kubernetes capacity planner. Dynamo's **SLA Planner** is the closest shipped planner but targets *online latency SLOs*, not batch deadlines.

**Framing hooks:**
- **Proof of demand:** the universal cloud "50%-off-for-24h" pattern — esp. Azure's explicit *"dynamic quota… when extra capacity is available"* — is exactly valley-filling economics. Cluster Capacity Planner lets **any llm-d operator — self-hosted enterprise or provider — capture the economics hyperscalers monetize today** (see §7.3–7.4).
- **Vocabulary:** position as the missing *planning* layer above mature *execution* primitives — **goodput** (DistServe), **stall-free/chunked-prefill admission** (Sarathi-Serve), **harvesting/valley-filling** (ConServe/Valve), **deadline scheduling** (Niyama/Chronus/Chiron).

---

## 7. Boundaries & positioning (build-on, not compete)

### 7.0 The three-way boundary — they size, we allocate, WVA scales

Three distinct jobs, no real overlap once named precisely:

| | llm-d-planner's **Capacity Planner** (Chen / Masluk) | **Cluster Capacity Planner** (this) | **WVA** |
|---|---|---|---|
| Question | "How much GPU does *this model* need?" | "How to *divide a shared cluster pool* across workloads over time?" | "Match replicas to load *now*." |
| Direction | Bottom-up: model → resources | Top-down: forecast + budget → allocation | Reactive: metrics → replicas |
| Unit | One model / config | Many models · 2 classes (interactive/batch) | One variant |
| Time | Day-0, one-shot | Ongoing, forecast-driven | Seconds–minutes |
| Verb | **Sizes** | **Allocates** | **Scales** |

**In-scope for us:** forecasting · interactive/batch partitioning · batch deadline scheduling & valley-filling · real-cost (OpenCost) optimization across the shared pool · setting the envelopes WVA and batch-gateway operate within.

**Out — we delegate:** per-model memory/config sizing → **call llm-d-planner's Capacity Planner** (its per-model KV/memory math + BLIS simulator are our sizing *kernel*, not a reimplementation); runtime replica control → **WVA/KEDA**; request routing → **EPP**; batch dispatch → **batch-gateway**.

**Confirmed no collision (2026-08-03).** llm-d-planner's own 3-stage roadmap deepens *per-deployment* (serving-stack knob tuning, single-config simulation via the BLIS discrete-event simulator) and explicitly does **not** contemplate fleet/multi-model scheduling, forecasting, cost, or batch valley-filling — so our scope is white space. Their component is literally named "Capacity Planner"; we are **Cluster** Capacity Planner (the cluster-wide level they don't address). Roadmap artifacts are thin, so a direct confirmation with the maintainers (Fredette / Chen / Masluk) is worthwhile.

### 7.1 vs. `llm-d-incubation/llm-d-planner`

It is a **day-0, single-model deployment configurator**: you state a model + requirements, it recommends a GPU type + config and emits one model's YAML. It answers *"what should I deploy for **this** model?"*

| Axis | `llm-d-planner` (existing) | **Cluster Capacity Planner** |
|---|---|---|
| Question | "What to deploy for this model?" | "How to split the fleet across classes?" |
| Scope | Single model | Whole pool, multi-model, interactive+batch |
| Workload input | You *state* it | We *forecast* it |
| Decision | GPU type + config for one model | **Partition** GPUs + batch schedule |
| Time | Day-0, one-shot | Ongoing, forecast-driven |
| Cost | Estimated / static | Real dollars (OpenCost) |
| Relationship | — | **Consumes it** as the per-config sizing kernel |

**We do not compete with it — we call it.** It sizes one config; we decide how many of each config the pool should run.

### 7.2 vs. WVA / `ig-wva` / Dynamo SLA Planner

- **WVA** (runtime autoscaler) and **`ig-wva`** (ILP provisioning prototype, hardcoded GCE pricing) solve *replica counts / provisioning*, reactively or statically, for **online** load. Cluster Capacity Planner sets the envelope they act within, adds **forecasting**, **two-class partitioning**, **real cost**, and **batch deadlines**. `ig-wva`'s ILP is prior-art for the allocation math.
- **Dynamo SLA Planner** is the closest shipped capacity planner, but it plans to *steady-state online latency SLOs* — no batch backlog/deadline, no fleet partition, single-deployment scope.

### 7.3 Not competing with cloud batch — bringing its economics in-house

The cloud Batch APIs (OpenAI, Bedrock, Vertex, Azure) are **not competitors**; they are **proof of demand**. They all sell the same tier: a relaxed ~24h completion window at **~50% off** the synchronous price. That discount is possible because a provider's fleet is sized for **peak** interactive demand, so between peaks GPUs sit idle; deadline-tolerant batch work is scheduled into those idle troughs at near-zero marginal cost. **The batch discount is a provider reselling spare capacity it would otherwise waste** — valley-filling, done opaquely and internally, with no planner exposed to the customer.

Cluster Capacity Planner brings that mechanism to any self-hosted llm-d cluster: idle troughs that are dead loss on an owned fleet become paid work. We do not compete with the cloud batch *business* — we are the engine that lets an llm-d operator *run* it. The nearest **shipped** comparable in the planner space is **NVIDIA Dynamo's SLA Planner** (online latency SLOs, single deployment) — that, not any cloud API, is the benchmark to beat.

### 7.4 Who it's for — provider-neutral, two segments

Cluster Capacity Planner is indifferent to *who* owns the GPUs; the valley-filling logic is identical. That yields two markets:

| Segment | Who | Captures the win as |
|---|---|---|
| **Self-host** | Enterprises running llm-d on owned/rented GPUs | Lower TCO — less idle waste |
| **Provider** | Neoclouds / inference providers building on llm-d | New revenue — resell a discounted batch tier, powered by the planner |

A cloud/inference provider running llm-d wouldn't compete with the planner — they'd **run it as the brain behind their batch tier.** And because valley-filling pays more the larger and more multi-tenant the fleet (deeper troughs, more deadlines to pack — cf. Valve's +34% utilization on 8,054 GPUs), the **provider segment is the highest-value use, not an edge case.**

Realistic caveat for honesty: the big proprietary clouds (OpenAI, Azure, AWS, GCP) mostly run their *own* internal stacks and are unlikely direct users. The provider segment that matters is the **open-stack / neocloud** world already adopting llm-d — where GPU-cloud players are among llm-d's backers.

---

## 8. Ecosystem connection map

```
                 ┌─ CONSUME (inputs) ──────────────────────────────────┐
  llm-d-benchmark (Benchmark Report v0.2 = perf profiles) ──┐
  llm-d-prism (same data lake, viz)                          │
  llm-d-inference-sim (GPU-free config what-if)              │
  llm-d-planner /api/v1/estimate (per-config kernel) ────────┼──▶  ┌──────────────────┐
  llm-d-kv-cache (KV memory model)                           │     │ CLUSTER CAPACITY │
  hermes (GPU/RDMA topology = supply side)                   │     │  forecast +      │
  OpenCost (real $/token, $/GPU-hr, idle premium) ───────────┘     │  interactive⇄    │
  Prometheus + vLLM/EPP telemetry (traffic, queue, KV) ───────────▶│  batch valley-   │
  batch-gateway backlog + job deadlines ─────────────────────────▶│  fill + cost     │
                                                                    └────────┬─────────┘
                 ┌─ PRODUCE-FOR (output) ─────────────────────────────────────┘
  llm-d-modelservice Helm values (decode/prefill.parallelism.{tensor,data},
   .replicas, accelerator.type, vLLM args; one release per workload class)
  + per-class envelope for WVA/KEDA + concurrency ceilings for batch-gateway

                 ┌─ POSITION AGAINST (don't duplicate) ─┐
  llm-d-planner (per-model sizer) · WVA/ig-wva (autoscaler/ILP) · Dynamo SLA Planner (online SLO)

                 ┌─ COMPLEMENTARY (defines the "batch" class runtime) ─┐
  llm-d-batch-gateway (+ llm-d-async): AIMD concurrency, saturation-gated admission.
   Seam: planner SIZES the batch partition + sets GlobalConcurrency / PerModelMaxConcurrency /
   dispatch-budget baseline B; gateway FILLS that envelope at runtime and feeds backlog metrics back.
```

---

## 9. Architecture sketch

Four stages:

1. **Forecaster** — from Prometheus history + vLLM token/queue metrics, predict per-class arrival rate **and ISL/OSL shape distribution** (not just average RPS). Horizon = provisioning/cold-start time. Candidate methods: Holt-Winters / Prophet / ARIMA for seasonality; distribution-aware, not point-estimate.
2. **Performance model** — map (config, load, ISL/OSL) → throughput & latency, from `llm-d-benchmark` Benchmark Report v0.2 profiles (offline), refined by live telemetry; reuse `llm-d-planner`'s per-config estimate as a kernel; validate candidates cheaply with `llm-d-inference-sim`.
3. **Allocator / valley-fill optimizer** — solve: partition the fixed pool across {interactive, batch, spare} × variants × accelerators to meet interactive SLOs at forecast peaks and batch deadlines in the valleys, minimizing OpenCost dollars (allocation basis), with spot/off-peak burst as an option. Prior art: `ig-wva` ILP, Pollux/Aladdin, Chiron/Chronus.
4. **Plan emitter** — serialize to `llm-d-modelservice` Helm values (one release per class) + per-class autoscaler envelope + batch concurrency ceilings. Phase 1: a report / proposed GitOps PR. Phase 2: reconciled structural objects.

---

## 10. Roadmap — Phase 1 recommend, Phase 2 implement

### Phase 1 — Recommend (advisory)
- **Goal:** produce a correct, trustworthy plan; validate the model against reality before anything acts on it.
- **Emits:** a plan artifact — report/dashboard or a proposed PR against the GitOps/values repo. A human approves and applies.
- **Writes nothing to the cluster.** Zero blast radius.
- **Success test:** shadow for weeks — "would-be plan" vs. what operators actually did / what the pool needed.

### Phase 2 — Implement (maintain)
- **Goal:** close the loop *structurally*, once recommendations are proven.
- **Owns:** partition-encoding objects — `ResourceQuota` / priority classes / node-pool assignments / per-pool min-max envelopes / batch concurrency ceilings.
- **Re-partitions** on a slow, structural cadence (traffic-regime shifts, hours/days), **not** on live load. Guardrails: rate-limited/hysteretic re-partitioning, min-dwell time, human-approval gate for large moves.
- **Stays non-autoscaler by construction:** it moves boundaries; the autoscaler flexes replicas inside them.

**Why phase it:** the modeling (forecast + perf model + SLO→GPU) is where the uncertainty is. Phase 1 proves it with no chance of causing an outage. Each phase ships value alone.

---

## 11. Open decisions

1. **Build on `llm-d-planner` vs. stand beside it** — consume its `/api/v1/estimate` as the per-config kernel, or reimplement. (Most affects scope.)
2. **Deliverable form** — report/dashboard, GitOps PR, or library/CLI (`plan` in CI)?
3. **Perf-model source** — offline benchmark profiles, passively-learned-from-history, or hybrid.
4. **Cost-component naming** — coordinate with the reserved `llm-d-inference-cost` repo owner.
5. **First proof scope** — single-model forecaster, interactive/batch split, or batch valley-filling to a deadline (the most differentiated).

---

## 12. References

- llm-d: https://github.com/llm-d · docs https://llm-d.ai
- WVA: https://github.com/llm-d/llm-d-workload-variant-autoscaler · `ig-wva`: https://github.com/llm-d-incubation/ig-wva
- Existing planner: https://github.com/llm-d-incubation/llm-d-planner
- Batch: https://github.com/llm-d/llm-d-batch-gateway · https://github.com/llm-d/llm-d-async · https://llm-d.ai/docs/dev/architecture/advanced/batch · https://developers.redhat.com/articles/2026/07/02/batch-inference-openshift-ai-llm-d
- Benchmark: https://github.com/llm-d/llm-d-benchmark · Prism: https://github.com/llm-d/llm-d-prism · Sim: https://github.com/llm-d/llm-d-inference-sim
- Modelservice: https://github.com/llm-d-incubation/llm-d-modelservice
- OpenCost + llm-d: https://opencost.io/blog/opencost-llmd-inference-cost/ · metrics https://opencost.io/docs/integrations/metrics/ · API https://opencost.io/docs/integrations/api/
- K8s batch: Kueue https://kueue.sigs.k8s.io · Volcano https://volcano.sh · KAI https://github.com/NVIDIA/KAI-Scheduler
- Cloud batch: OpenAI https://developers.openai.com/api/docs/guides/batch · Bedrock https://docs.aws.amazon.com/bedrock/latest/userguide/batch-inference.html · Vertex https://ai.google.dev/gemini-api/docs/batch-mode · Azure https://learn.microsoft.com/azure/foundry/openai/how-to/batch
- Dynamo SLA Planner: https://docs.nvidia.com/dynamo/latest/components/planner/sla-based-planner.html
- Research: DistServe (2401.09670) · Sarathi-Serve (2403.02310) · Niyama (2503.22562) · ConServe (2410.01228) · Valve (2604.07874) · Pollux (2008.12260) · Chronus (SoCC'21) · Chiron (2501.08090) · Aladdin (2405.06856) · SpotServe (2311.15566) · SkyServe (2411.01438)
