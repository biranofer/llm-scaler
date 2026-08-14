# Topology-agnostic pod attribution: making WVA work with and without FMA

## Status

Proposed.

## Problem

WVA cannot measure a Fast Model Actuation (FMA) variant at all.

FMA splits a model server across two pods. A **requester** Deployment carries the
llm-d identity and is what a scaler moves; **launcher** pods, owned by a
`LauncherConfig`, hold the GPU and run vLLM. WVA attributes a pod's metrics by
walking its `ownerReferences` looking for a Deployment or LWS under a
ScaledObject. A launcher's owner is a `LauncherConfig`, so the walk ends with
nothing and the pod is dropped into `wva_pod_mapping_miss_total`.

The two halves fail in opposite directions:

| | scalable | reports engine metrics |
| --- | --- | --- |
| requester Deployment | yes | no — it runs no engine |
| launcher pods | no — owned by a `LauncherConfig` | yes |

So there is no workload WVA can both scale and measure. Point a ScaledObject at
the requester and the controller says so every cycle:

```
has 1 ready pod(s) but none attributed
```

Measured on pokprod001 (2026-08-14): EPP dispatched ~64 req/s across nine
launchers against 5.9 req/s to the decode pod. A launcher scraped mid-benchmark
reported 143 requests running, 61 waiting, KV cache 99.9% full. WVA saw none of
it, and demand was computed from the decode pod alone.

This is not an FMA defect. It is WVA assuming that the pod which reports is the
pod which is owned, and FMA is the first topology where that is deliberately
false.

## What FMA actually does

From FMA's `docs/dual-pods.md`, confirmed against a live cluster:

- **The binding is recorded on the pods themselves.** `dual-pods.llm-d.ai/dual`
  is "put on both requester and provider Pods. Value is the name of the dual
  Pod." The authoritative record is an annotation on the provider; the label is
  the observable form, present on *both* halves of a bound pair.
- **Unbound means asleep.** "An unbound server-providing Pod is asleep. The
  transitions between asleep and awake happen while bound: wake up ASAP after
  binding, go to sleep just before unbinding." So *bound ⇔ awake*, and a pod
  carrying the `dual` label is by construction a live engine.
- **Replicas are the scale lever.** "Increasing the replica count on a
  server-requesting Deployment creates additional server-requesting Pods, each
  requiring its own binding to a server-providing Pod. Each replica generates a
  distinct inference server instance." Scaling the requester Deployment is
  exactly the actuation WVA already performs — nothing new is needed on the
  write path.
- **The provider joins the InferencePool, not the requester.** "The Pod labels
  that match the right InferencePool go on the nominal server-providing Pod and
  not on the server-requesting Pod," inherited from the `InferenceServerConfig`.
- **The ReplicaSet must not own the provider.** The server patch "must also
  change the Pod's labels in such a way that the nominal server-providing Pod
  does not match the Pod label selector in the definition of the set." The
  ownerReferences gap is intentional and permanent. Waiting for it to close is
  not a plan.
- **`dual-pods.llm-d.ai/accelerators`** is an *annotation* — not a label —
  "that the dual-pods controller maintains on both server-requesting and
  server-providing Pods". Value is a comma-separated list of GPU UUIDs.

Verified against FMA's own source, `pkg/api/interface.go`, because the whole
design rests on it:

| key | kind | applied to |
| --- | --- | --- |
| `dual-pods.llm-d.ai/dual` | label | **both** requester and provider |
| `dual-pods.llm-d.ai/sleeping` | label | provider only |
| `dual-pods.llm-d.ai/instance` | label | requester only |
| `dual-pods.llm-d.ai/accelerators` | annotation | both |
| `dual-pods.llm-d.ai/launcher-based` | annotation | provider only |

`DualLabelName` is documented as "the label that the dual-pods controller
maintains on the server-requesting and server-providing Pods". That it lives on
**both** halves is what makes this design a single pod GET instead of a
namespace-wide LIST: the launcher names its requester directly.

Also observed directly: a sleeping launcher still answers `/metrics` with
`vllm:num_requests_running 0`, `vllm:kv_cache_usage_perc 0` and
`vllm:engine_sleep_state{sleep_state="weights_offloaded"} 1`. Scraped and
attributed naively, it reads as an idle replica and *suppresses* scale-up.

