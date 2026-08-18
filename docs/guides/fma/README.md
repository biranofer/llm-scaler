# Autoscale a Fast Model Actuation stack

## Overview

This guide makes WVA size a model served by
[Fast Model Actuation](https://github.com/llm-d-incubation/llm-d-fast-model-actuation).
FMA runs the engine in a pod no ScaledObject owns, so without the two steps below
WVA measures nothing and holds the workload at `minReplicaCount` while reporting
healthy.

Everything here is additive to the ordinary install: if your namespace has no
launcher pods, none of it applies and nothing changes.

**What FMA does differently.** It splits a model server across two pods:

| | scalable | runs the engine | reports metrics |
| --- | --- | --- | --- |
| **requester** Deployment | yes | no | no |
| **launcher** pods | no — owned by a `LauncherConfig` | yes | yes |

A dual-pods controller binds them and stamps `dual-pods.llm-d.ai/dual` on both
halves, each naming the other, plus the llm-d serving labels onto the bound
launcher so it joins the InferencePool. Binding takes under five seconds — that
speed is the point of FMA.

WVA follows that pairing: when a pod's `ownerReferences` reach no scale target and
it declares a partner, WVA resolves the partner instead, so a launcher's metrics
are attributed to the requester's ScaledObject. Design detail:
[FMA-aware attribution](../../proposals/fma-aware-attribution.md).

## Prerequisites

- WVA installed ([in a namespace](../install-in-namespace/) or
  [cluster-wide](../install-cluster-wide/))
- an FMA stack in the workload namespace: a `LauncherConfig`, a
  `LauncherPopulationPolicy`, an `InferenceServerConfig`, and a requester
  Deployment
- a Prometheus scraping that namespace

```bash
source docs/guides/env.sh
export NAMESPACE=<namespace-with-launchers>
```

```bash
# FMA present?
kubectl get launcherconfig,launcherpopulationpolicy,inferenceserverconfig -n ${NAMESPACE}
kubectl get pods -n ${NAMESPACE} -l app.kubernetes.io/component=launcher
```

## Installation Instructions

### 1. Make the launchers scrapeable

**Do this first.** Attribution has nothing to work with until the launchers are
scraped, and by default they are not: a launcher declares **no container ports**
and serves on `:8000`, so a PodMonitor selecting its endpoint by port *name*
resolves nothing and generates **no target at all** — not a failing target, no
target, and no error anywhere.

```bash
kubectl apply -k config/fma-launcher-metrics -n ${NAMESPACE}
```

Or at install time:

```bash
WVA_FMA_LAUNCHER_METRICS=true make deploy-wva
```

It builds the scrape address from `dual-pods.llm-d.ai/server-port`, the port FMA
records on the pod, so it follows FMA rather than hardcoding one — and it skips
launchers with no bound instance, which carry no such annotation.

> The installer **refuses** to apply this alongside another PodMonitor that
> already scrapes launchers. Two scrape configs on one pod give it two targets
> under the same `(instance, pod)` key, and WVA's additive queries would
> double-count throughput while capacity still looked correct.

#### Why EPP already sees what Prometheus cannot

A fair question at this point: EPP routes to these launchers perfectly well, so
why does WVA need its own PodMonitor? Because the two find a pod by different
means, and only one of them needs a declared port.

| | how it finds the launcher | needs a declared container port? |
| --- | --- | --- |
| **EPP** (InferencePool) | label selector, then the **pool's** `targetPort` | no — the port is a property of the pool |
| **PodMonitor** by port `name` **or** `portNumber` | label selector, then a port **declared on the pod** | yes — and a launcher declares none |
| **PodMonitor** building `__address__` (what we ship) | label selector, then podIP + the `server-port` annotation | no |

`portNumber` looks like it should be the easy answer — an integer, no name required —
and it is not. Measured on kind, both PodMonitors selecting the same portless pod
in the same Prometheus: the `portNumber` job **is** generated and then relabelled
away, landing in `droppedTargets`, because `__meta_kubernetes_pod_container_port_number`
never matches a port the pod never declared. The address-building job scraped it
`health=up`. Constructing the address is not a workaround here; it is the only
mechanism that reaches these pods.

FMA stamps `llm-d.ai/model`, `llm-d.ai/inferenceServing=true`, the
`dual-pods.llm-d.ai/dual` pair label and the `server-port` annotation onto a
launcher **at bind time**, and removes them at unbind. Those labels are what put
it in an InferencePool, so EPP picks it up with no port information from the pod
at all. A port-name PodMonitor gets nothing from the same pod, which is why
launcher metrics are missing by default and why this file exists.

Measured on a shared cluster: **163 InferencePools, every one of them
`targetPort: 8000`** — and 48 of them select on `llm-d.ai/model`.

**EPP cannot follow a port change, and nothing updates it.** `targetPorts` is a
field on the InferencePool — the pool spec has only `appProtocol`,
`endpointPickerRef`, `selector` and `targetPorts`, with no per-endpoint form —
written by the llm-d router chart, while FMA's controllers only patch pod labels.
Yet the port is declared *per model*: the `InferenceServerConfig` carries both
the model label and `modelServerConfig.port` in the same object, so a rebind to a
different model brings a different ISC and may bring a different port. They agree
today because everything uses 8000, by convention rather than by construction. If
they ever diverge, the launcher joins the pool by label and EPP dials a port
nothing is listening on — healthy by selector, unreachable in fact.

This is where the WVA scrape path is the more robust of the two: the
`server-port` annotation is per-pod and FMA rewrites it at bind time, so the
PodMonitor follows a rebind that EPP cannot.

The ports are a property of the **pool**, not of the pod. `targetPorts` is a list
of up to 8, and the v1 API says every entry "will be treated as a distinctive
endpoint by EPP, addressable as a `podIP:portNumber` combination" — so a launcher
hosting several instances *can* be fully routable, provided the pool declares
each port. Nothing on the pod says which ports it serves; the pool does.

In practice **every one of the 163 pools measured declares exactly one port**, so
today EPP reaches one server per launcher pod. That is a property of how these
pools are configured, not a limit of the contract — see
[One launcher, several models](#one-launcher-several-models).

### 2. Register the workload

```bash
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```

What the plan writes depends on what the namespace holds:

| namespace holds | the plan writes | default |
| --- | --- | --- |
| a `decode`/`prefill` Deployment only | one entry, that Deployment | `apply: yes` |
| an FMA **requester** only | one entry, the requester | `apply: yes` |
| **both** | **two entries** | the FMA half is `apply: no` |

```yaml
plan:

  # note: FMA variant: the requester is the scale target, and its engine metrics
  # come from launcher pods scraped by fma-launcher-metrics. WVA follows the
  # dual-pods pairing to attribute them.
  - apply: "yes"
    namespace: llm-d-sim
    kind: Deployment
    name: fma-requester-dev-model   # the requester, not a decode Deployment
    modelID: "meta/llama"           # read from the InferenceServerConfig
    minReplicas: 1
    maxReplicas: 3                  # capped at the launcher pods present
    variantCost: "10.0"
    # inferencePool: (none)         # the pool selects the LAUNCHER, not the requester
```

If step 1 was skipped the entry arrives as `apply: no`, carrying the command that
fixes it. That is deliberate: a requester runs no engine, so every metric for the
variant comes from a launcher, and a ScaledObject that cannot measure its
workload is worse than none.

### 3. Size `maxReplicas` from the launcher pool

**FMA does not cap scaling — the pool does.** A modelservice Deployment in the
same namespace still scales to its own `maxReplicas` as usual.

A requester replica becomes capacity only if a launcher can bind to it *and* a GPU
is free. Past that, extra requester pods sit `Pending` — and pending replicas
count toward *anticipated supply*, so they suppress the very scale-up they were
meant to provide.

The dependency runs one way only: **requesters are bounded by launchers, never
the reverse.** The pool is a declared count, not a response to demand —
`LauncherPopulationPolicy.spec.countForLauncher[].launcherCount` is per matching
*node*, so the pool size is `nodes × launcherCount` and it does not grow when
requesters do. Confirmed on a live cluster: 14 nodes match, `launcherCount` is 1,
and there are exactly 14 launcher pods — sitting there with the requester
Deployment at `replicas: 0`. Launchers exist with no requesters at all.

```
ceiling = min( launcher pods,        # one ROUTABLE instance each, see below
               Σ_nodes free GPUs )   # accelerators
```

**Not `launcherCount × maxInstances`.** `maxInstances` is FMA's per-pod ceiling —
4 in the deployment measured — but a second instance on the same pod is only
useful if the InferencePool declares its port, and every pool measured declares
one. So `maxInstances` is not the binding constraint and sizing against it
overstates the ceiling by that factor: the 15th replica in a 14-launcher
namespace has nowhere to go, and requester pods past the ceiling sit `Pending`
while counting toward *anticipated supply*, suppressing the very scale-up they
were meant to provide.

The plan already caps the entry at the launcher pods present, which is the first
term. The second is not visible from discovery, and in an FMA namespace the GPU
picture is itself a lower bound ([why](#gpu-accounting-is-a-lower-bound)).

Raise the first term by having the pool declare the other ports, not by raising
`maxReplicas` — see [One launcher, several models](#one-launcher-several-models).

## Verification

### 1. The launchers are scrape targets

```bash
kubectl get podmonitor -n ${NAMESPACE}
```

A bound launcher should appear in `up{}` within about 30 seconds of binding.
Unbound launchers should **not** appear at all — that is the `keep` rule working,
not a fault.

### 2. WVA is attributing them

```bash
kubectl logs -n ${WVA_NAMESPACE} -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200 \
  | grep "Attributed FMA launcher pods"
```

```
Attributed FMA launcher pods through their dual-pods pairing  {"count": 4, "example": "launcher-… -> fma-requester-…"}
```

Silence here with launchers running means the hop is not resolving — check step 1.

### 3. Demand reflects the launchers

```bash
kubectl logs -n ${WVA_NAMESPACE} -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200 \
  | grep analyzer-result | tail -1
```

```
analyzer-result  supply: 2137292  demand: 961383  util: 0.45
```

Before launcher attribution, the same namespace under the same load reported
`supply: 550758, demand: 0, util: 0` — the decode pod alone — while four launchers
ran at 155, 136, 56 and 44 concurrent requests.

### 4. Nothing is being dropped that should not be

```bash
kubectl logs -n ${WVA_NAMESPACE} -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200 \
  | grep "Skipping pod"
```

| reason | meaning |
| --- | --- |
| `unbound_launcher` | a warm spare — expected, and rare, since spares are not scraped |
| `pairing_unresolved` | a declared pairing that led nowhere — **investigate** |
| `unresolved` | should be zero in an FMA namespace |

## Benchmarking an FMA stack

### Run this after every standup

```bash
make benchmark-fma-fixups BENCHMARK_NAMESPACE=<ns>
```

`benchmark-standup` calls it automatically when FMA is present. Run it by hand
after any standup done another way. It is idempotent and will not restart
healthy launchers, so it never discards warm instances.

It repairs two defects the standup reintroduces every time. Neither produces bad
numbers — together they produce **no** numbers, and the failure looks like a
load-generator crash rather than an FMA misconfiguration:

```
launcher SA cannot patch pods (403, retried every 5s forever)
  -> a launcher whose requester was deleted keeps llm-d.ai/inferenceServing=true
  -> that dead endpoint stays in the InferencePool
  -> EPP dispatches to it; ~20% of requests return 503
  -> guidellm validates its backend ONCE before generating load, and dies
       RuntimeError: Worker process group startup failed: error_event is set
  -> no results.json; every metric in the table reads "?"
```

1. **Launcher RBAC.** The `state-change-reflector` sidecar patches labels onto
   its own pod, but the standup gives it the namespace `default` ServiceAccount,
   which cannot patch pods. Check with
   `kubectl auth can-i patch pods --as=system:serviceaccount:<ns>:default -n <ns>`.
2. **Controller version.** The standup pins `v0.6.0-alpha.13`, which drops a
   fresh reconcile notification when the item is already queued with a future
   `processAfter` from a rate-limited retry (upstream #696). Fix 1's 403s
   generate exactly those retries, so the unbind that would clear the stale label
   is swallowed. `FMA_VERSION` defaults to `v0.6.4`.

Both are namespaced: the FMA controllers run with `--namespace=<ns>` and a
namespaced Role. The CRDs are the only cluster-scoped surface and are not
touched.

### Measure actuation, not tokens

FMA's claim is that capacity *arrives sooner*, not that tokens are faster. Test
that directly:

```bash
make benchmark-actuation BENCHMARK_NAMESPACE=<ns> \
     ACTUATION_TARGET=<deployment> ACTUATION_TRIALS=5
```

Scale up, time each replica from creation to Ready, scale back, repeat. No load
generator, no router, no Prometheus — so none of the failures above can affect
it, and it runs in minutes. Baseline on pokprod, both variants: **median 90s**,
0 of 6 woken. A working wake is **2-3s**, so the two are unmistakable.

### Warm pools are not what they look like

A sleeping instance is reusable **only by a requester handed the same GPU** —
the instance ID is a hash that includes the GPU UUIDs. Two consequences:

- Sleepers the **launcher-populator** creates are keyed to GPUs no requester will
  reserve, and are never woken. One sat idle through every test and a requester
  that bound to it still paid 494s.
- Sleepers created by scaling requesters **up then down** are keyed to GPUs the
  scheduler actually hands out, and those hit (2s, and 3s on a repeat).

So warm capacity is a by-product of real allocations, not something that can be
provisioned. And it is fragile: the populator deletes any launcher above
`launcherCount` **per node** as an "excess launcher pod", warm instance included,
about 20s after a scale-down. Durable warm capacity is `launcherCount x nodes`.

Check before trusting a run — a pool that is not warm makes every scale-up cost
~50-80s and the benchmark will silently measure the cold path:

```bash
WARM_POOL_NS=<ns> bash hack/benchmark/warm_pool.sh verify [n]   # gate
WARM_POOL_NS=<ns> bash hack/benchmark/warm_pool.sh report       # sleepers + their GPUs
WARM_POOL_NS=<ns> bash hack/benchmark/warm_pool.sh size <n>     # durable capacity, in REPLICAS
```

`postprocess.py` reports **Warm binds (woken)**, **Cold builds (rebuilt)** and
**Median replica start (s)** so a result carries that distinction rather than
leaving it to inference. `?` means not measured, never zero.

### Placement decides warm or cold

Binding is **node-local**. A requester that lands on a node holding a sleeper
wakes it in ~3s; one that lands anywhere else rebuilds from scratch in ~50-90s.
Warming the pool is therefore only half the job — the other half is arranging
for the requester to be scheduled beside it. Two modes:

| | `fma.warmAffinity` (ours) | `fma.launcherNodeSelection` (upstream) |
|---|---|---|
| How | Launchers spread over every eligible GPU node; the requester carries a **preferred** `podAffinity` toward nodes holding a sleeper | step_06 picks **one** node, labels it, and hard-pins launchers and requesters to it with a `nodeSelector` |
| Pool size | Any number of nodes; sized in replicas via `warm_pool.sh` | Capped at one node's free GPUs, read once at standup |
| Growing it | Raise the replica count | Relabel — but the next standup strips the label from every node but its own pick |
| Cluster full | Schedules anyway and rebuilds cold | Standup **fails**: "All GPU nodes are currently occupied" |
| Right for | A shared cluster (pokprod) | A dedicated benchmark cluster |

`warmAffinity` is applied to the gitignored clone by `hack/benchmark/patch_harness.sh`
(fix 3), so it survives the re-checkout that `benchmark-install` performs. It
renders two weighted preferred terms: weight 100 toward
`dual-pods.llm-d.ai/sleeping=true` (a wakeable sleeper is here) and weight 50
toward `app.kubernetes.io/component=launcher` (covers the window before anything
has slept, where the first term scores every node zero). Preferred and never
required — with the warm set exhausted the pod still schedules and rebuilds,
which is what it would have done anyway; a hard predicate would leave it
`Pending` instead, trading a slow replica for no replica.

**Why the default is worse than random.** On pokprod there are 14 GPU nodes, all
one accelerator type, and the LauncherPopulationPolicy pins launchers to 5 named
hosts — while the requester's `nodeAffinity` matches `gpu.product`, so all 14 are
eligible. An unconstrained requester therefore lands beside warm capacity about
a third of the time at best, and less than that in practice: the scheduler
*spreads* replicas away from nodes already running pods, which is precisely the
wrong direction here.

That a *preferred* term is strong enough to reverse it is a claim about
scheduler scoring, not about YAML, so it is measured rather than asserted:

```bash
make benchmark-verify-warm-affinity   # any cluster with >= 3 nodes, no GPUs needed
```

It stands launchers up on one node of three, then runs six requester replicas
with and without the rendered affinity. Measured: **2/6 on the launcher node
without it — an even scatter — against 6/6 with it.**

**Everything under `fma:` is inert while `fma.enabled` is false.** `step_06`
skips itself when FMA is not a deployed method, so no node is selected or
labeled and no `nodeSelector` or affinity is rendered; `step_02a_fma_warmup_hotstart`
returns early on the same flag, so `WARM_REPLICAS` warms nothing. Our scenario
carried `launcherNodeSelection.enabled: true` under `enabled: false` for months
— it read as configured, did nothing, and every FMA-vs-non-FMA number taken
before 2026-08-18 is a cold-path measurement because of it. A node selector
matching **zero** nodes fails the same way: it constrains nothing and reports
success.

Both shapes are now checked rather than eyeballed:

```bash
# static, runs automatically at the top of benchmark-standup
bash hack/benchmark/fma_placement.sh check hack/benchmark/scenarios/guides/workload-autoscaling.yaml

# live, runs automatically in benchmark-run; against someone else's FMA:
make benchmark-fma-verify BENCHMARK_NAMESPACE=<ns>
```

`verify` prints launchers and requesters per node and fails when the scenario
claims a placement the cluster does not have. A requester sitting on a node with
no launcher is reported as a warning — that one is a cold rebuild on its next
wake.

### Do not start a run before the model serves

`benchmark-run` gates on the endpoint the harness itself detects
(`hack/benchmark/wait_serving.sh`). Deployment `readyReplicas` is not the same
condition: the EndpointSlice, the InferencePool's view of it and the router all
sit between them. Set `WAIT_SERVING=false` only for a target with no router.

## Cleanup

```bash
kubectl delete -k config/fma-launcher-metrics -n ${NAMESPACE}
```

Removes only the scraping. The ScaledObjects go with `make undeploy-wva`, and the
FMA stack itself is not WVA's to remove.

## Configuration

| parameter | default | what it does |
| --- | --- | --- |
| `WVA_FMA_LAUNCHER_METRICS` | `false` | apply the launcher PodMonitor at install time, into `WVA_WATCH_NS` (else `WVA_NS`) |

FMA's own knobs, for reference — these are set where the FMA stack is deployed,
not by WVA:

| object | field | what it controls |
| --- | --- | --- |
| `InferenceServerConfig` | `modelServerConfig.options` | the vLLM command line, including `--model`. WVA reads the model ID from here |
| `InferenceServerConfig` | `modelServerConfig.port` | the port an instance serves on (`8000` in every stack observed) |
| `InferenceServerConfig` | `modelServerConfig.labels` | the llm-d labels stamped onto a launcher **at bind time**, which is how it joins the InferencePool |
| `LauncherConfig` | `maxInstances` | concurrent vLLM instances **per launcher pod** (see below) |
| `LauncherPopulationPolicy` | `countForLauncher[].launcherCount` | launcher pods **per matching node** — not a cluster total |
| `LauncherPopulationPolicy` | `enhancedNodeSelector` | which nodes get launchers |
| requester Deployment | `replicas` | **the scale lever.** Each replica reserves a GPU and binds one instance |

## One launcher, several models

A launcher pod is **not** a model server. It is a process manager: `launcher.py`
listening on `:8001` with a CRUD API over vLLM child processes.

```
launcher pod
├─ launcher process            :8001   creates/deletes instances on request
├─ vLLM instance A             :8000   from InferenceServerConfig A  → model X
├─ vLLM instance B             :80xx   from InferenceServerConfig B  → model Y
└─ …                                   up to LauncherConfig.maxInstances
```

Each instance comes from an `InferenceServerConfig`, and different ISCs name
different models — so **one launcher pod can host instances of several different
models at once**. Each gets its own port, its own instance ID, and its own GPU.

Ask a launcher directly:

```bash
kubectl exec -n ${NAMESPACE} <some-pod> -- \
  curl -s http://<launcher-pod-ip>:8001/v2/vllm/instances
```

```json
{"revision":1,"total_instances":1,"running_instances":1,
 "instances":[{"status":"running","instance_id":"IfWgHx-z7eR2…",
   "options":"--model Qwen/Qwen3-0.6B --enable-sleep-mode … --port 8000",
   "gpu_uuids":["GPU-ae2da921-…"],
   "annotations":{"inference-port":"8000","isc-name":"fma-qwen-…"}}]}
```

**Binding is per instance, not per pod.** A requester pod binds one instance:
`dual-pods.llm-d.ai/instance` on the requester is the instance ID, while
`dual-pods.llm-d.ai/dual` on both pods names the paired *pod*.

### What that means for WVA

Two separate limits, and they are worth keeping apart because only one of them
needs anything from FMA.

**Measurement already distinguishes instances.** Every series carries
`model_name` from vLLM plus `instance` (`podIP:port`) and `pod` from Prometheus,
and WVA keys replicas on `podName + ":" + port`. Two instances in one launcher
are two distinct replicas as far as the collector is concerned — no change
needed.

**Attribution is solvable with what is already on the pods.** The launcher's
`dual` label is singular, so it names one requester. But each requester names its
launcher, so the reverse lookup — the pods whose `dual` points at this launcher —
yields every requester bound into it. The row itself carries `model_name`, and
each requester belongs to exactly one model, so the model is the join key: pick
the requester whose scale target belongs to the model this collection pass is
for. No instance ID required. WVA does not do this yet; it is the reverse index
described in [the design](../../proposals/fma-aware-attribution.md) §1.5.

**Scraping is the part that needs FMA.** A PodMonitor builds its targets from pod
metadata, and there is one `server-port` annotation per pod, so only that
instance is ever scraped — the other instances' rows do not exist to attribute,
however good the attribution is. Closing that needs FMA to expose one aggregated
`/metrics` per launcher, labelled by instance —
[upstream request 5](../../proposals/fma-upstream-requests.md).

**Routing is configuration, not a wall.** An `InferencePool`'s `targetPorts` is a
list of up to 8, and the v1 API treats each as a distinct endpoint addressable as
`podIP:portNumber`. So the contract already accommodates a launcher serving four
instances on four ports — `maxInstances: 4` fits inside the limit of 8 with room
to spare.

What is missing is that nobody declares them: **163 pools measured, every one
with exactly one port**. Until a pool lists the other ports, the instances behind
them get no traffic. That is a change to how the pool is generated — the llm-d
router chart writes it — not a change to the API.

So the order of work is: the pool must declare the ports (routing), FMA must
expose the other instances' metrics (measurement, upstream request 5), and WVA
must attribute them (the reverse index above). All three are needed, and the
first two are not ours. What has changed is that none of them requires a new API.

So today WVA measures the instance on the annotated port and **under-measures the
rest**. Rows whose model does not match the pairing are rejected rather than
mis-attributed: a row is never charged to a variant it does not belong to. Watch
for `pairing_unresolved`.

Every deployment observed so far runs **one instance per launcher**, where none of
this arises.

### When a launcher changes instance

The scrape address is rebuilt from the pod's annotation on every service-discovery
pass, so a rebind is followed without anything being reconfigured. Measured on
kind, with one pod running two engines on two ports and the annotation moved
between them:

| | `server-port` | scrape target | series |
| --- | --- | --- | --- |
| bound to A | `8000` | `podIP:8000` up | `model_name=A instance=podIP:8000` |
| rebound to B | `8001` | **follows** to `podIP:8001` up | `model_name=B instance=podIP:8001` |
| unbound | *removed* | **no target** | — |

Two consequences worth knowing.

**The old series do not vanish with the target.** For one lookback window the
instant query still returns `model_name=A instance=podIP:8000`, because its last
sample is recent even though nothing is scraping it any more. That is what makes
the `instance` label alone unsafe as identity across a rebind.

**Attribution rejects them anyway, and now says so.** Collection runs per model,
so those rows are looked up against the model they claim, while the pod's pairing
now names the *new* model's requester. The row resolves to a variant this model
does not own, so it is ignored rather than charged to the wrong one, and counted
as `other_model_variant`. A brief count right after a rebind is expected; a
sustained one means a pairing pointing at a variant of a model the pod does not
serve.

### The control loop, not FMA, is the slow part

FMA actuates fast. Measured on pokprod, creation to `Ready`:

| | |
| --- | --- |
| decode pod (cold engine) | **90s** |
| **FMA requester** | **3s** |

The requester holds no GPU and runs no engine — it is a claim on a launcher that
is already warm, which is the entire point. With 14 launchers resident and none
bound, capacity was available in seconds.

WVA does not ask for it in seconds:

```
GLOBAL_OPT_INTERVAL                      60s   one optimization pass a minute
PROMETHEUS_METRICS_CACHE_FETCH_INTERVAL  30s   metrics up to this stale
KEDA pollingInterval                      5s
FMA requester ready                       3s
```

So up to ~90 seconds passes between load arriving and WVA asking for more, and
then 3 seconds to get it. **FMA's speed is about 3% of the end-to-end latency;
the control loop is the rest.**

For a modelservice variant this is proportionate — there is no point deciding
faster than a 90-second pod start. For an FMA variant it is 30× slower than the
thing it drives, and the warm pool buys nothing it could not have got by waiting.
A benchmark measured this directly: an FMA variant stayed at 1–2 replicas through
a burst while 14 warm launchers sat unbound, and the shortfall showed up as
shed load rather than as slow pods.

If you are autoscaling an FMA stack, `GLOBAL_OPT_INTERVAL` is the first thing to
look at. Nothing here is evidence for a particular value — only that the default
was chosen for cold pods and is the binding constraint once actuation is warm.

### Sleep mode and the warm pool

An instance survives unbinding. It goes to sleep — weights offloaded, `engine_sleep_state{sleep_state="weights_offloaded"} 1` — and stays resident, which is why the next bind takes seconds instead of minutes.

That resident instance still occupies a GPU.

## GPU accounting is a lower bound

A launcher requests **no** `nvidia.com/gpu`: the requester reserves the accelerator
and the launcher binds onto it, so requesting on both halves would double-book.
While a pair is bound, accounting is exactly right.

On unbind it is not. Measured with every requester at `replicas: 0`:

| | |
| --- | --- |
| GPU requests charged in the namespace | 1 |
| launcher pods running a vLLM instance | 9, on 9 distinct GPU UUIDs |

A `ResourceQuota` on `requests.nvidia.com/gpu` cannot see that, WVA's free-GPU
view is optimistic by the size of the warm pool, and the scheduler may place
another workload on a GPU a sleeping instance will wake onto. **There is no fix
from outside FMA** — the annotations naming the GPU are stripped at unbind, so the
API server holds no record of it.

Treat every GPU number in an FMA namespace as a minimum. Detail:
[the GPU limiter guide](../../deployment/gpu-limiter.md), "FMA namespaces".

## Known limitations

- **Multi-model launchers are partly measured**, as above.
- **Warm-pool GPUs are invisible**, as above.
- **`sleeping` is not a liveness signal.** The label reports the instances' sleep
  state, so it reads `false` both when an instance is awake and when there is
  none. WVA keys on the pairing label instead. Observed live: a pod labelled
  `sleeping=false` hosting zero instances, and one labelled `sleeping=true`
  hosting a running instance.
- **A non-FMA guide can silently disable scraping.** Both render a PodMonitor
  named `vllm-<model>`; standing up a non-FMA guide in the same namespace
  overwrites the FMA-aware one with a port-name endpoint, and the launchers go
  dark with no error. `make benchmark-standup` warns when it detects launchers.
- **A launcher's readiness does not track its instance.** The `readinessProbe`
  asks `:8001/v2/vllm/instances` — the launcher's own CRUD API — while EPP dials
  `:8000`, the pool's `targetPort`. Port 8001 answers whenever the process
  manager is up, including with zero instances, with one asleep, and while one is
  starting, so the pod reads `Ready` for its whole life regardless of whether
  anything can serve. The decode pod probes `/v1/models` — the engine itself —
  and so stays `NotReady` until it can actually answer.

  In practice the exposure is small, because a bound instance wakes in seconds:
  no request has been observed lost to it here. What it means is that the
  guarantee rests on wake being fast rather than on the pod reporting readiness,
  so a slow or failed wake becomes silent traffic loss. Not fixable from WVA,
  and not fixable in `LauncherConfig` either — a probe set in `podTemplate` is
  overwritten by the controller (tested). Tracked as
  [upstream request 7](../../proposals/fma-upstream-requests.md).

## Next

- [What we have asked FMA to change](../../proposals/fma-upstream-requests.md)
- [How attribution works](../../proposals/fma-aware-attribution.md)
- [After the install](../../deployment/operations.md)
