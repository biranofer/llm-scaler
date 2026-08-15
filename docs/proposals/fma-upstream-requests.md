# Requests to Fast Model Actuation

Findings and change requests for
[`llm-d-incubation/llm-d-fast-model-actuation`](https://github.com/llm-d-incubation/llm-d-fast-model-actuation),
gathered while making the Workload Variant Autoscaler work with FMA. Everything
below was measured on a live OpenShift cluster (pokprod001, 2026-08-14) running
FMA `v0.6.0-alpha.13` with `llm-d-benchmark`'s `workload-autoscaling` guide.

Only item 1 is a defect. The rest are observability and accounting gaps that any
GPU-aware controller hits, not just WVA — a plain HPA on custom metrics, a KEDA
scaler, and a Grafana dashboard all run into the same three walls.

## Summary

| # | request | severity | affects |
| --- | --- | --- | --- |
| 1 | Warm instances hold GPUs invisibly to the API server | **high** | quota, scheduling, any GPU accounting |
| 2 | `sleeping` label contradicts the launcher's own instance list | medium | anything reading liveness from labels |
| 3 | Multi-instance launchers are unscrapeable | medium | all Prometheus consumers |
| 4 | Binding metadata is dropped on unbind | medium | accounting, post-hoc analysis |
| 5 | The warm pool has no numeric, scalable control surface | low | autoscaling the pool |
| 6 | `docs/metrics.md` does not cover vLLM scraping | low | operators |

---

## 1. Warm instances hold GPUs that Kubernetes cannot see

**Severity: high.** This is the one worth fixing first.

Measured with **every requester Deployment at `replicas: 0`**:

| | |
| --- | --- |
| GPU requests charged in the namespace | 1 (an unrelated decode pod) |
| launcher pods requesting any GPU | 0 of 14 |
| launchers running a vLLM instance | **9** |
| distinct physical GPUs those instances hold | **9** |

Each of the nine reports its own GPU UUID through
`GET :8001/v2/vllm/instances` — for example
`gpu_uuids: ["GPU-ae2da921-35dd-b643-00be-4d8788681bd5"]`,
`CUDA_VISIBLE_DEVICES: 3` — while requesting no `nvidia.com/gpu` at all.

We understand why: the requester reserves the GPU and the launcher binds onto it,
and requesting on both halves would double-book N launchers plus N requesters on
an N-GPU node. The `LauncherConfig` pod template declares `nvidia.com/gpu: 1`
and the populator strips it at pod creation, so this is deliberate.

The consequence is that **a resident instance survives unbinding**, and once
unbound its GPU is charged to nobody:

- A `ResourceQuota` on `requests.nvidia.com/gpu` counts requester pods only. It
  cannot express a warm pool, so GPU quota does not bound FMA's real consumption
  and tenant fairness built on it is defeated.
- The scheduler treats those GPUs as allocatable. Another workload may be placed
  on one, take the memory a sleeping instance released, and contend with it when
  FMA wakes it. (Not currently realised on our cluster — every node hosting a
  resident launcher still has headroom — but nothing prevents it.)
- No controller can compute "GPUs actually in use" from the API server.

**Request.** Make warm occupancy visible to the API server. Either:

- **(a)** have launchers request the accelerators their resident instances hold —
  an *extended resource* (e.g. `fma.llm-d.ai/warm-gpu`) would avoid
  double-booking against the requester's real request while still being
  quota-able and visible in `kubectl describe node`; or
- **(b)** at minimum, keep the binding annotations (item 4) for as long as an
  instance is **resident**, not only while it is bound, so occupancy is
  derivable from pod metadata.

(a) is strictly better: it lets cluster administrators bound the warm pool with
the mechanism they already use, and it makes the pool visible to the scheduler.

## 2. The `sleeping` label contradicts the launcher's own instance list

**Severity: medium.** Two pods, sampled at the same moment:

```
pod labelled dual-pods.llm-d.ai/sleeping=false
  GET :8001/v2/vllm/instances → {"total_instances":0,"running_instances":0,"instances":[]}

pod labelled dual-pods.llm-d.ai/sleeping=true
  GET :8001/v2/vllm/instances → {"total_instances":1,"running_instances":1,
       "instances":[{"status":"running","options":"… --port 8000",
                     "gpu_uuids":["GPU-ae2da921-…"]}]}
```

The pod claiming to be awake hosts nothing; the pod claiming to be asleep hosts a
running instance. We also observed `sleeping=true` on a launcher immediately
after a successful bind.

We read the label as describing *the sleep state of the instances*, which makes
it report `false` both when an instance is awake and when there is no instance at
all — the two states a scheduler or autoscaler most needs to distinguish. Also
observed: 5 pods labelled `sleeping=false` with 0 requester replicas in the
namespace, so nothing could have been bound to them.

**Request.** Either document that `sleeping` is not a liveness signal and must
not be read as one, or split it into two labels — "has a resident instance" and
"that instance is awake". Consumers currently have to fall back to
`vllm:engine_sleep_state`, which requires scraping, which requires item 3.

Note `docs/dual-pods.md` states "An unbound server-providing Pod is asleep… wake
up ASAP after binding, go to sleep just before unbinding." Item 1 shows unbound
pods keeping resident instances, so the documented invariant and the observed
behaviour differ; it would help to know which is intended.

## 3. A launcher hosting several instances cannot be scraped

**Severity: medium.** `docs/launcher.md` says one launcher can run multiple vLLM
instances concurrently, "where clients can spin up different models on demand".
Each instance gets its own port — the instance record carries it twice, as
`--port 8000` in the options and as `annotations.inference-port` — and the
`InferenceServerConfig` declares a single `modelServerConfig.port`, so a second
concurrent instance cannot reuse it.

A `PodMonitor` generates targets from **pod metadata**, and a pod carries exactly
one `dual-pods.llm-d.ai/server-port` annotation. So Prometheus can reach at most
one instance per launcher, and the rest are invisible — completely, not
partially, and silently, because the one instance that *is* scraped looks
healthy.

`llm-d-benchmark`'s FMA PodMonitor works around this by rewriting `__address__`
to `<podIP>:8000`, which has the same one-instance ceiling and additionally
hardcodes the port.

**Request.** Expose **one aggregated `/metrics` endpoint on the launcher**,
covering every instance it hosts, with an instance-identifying label on each
series (`instance_id`, matching `dual-pods.llm-d.ai/instance-id`). A single
endpoint on a known port would let every Prometheus-based consumer work
unmodified. Failing that, one annotation per instance would at least make the
ports discoverable.

## 4. Binding metadata is dropped when the pair unbinds

**Severity: medium.** A **bound** launcher carries a rich set of annotations:

```
dual-pods.llm-d.ai/requester    = <uid> fma-requester-…-zhlrl
dual-pods.llm-d.ai/server-port  = 8000
dual-pods.llm-d.ai/instance-id  = IOOesGI4Otm…
dual-pods.llm-d.ai/vllm-config  = {"options":"… --port 8000","gpu_uuids":["GPU-712a7368-…"], …}
dual-pods.llm-d.ai/isc-label-keys = component llm-d.ai/guide llm-d.ai/inferenceServing llm-d.ai/model
```

An **unbound** launcher that is still running an instance carries only
`launcher-config-hash` and `launcher-populator-template-hash`. Cluster-wide, no
pod carried a `vllm-config` annotation at a moment when nine GPUs were held.

This is what makes item 1 unfixable from outside: the information exists, FMA
already computes it, and it is discarded exactly when it becomes the only record
of what is consuming an accelerator.

**Request.** Keep `vllm-config` (or at least `accelerators` and `server-port`) on
the provider pod for as long as an instance is **resident**, independently of
binding. `accelerators` is already documented as maintained on both pods; the
observed behaviour ties it to the binding instead.

## 5. The warm pool has no numeric, scalable control surface

**Severity: low**, and dependent on item 1.

`LauncherPopulationPolicy.spec.countForLauncher[].launcherCount` is per *matching
node*, so "keep N warm instances" must be expressed as
`N ≈ nodes × launcherCount × maxInstances` and re-derived whenever the node set
changes. None of `launcherconfigs`, `launcherpopulationpolicies` or
`inferenceserverconfigs` exposes a `scale` subresource — all three carry `status`
only — so KEDA, an HPA, or anything else declarative cannot drive the pool. And
both `status` blocks contain only `observedGeneration`: no resident, bound or
free instance counts.

Confirmed the populator holds a declared count rather than growing with demand:
14 nodes match the selector, `launcherCount` is 1, and there are exactly 14
launcher pods.

**Request**, in dependency order:

1. Report pool state in `status` — resident, bound and free instance counts — so
   a controller can read it instead of polling pod APIs.
2. Offer a **total-count** warm-pool field rather than a per-node one.
3. Give it a `scale` subresource, so the pool can be driven by the same machinery
   as any other workload.

Without item 1, none of this is safe: growing the pool would consume GPUs that no
quota accounts for.

## 6. `docs/metrics.md` does not cover vLLM scraping

**Severity: low.** The document covers FMA controller metrics. It does not say
how a launcher's vLLM instances are exposed, on what port, or what
`PodMonitor`/`ServiceMonitor` shape is expected — which is the first thing an
operator needs, and the gap that produced items 3 and 4 above.

A short section stating the port convention, the recommended scrape config, and
the sleeping-instance caveat would save each consumer rediscovering it.

---

## 7. A bound launcher joins the InferencePool before its instance can serve

**Severity: high.** This one costs requests.

A launcher's pod readiness reflects the **launcher process**, not the vLLM
instance inside it. The pod has been `Ready` for as long as it has been running —
26 hours, in the deployment measured. FMA stamps `llm-d.ai/inferenceServing=true`
on it at **bind** time, and the `InferencePool` selector is labels only
(`{llm-d.ai/inferenceServing: "true", llm-d.ai/model: …}`); readiness cannot be
expressed in a label selector. So the moment a pair binds, a pod that looks
healthy in every respect enters the pool while its instance is still waking from
sleep — and EPP dispatches to it.

Measured on pokprod during a 594-second benchmark at 10 req/s:

| minute | ok | errors |
| --- | --- | --- |
| 12:53 | 336 | 0 |
| 12:54 | 626 | 0 |
| **12:55** | **217** | **324** |
| 12:56 | 552 | 0 |
| … | … | 0 |

All 324 were `503 Service Unavailable` from the EPP route, confined to the single
minute in which a bind occurred; every other minute of the run was clean.

The control is what makes it conclusive: **the same benchmark without FMA scaled
1 → 4 replicas and logged zero errors.** A starting *decode* pod never breaks
anything, because that pod carries a `readinessProbe` on `/v1/models` and stays
`NotReady` until its engine answers. Only the launcher path fails, because
nothing ties its pod readiness to the instance it is hosting.

**The probe exists and points at the wrong service.** A launcher already has a
`readinessProbe` — it just does not measure what pool membership needs:

```
launcher readinessProbe   ->  :8001/v2/vllm/instances    the launcher's CRUD API
EPP dials                 ->  :8000                      the pool's targetPort
```

Port 8001 is the process manager. It answers `200` whenever the launcher itself
is up — including with `total_instances: 0`, with the instance asleep, and while
one is still starting — so `Ready` is true for the entire life of the pod and
says nothing about whether anything can serve on 8000.

Contrast the decode pod, which never produced one of these errors:

```
decode readinessProbe     ->  /v1/models                 the engine itself
decode startupProbe       ->  /v1/models
```

Same cluster, same EPP, same InferencePool mechanics. The difference is only
which endpoint the probe asks.

**A consumer cannot work around it.** `LauncherConfig.spec.podTemplate` is a full
pod template and the CRD schema carries `readinessProbe`, so the obvious
workaround is to set the right one there. It does not work: the controller
overwrites it.

Tested on this cluster with a separate `LauncherConfig` — a copy of the live one
with `readinessProbe: /v1/models` on port 8000, and a population policy pinning a
single launcher to one node:

```
specified in podTemplate:  /v1/models          on 8000
probe on the created pod:  /v2/vllm/instances  on 8001
```

So the default is not a fallback for an unset field, it is applied
unconditionally, and every consumer inherits it. That is why this needs fixing
upstream rather than in each caller's config.

**Nor can EPP be configured around it today.** The EPP plugin config is ours to
write (`wva-plugins.yaml`), so the second obvious workaround is a plugin that
excludes an endpoint which cannot serve. The configured profile has none:

```yaml
plugins:
  - type: queue-scorer
  - type: kv-cache-utilization-scorer
  - type: prefix-cache-scorer
```

Three **scorers** and no **filter**. Scorers rank candidates; they do not remove
them, so every pool member stays eligible and a sleeping launcher is simply
scored on absent data. llm-d-benchmark's own FMA scenarios —
`ocp-wva-fma-hotstart` and `ocp-wva-fma-warmstart` — carry the same three plus
`no-hit-lru-scorer`, still all scorers. Nothing shipped anywhere treats a
launcher differently from a model server.

If GAIE offers a filter that drops endpoints failing metrics collection, adding
it here would mitigate this without an FMA change; that is worth checking before
assuming the probe fix is the only route.

**Request** — the first is a one-line change and closes it outright:

1. Point the launcher's `readinessProbe` at the bound instance: `/v1/models` on
   the `dual-pods.llm-d.ai/server-port` port, not `/v2/vllm/instances` on 8001.
   `Ready` then means servable, and the readiness filtering that already protects
   the decode path protects launchers too — nothing downstream has to change.
2. Or do not apply the serving labels until the instance is awake, so pool
   membership implies servability, which is what a label-only selector assumes.
3. Or publish the instance's readiness on the pod for consumers to gate on. The
   `sleeping` label is the closest thing today, and item 2 above shows it
   disagrees with the launcher's own instance list, so it cannot carry this.

Until then, every bind is a window in which a share of traffic 503s, and the
window scales with how often the autoscaler moves — which makes autoscaling an
FMA stack cost errors in proportion to how well it works.

---

## What we confirmed works well

Worth saying, since the above is a list of problems:

- **The pairing labels are exactly the right design.**
  `dual-pods.llm-d.ai/dual` on both halves, each naming the other, let an
  external controller resolve a launcher to the workload that governs it with a
  single lookup and **no dependency on the `fma.llm-d.ai` API group** — no
  client, no informer, no RBAC, no CRD-presence check. That is what lets FMA be
  installed long after another controller and be picked up automatically. WVA's
  entire integration is built on it.
- **Binding is genuinely fast.** A requester scaled `0 → 1` was bound and
  labelled in **under 5 seconds**.
- **The GPU handoff is coherent.** The requester's
  `dual-pods.llm-d.ai/accelerators` and the launcher's `vllm-config.gpu_uuids`
  named the same GPU (`GPU-712a7368-…`) on the pair we bound.

## How these were measured

All observations come from read-only queries plus one bounded experiment: a
requester Deployment scaled `0 → 1`, both pods dumped, then scaled back to `0`.
Instance state came from `GET :8001/v2/vllm/instances` on the launcher, engine
state from `GET :8000/metrics`, and scrape-target state from `up{namespace=…}` in
the cluster's Thanos.

Context for these requests, including how WVA resolves the pairing:
`docs/proposals/fma-aware-attribution.md`.