## Who is the scale target, who emits metrics, and how we get from one to the other

This is the whole problem in one section. Two questions must have an answer for
every serving pod: *which ScaledObject governs it* and *which pod's series
describe it*. Today WVA assumes one pod answers both.

**Non-FMA (modelservice).** The chain is one walk, and every link was measured
on pokprod001:

```
ScaledObject  qwen-…-decode-wva            (scaleTargetRef, min=1 max=10)
  └─ Deployment  qwen-…-decode             ← what KEDA scales
       └─ ReplicaSet  …-9cf474444
            └─ Pod  …-decode-9cf474444-rpqdq   ← owned AND emitting
                    vllm:num_requests_waiting{pod="…-rpqdq"} = 53
```

The pod that carries the `pod` label on the vLLM series is the same pod the
ownerReferences walk starts from. `locator.Locate()` walks up, finds the
Deployment, looks it up in the registry, and returns the ScaledObject. One
direction, one walk, no ambiguity.

**FMA.** The two roles split across two pods that are deliberately not related
by ownership:

```
ScaledObject  <requester>-wva
  └─ Deployment  fma-requester-qwen-…      ← what KEDA scales
       └─ ReplicaSet
            └─ Pod  fma-requester-…-9rlg7  ← owned, emits NOTHING
                    labels: llm-d.ai/role=requester
                            dual-pods.llm-d.ai/dual = launcher-fma-…-pgqdm  ──┐
                                                                              │
   LauncherConfig  fma-qwen-…                                                 │
     └─ Pod  launcher-fma-…-pgqdm          ← emits EVERYTHING, owned by ──────┘
             vllm:num_requests_running = 143, waiting = 61, kv = 99.9%
             labels: dual-pods.llm-d.ai/dual = fma-requester-…-9rlg7
                     llm-d.ai/inferenceServing=true, llm-d.ai/model=…
```

The ownerReferences walk from the launcher reaches a `LauncherConfig` and stops.
FMA guarantees it always will: the server patch "must change the Pod's labels in
such a way that the nominal server-providing Pod does not match the Pod label
selector in the definition of the set", precisely so the ReplicaSet does not
adopt it.

**The traversal we add** is the dashed line above, in the launcher → requester
direction:

| step | how | cost |
| --- | --- | --- |
| 1. series → pod | `pod` label on `vllm:*` | already done |
| 2. pod → owner walk | existing `locator.Locate()` | already done |
| 3. *walk failed* → partner | read `dual-pods.llm-d.ai/dual` off the pod we already fetched | **0 extra calls** |
| 4. partner → owner walk | existing walk, from the requester pod | 1 GET |
| 5. Deployment → ScaledObject | existing registry lookup | already done |

Step 3 is free because `Locate()` has the pod object in hand at the moment the
walk fails. Only step 4 costs anything, and only for pods that were about to be
discarded.

Direction matters. Going requester → launcher would mean listing every pod in
the namespace to find which requester names a given launcher — a LIST per
collection cycle, in a hot path, for every namespace including non-FMA ones.
Because FMA maintains `dual` on **both** halves, we can go the cheap way.

## Design principles

1. **Auto-detect, never configure.** No FMA flag, no per-namespace switch. A
   namespace is FMA-shaped or it is not, and WVA finds out by looking.
2. **FMA may be installed after WVA, and may never be installed.** No dependency
   on the `fma.llm-d.ai` CRDs existing at boot: no informer, no watch, no client
   for those types, no RBAC on that group. Detection must survive the API
   surface appearing later, and cost nothing when it never does.
3. **Zero cost on the common path.** A non-FMA namespace must not pay an extra
   API call, and its behaviour must be byte-identical to today.
4. **Prefer the pod over the CRD.** Everything needed is on pod labels, which we
   already have RBAC for (`get;list;watch`, declared at
   `internal/collector/locator/locator.go:6`).

## The design

### 1. One pairing hop in the locator

`podLocator.Locate(ctx, namespace, podName)` already reads the pod and walks its
ownerReferences. Add a single fallback:

> When the walk ends without a managed scaler, and the pod carries
> `dual-pods.llm-d.ai/dual`, restart the walk from the pod it names.

A bound launcher names its requester; the requester is owned by a ReplicaSet →
Deployment → ScaledObject, and the existing walk resolves it unchanged. The
launcher's metrics are then attributed to the requester's scale target, which is
the thing WVA scales.

This satisfies every principle at once. It reads a label, not a CRD, so FMA can
be installed at any time and the next collection cycle picks it up with no
restart. It runs only after the walk has already failed, so a non-FMA namespace
never executes it — and a namespace with no FMA sees exactly one extra map
lookup on pods that were going to be dropped anyway.

Guards, all cheap and all necessary:

- **One hop only.** The second walk does not itself fall back. Two pods naming
  each other must not loop.
- **Same namespace.** The label carries a bare pod name; resolve it in the
  pod's own namespace and nowhere else.
- **Model must agree.** Accept the hop only when both pods carry the same
  `llm-d.ai/model`. A stale label pointing at a recycled pod name would
  otherwise charge one model's load to another's variant.
- **Partner must resolve.** If the named pod is gone, treat it as an ordinary
  miss.

### 1.5 A launcher may host several models at once

FMA's `docs/launcher.md` is explicit that one launcher pod can run multiple vLLM
instances concurrently, "where clients can spin up different models on demand",
each an isolated subprocess with its own configuration. So "one launcher, one
variant" is not an assumption the design may make.

Two facts make this tractable:

- **We already key per engine, not per pod.** `buildInstanceKey` produces
  `podName + ":" + port`, and `instance` is in the grouping key of every
  collector query (`max by (model_name, instance, pod)`). Two engines in one
  launcher arrive as two distinct replica rows, with distinct ports, today —
  *provided both are scraped*, which under the PodMonitor in §4 they are not.
  The reasoning below is therefore about what attribution does with rows that
  reach it; §4 is where the rows go missing in the first place.
- **Every vLLM series carries `model_name`.** A row already declares which model
  it belongs to, independently of any pod-level label.

So the guard is a per-row model check, not a per-pod one:

> Accept the pairing hop for a row only when the **registry entry** of the
> ScaledObject the hop resolved to declares a `ModelID` equal to that row's
> `model_name`.

It must be the registry's `ModelID`, not a pod label. The obvious-looking version
of this guard — compare the requester's `llm-d.ai/model` to `model_name` —
compares two things that are never equal:

| | observed value |
| --- | --- |
| pod label `llm-d.ai/model` | `qwen-qwe-694d2b87-en3-0-6b` |
| series label `model_name` | `Qwen/Qwen3-0.6B` |

The label is a sanitized DNS-safe form of the model ID. A guard written that way
rejects every row, the hop attributes nothing, and phase 1 ships as a no-op that
looks implemented. `registry.Entry.Metadata` → `scalermeta.Meta.ModelID` is
documented as "the model this variant serves", comes from the KEDA trigger
metadata, and is the identity the rest of attribution already keys on — so the
comparison is apples to apples and costs no extra call.

It is also the more robust choice for a second reason: `llm-d.ai/model` on a
launcher is stamped from the `InferenceServerConfig`'s label map, which FMA
treats as arbitrary (its own examples use `model-reg` / `model-repo` /
`e2e-test.fma.llm-d.ai/isc-label`). Nothing guarantees an llm-d label convention
on an FMA pod. The registry does not depend on one.

Consequences, stated plainly:

- Several engines of the **same** model in one launcher — all rows accepted, all
  attributed to the same ScaledObject, and they stay distinct replicas for
  capacity and demand. Correct.
- Engines of a **different** model in the same launcher — rejected, counted, and
  *under-measured*. Not mis-attributed, which is the property that matters: a
  row is never charged to a variant it does not belong to.

The under-measurement is real and worth naming. The provider's `dual` label is
singular — one pod name — so it structurally cannot express a launcher bound to
several requesters. Only the requester side carries the per-instance identity
(`dual-pods.llm-d.ai/instance`, requester-only, an opaque ID such as
`Izmp_2DY2cq-OEWQ0emoeC5q5qW5GvmCpmVuUkTdGZNci` — not a port, so it does not join
to the Prometheus `instance` label).

Fully resolving a multi-model launcher therefore requires the reverse index:
LIST the namespace's pods carrying `dual-pods.llm-d.ai/dual`, keyed
launcher → [requesters]. That expresses the multiplicity the forward label
cannot. It costs a LIST per cycle, so it is **not** in phase 1; it is the
phase-3 upgrade, and it should be built only if a real deployment shows a
launcher serving two variants at once. Until then, the phase-1 behaviour —
attribute one model, count the rest, never lie — is the right trade.

Emit `wva_pod_mapping_miss_total{reason="foreign_model_instance"}` for the
rejected rows so the situation is visible rather than inferred from a demand
number that looks slightly low.

### 2. Sleeping launchers exclude themselves

FMA's invariant does most of the work: unbound ⇒ asleep, and unbound ⇒ no `dual`
label, so a sleeping launcher fails the pairing hop and is dropped — correct,
because it is serving nothing.

**That invariant must not be trusted on its own.** It is maintained by the
dual-pods controller, and a controller that is lagging, restarting or wedged can
leave a stale `dual` label on a provider that has already gone to sleep. Attribute
that pod and WVA gains a replica reporting `kv_cache_usage_perc 0`,
`num_requests_running 0` — a fake idle replica, which dilutes utilization and
**suppresses scale-up**. That is precisely the failure this proposal exists to
fix, reintroduced through its own fix, and it fails silently in the direction
that hurts.

Two guards, in increasing order of cost and authority:

1. **Reject when `dual-pods.llm-d.ai/sleeping == "true"`.** Provider-only, on the
   pod object already in hand, zero extra API calls. Catches every case where the
   controller updated one label and not the other.
2. **Require `vllm:engine_sleep_state{sleep_state="awake"} == 1`.** The engine's
   own report of its own state, owing nothing to the control plane. Costs one
   more collector query, and it is the only guard that survives a controller
   which has stopped updating labels altogether.

Take (1) in phase 1 — it is free. Take (2) if a real deployment ever shows a
stale binding, or preemptively if the extra query is judged cheap. Note (2) also
gives a direct answer to "is this replica actually serving", which no label can.

It should be dropped *legibly*, though. Add a second reason to
`wva_pod_mapping_miss_total` alongside `PodMappingMissUnresolved`:

```go
// PodMappingMissUnboundLauncher: an FMA provider pod with no current binding.
// Expected and benign -- an unbound provider is asleep and serving nothing --
// but distinguished from `unresolved` so a genuine attribution bug does not
// hide inside a pool of warm spares.
PodMappingMissUnboundLauncher = "unbound_launcher"
```

Identify it by `app.kubernetes.io/component=launcher` plus the absence of
`dual-pods.llm-d.ai/dual`. Getting this wrong is harmless; it only changes a
label on a diagnostic counter.

### 3. Replica semantics stay as they are

The number reported to KEDA/HPA as current replicas is the **scale target's**
replica count — the requester Deployment — exactly as today. Attributed metric
pods (bound launchers) feed the demand aggregates. In steady state the two are
equal, by FMA's own "each replica generates a distinct inference server
instance". They diverge only mid-binding, and briefly, in the direction that
under-reports supply — the safe direction.

`VariantCapacity.ReplicaCount` remains in scale-target units. Nothing changes.

**Unverified, and it must be verified before phase 1 lands.** The claim above
about the divergence being safe assumes supply is derived from the attributed
rows. If supply is instead derived from the scale target's replica count while
demand comes from attributed rows, the sign flips and the divergence becomes
dangerous: a requester pod is counted as supply the moment it exists, its
launcher is not yet bound so nothing is attributed to it, and the variant reads
as over-provisioned exactly while it is waiting for capacity — suppressing the
scale-up that would fix it. Read the supply path in the saturation analyzer and
settle this; do not carry the assumption into code.

### 4. Make the launchers scrapeable (the prerequisite)

Attribution is worthless if Prometheus never scrapes the launchers, and today it
frequently doesn't:

- A launcher declares **no container ports** and serves metrics on `:8000` (a
  decode pod uses `:8200`). A PodMonitor selecting its endpoint **by port name**
  resolves nothing and generates *no target at all* — not a failing target.
- llm-d-benchmark renders the correct form when a scenario sets `fma.enabled`:
  keep container `inference-server`, relabel `__address__` to `<podIP>:8000`.
  But that object is named `vllm-<model>`, so standing up a **non-FMA guide into
  the same namespace overwrites it** with the port-name version. The FMA stack
  keeps serving; its metrics silently stop being collected. That is precisely
  what happened on pokprod001 — FMA guide at 09:25Z, `workload-autoscaling` over
  the top at 09:26Z.

Ship an **opt-in** PodMonitor under a name nothing else claims
(`config/base/monitoring/fma-launcher-podmonitor.yaml`), relabelling to
`<podIP>:8000` and selecting on **both** `app.kubernetes.io/component=launcher`
and `dual-pods.llm-d.ai/launcher-config-name`. The first is a generic
`app.kubernetes.io` label that any workload may use; the second is FMA's own and
is present on every launcher pod observed. Selecting on the generic label alone
risks adopting an unrelated workload's pods and scraping them on a port that
means something else entirely.

**This pins the design to one engine per launcher pod, and that limit is real.**
A launcher running several vLLM instances puts them on several ports; a
`__address__` rewrite to a fixed `:8000` scrapes exactly one of them and the rest
are never collected. So the multiplicity §1.5 reasons about is lost at the scrape
layer *before* attribution can apply any guard — the under-measurement there is
total for the non-`:8000` engines, not partial. A PodMonitor cannot enumerate
ports that are assigned at instance-creation time, so this cannot be fixed on our
side. It is stated here as a known limit and pushed upstream below.

It must be opt-in, and the installer must refuse to apply it when another
PodMonitor already selects launcher pods. Two scrape configs on one pod produce
two targets at the same `(instance, pod)` key, and the collector's additive
queries — `sum by (model_name, instance, pod) (rate(...))` for dispatch rate and
generation-token rate — would **double-count**. The `max by` queries would not,
which makes the failure asymmetric and hard to spot: capacity looks right while
throughput reads 2×.

Preflight, in `deploy/lib`: list PodMonitors in the namespace, resolve each
selector against launcher pods, and apply ours only if none match.

### 5. Target selection at discovery time

`deploy/lib/scaledobject.sh` already detects the requester via
`so_fma_requester`. Make the rule explicit:

| namespace shape | target | note |
| --- | --- | --- |
| modelservice only (`decode`/`prefill`) | that Deployment | unchanged |
| requester only | the **requester** Deployment | FMA path; requires §4 in place |
| both | the modelservice Deployment | warn: launcher traffic is unmeasured |

The third row is a misconfigured namespace, not a supported topology. It is what
pokprod001 has, and it is why the earlier "retarget to the requester" change was
reverted — correctly, for that namespace. With §1 and §4 in place the second row
becomes genuinely supported, which it is not today.

When the FMA path is selected but §4 is not satisfied, emit the plan entry with
**`apply: no` and the reason**, rather than either applying it or omitting it.

Applying it is wrong: a ScaledObject that cannot measure its workload is worse
than no ScaledObject, because it holds the workload at `minReplicaCount` while
reporting healthy. But omitting it is also wrong, and that was the earlier
draft's mistake — it turns a monitoring misconfiguration into a silent absence
the operator has to diagnose from nothing, and it makes a fixable scrape problem
look like discovery failing to find the workload at all. `apply: no` shows both
the workload and what to fix, which is what the plan format exists for.

### 6. GPU accounting

`dual-pods.llm-d.ai/accelerators` carries GPU UUIDs on both halves of a bound
pair, so `internal/gpuusage` can charge a variant the GPUs actually behind it
instead of inferring from replica count.

Unbound launchers hold real GPUs too — that is what makes FMA fast — but they
belong to no variant. Report them as a namespace-level pool reservation, never
as variant demand. Charging warm spares to a variant would make every FMA
namespace look permanently over-provisioned and would fight the limiter, which
is advisory anyway (see `docs/`, WVA limiter notes).

## What does not change

The non-FMA path executes the identical code with the identical API calls. The
pairing hop is unreachable without the label; the new PodMonitor is opt-in; the
new miss reason cannot be emitted without a launcher pod. A regression here
would have to come from the guards, which is what the tests below are for.

## Observability

- `wva_pod_mapping_miss_total{reason="unbound_launcher"}` — warm spares, benign.
- `wva_pod_mapping_miss_total{reason="unresolved"}` — should go to zero in an
  FMA namespace once this lands. It is the acceptance signal.
- One `Info` log per cycle when the pairing hop fires at least once, naming the
  count and one example pair. Silence in an FMA namespace means the hop is not
  working.

## Testing

Unit, in `internal/collector/locator`:

- launcher with `dual` → resolves to the requester's ScaledObject
- launcher with `dual` naming a **nonexistent** pod → miss, no error, no panic
- two pods naming **each other** → one hop, no loop
- partner in a **different namespace** → not followed
- partner with a **different** `llm-d.ai/model` → rejected
- launcher **without** `dual` → `unbound_launcher`
- ordinary Deployment pod → byte-identical to today, one API call

Fixture, in `test/e2e/fixtures/model_service_builder.go`: an FMA layout — a
requester Deployment under a ScaledObject, plus pods owned by a fake
`LauncherConfig` carrying the pairing labels. No FMA controller required; the
labels are the whole contract.

End-to-end, on a real FMA install: drive load, then assert
`wva_desired_replicas` moves and `unresolved` misses stay at zero.

## Phases

0. **Settle the supply-path question in §3** before writing any of it. It is a
   read, not a change, and it decides whether the mid-binding divergence is safe
   or actively harmful.
1. **Locator pairing hop + tests.** Self-contained. Makes FMA measurable
   anywhere the launchers are already scraped correctly. Includes the
   registry-`ModelID` guard (§1.5) and the `sleeping` label guard (§2) — both
   are part of phase 1, not follow-ups; without them the hop either attributes
   nothing or attributes sleeping pods.
2. **Scrape enablement.** PodMonitor, preflight, docs.
3. **Discovery target rules.** Requester-only namespaces become supported.
4. **GPU accounting** via `accelerators`.

Phase 1 is worth landing alone: it is ~40 lines behind a guard that cannot fire
without FMA.

## Upstream asks for FMA

Worth filing, none blocking:

1. Declare a named `metrics` container port on launcher pods, so the standard
   llm-d PodMonitor works without relabelling.
2. Give the FMA PodMonitor a name that a non-FMA guide cannot overwrite.
3. Document vLLM scraping in `docs/metrics.md`, which today covers only FMA
   controller metrics.
4. Stamp the bound instance's **admin/serving port** onto the requester pod next
   to `dual-pods.llm-d.ai/instance`. That single addition turns the
   engine → requester mapping into a pure metadata join and would let any
   observer attribute a multi-model launcher correctly, without calling the
   launcher's CRUD API.
5. **Expose one aggregated `/metrics` on the launcher covering every instance it
   hosts, with an instance-identifying label on each series.** This is the ask
   that actually unblocks multi-instance launchers, and it subsumes (4). Today
   each instance serves its own metrics on its own dynamically-assigned port, and
   no `PodMonitor` can enumerate those — so a multi-instance launcher is
   unobservable by any Prometheus-based consumer, not just by WVA. A single
   endpoint on a known port, with `instance_id` on the series, would make the
   whole class of tools work.

## Open questions

- **Mapping a specific engine to a specific requester**, in a launcher hosting
  several. §1.5 attributes at most one model per launcher pod; the rest are
  under-measured. Closing it needs a port → instance-ID → requester join that no
  label currently provides. The launcher's own CRUD API
  (`GET /v2/vllm/instances`) knows the mapping, but calling it from WVA means
  talking to a workload pod, which the collector deliberately does not do.
  A cheaper option: have FMA stamp the admin port onto the requester alongside
  `dual-pods.llm-d.ai/instance`, which would make the join pure metadata. This
  is an upstream ask, listed below.
- **Does the `dual` label survive a launcher restart** before rebinding? If the
  label lands after the container is ready, there is a window where a live
  engine is unattributed. Harmless (it under-reports supply) but worth measuring.
- **Does the `dual` label survive a launcher restart** before rebinding? If the
  label lands after the container is ready, there is a window where a live
  engine is unattributed. Harmless (it under-reports supply) but worth measuring.
