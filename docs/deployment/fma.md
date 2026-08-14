# WVA with Fast Model Actuation

Everything you need to run the Workload Variant Autoscaler in a namespace that
uses [Fast Model Actuation](https://github.com/llm-d-incubation/llm-d-fast-model-actuation).
If your namespace has no launcher pods, none of this applies and nothing here
changes what WVA does.

## What FMA changes

FMA splits a model server across two pods:

- a **requester** Deployment, which carries the llm-d identity and is what a
  scaler moves. It runs no engine — measured on a real cluster, its ports answer
  404 and it reports zero `vllm:` series.
- **launcher** pods, owned by a `LauncherConfig`, which hold the GPU and run
  vLLM. These serve the traffic and report every metric.

A dual-pods controller binds them, stamping `dual-pods.llm-d.ai/dual` on both
halves — each naming the other — and the serving labels onto the bound launcher,
which is how it joins the InferencePool. Binding is fast: measured under five
seconds.

The consequence for an autoscaler is that **the pod that reports is not the pod
that is owned**. A launcher's `ownerReferences` lead to a `LauncherConfig`, never
to a Deployment under a ScaledObject — deliberately, since FMA patches the
provider's labels so no ReplicaSet adopts it. So the two halves fail in opposite
directions:

| | scalable | reports engine metrics |
| --- | --- | --- |
| requester Deployment | yes | no |
| launcher pods | no | yes |

WVA closes that gap by following the pairing: when the ownerReferences walk finds
no scale target and the pod declares a partner, it resolves the partner instead.
A bound launcher's metrics are therefore attributed to the requester's
ScaledObject. The controller reports it once per cycle:

```
Attributed FMA launcher pods through their dual-pods pairing  {"count": 4, "example": "launcher-… -> fma-requester-…"}
```

Design detail: [FMA-aware attribution](../proposals/fma-aware-attribution.md).

## 1. Make the launchers scrapeable

**Do this first.** Attribution has nothing to work with until the launchers are
scraped, and by default they are not.

A launcher declares **no container ports** and serves metrics on `:8000` (a
decode pod uses `:8200`). A PodMonitor that selects its endpoint *by port name*
resolves nothing and generates **no target at all** — not a failing target, no
target. So `up{}` in an FMA namespace lists the decode pod, EPP and WVA, and no
launcher, with no error anywhere.

WVA ships a PodMonitor for this:

```bash
# at install time
WVA_FMA_LAUNCHER_METRICS=true ./deploy/install.sh

# or at any point, standalone — this needs nothing from WVA and is equally
# useful in a namespace running FMA with no autoscaler at all
kubectl apply -k config/fma-launcher-metrics -n <namespace-with-launchers>
```

It builds the scrape address from `dual-pods.llm-d.ai/server-port`, the port FMA
records on the pod, rather than hardcoding one — so it follows FMA if the port
changes, and it **skips launchers with no bound instance**, which carry no such
annotation. Measured: 14 unbound launchers produced 0 targets, and a launcher
reached `up=1` with 96 distinct `vllm:` metric names within 30 s of binding.

Three things to know:

- It goes in the **workload** namespace, not the controller's. The installer uses
  `WVA_WATCH_NS` (falling back to `WVA_NS`); a cluster-scoped install must repeat
  the command per namespace.
- **With no FMA it does nothing at all** — the selector matches no pods, so no
  targets and no series. It is safe to apply pre-emptively and safe to leave in
  place after FMA is removed.
- The installer **refuses** to apply it when another PodMonitor already scrapes
  launchers. Two scrape configs on one pod give it two targets under the same
  `(instance, pod)` key, and WVA's additive `sum by` queries would double-count
  throughput while the `max by` ones still looked correct — inflated tokens/sec,
  not an error.

### The trap that hides all of this

llm-d-benchmark renders a correct FMA PodMonitor when a scenario sets
`fma.enabled`. But that object is named `vllm-<model>`, and standing up a
**non-FMA** guide into the same namespace renders the same name with
`port: metrics` and **overwrites it**. The FMA stack keeps serving; its metrics
just stop being collected.

That is not hypothetical — it is what happened on a real cluster (an FMA guide at
09:25Z, another guide over the top at 09:26Z) and it is why a benchmark once
showed a variant flat at one replica through a 155-deep queue. `make
benchmark-standup` now warns when it detects launcher pods.

Check it directly:

```bash
# should include launcher-* pods; if it lists only decode/EPP/WVA, they are dark
kubectl get --raw "/api/v1/namespaces/$NS/services/prometheus:9090/proxy/api/v1/query?query=up%7Bnamespace%3D%22$NS%22%7D"

# a `port` of `metrics` on an FMA namespace means the launchers are not scraped
kubectl get podmonitor -n "$NS" -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.podMetricsEndpoints[*].port}{"\n"}{end}'
```

## 2. Register the workload

What `make scaledobjects-plan` writes depends on what the namespace holds:

| namespace holds | the plan writes | default |
| --- | --- | --- |
| a `decode`/`prefill` Deployment only | one entry for that Deployment | `apply: yes` |
| an FMA **requester** only | one entry for the requester | `apply: yes` — nothing else there can be scaled |
| **both** | **two entries**, the modelservice half and the FMA half | the FMA half is `apply: no`, so an existing install applies exactly what it applied before |

A requester entry looks like this. The model comes from the
`InferenceServerConfig` the pod template names, since a requester has no
`--served-model-name` of its own:

```yaml
plan:

  # note: FMA variant: the requester is the scale target, and its engine metrics
  # come from launcher pods scraped by fma-launcher-metrics. WVA follows the
  # dual-pods pairing to attribute them.
  - apply: "yes"
    namespace: llm-d-sim
    kind: Deployment
    name: fma-requester-dev-model      # the requester, not a decode Deployment
    modelID: "meta/llama"              # read from the InferenceServerConfig
    minReplicas: 1
    maxReplicas: 3                     # capped at the launcher pods present
    variantCost: "10.0"
    # inferencePool: (none)            # the pool selects the LAUNCHER, not the requester
```

### Running both halves of one model

When a modelservice Deployment and an FMA requester both serve a model, the plan
offers both and lets you pick with the switch it already has:

| you want | you do |
| --- | --- |
| the modelservice half only | leave the plan alone — this is the default |
| **both, as two variants** | set the FMA entry to `apply: yes` |
| the FMA half only | set the FMA entry to `yes` and the modelservice entry to `no` |

Both entries carry the **same `modelID`**, which is what makes WVA treat them as
two variants of one model — it then allocates replicas between them by
`variantCost`, exactly as it does for two accelerator types. They are not two
models, and nothing new is deployed: this option appears only when both workloads
already exist, since WVA never creates model servers.

### When the entry arrives switched off

If step 1 has not been done, the FMA entry arrives as `apply: no` with the command
that fixes it, rather than being omitted or applied blind:

```yaml
  # note: No PodMonitor generates a scrape target for the N launcher pod(s) in
  # <namespace>, so this variant would have no metrics at all — launchers declare
  # no container ports, and a port-name endpoint resolves nothing. Fix with:
  # kubectl apply -k config/fma-launcher-metrics -n <namespace>
  # (or WVA_FMA_LAUNCHER_METRICS=true at install), then set apply: yes.
  - apply: "no"
    namespace: llm-d-sim
    kind: Deployment
    name: fma-requester-dev-model
    modelID: "meta/llama"
```

That is deliberate: a requester runs no engine, so every metric for the variant
comes from a launcher pod. A ScaledObject that cannot measure its workload is
worse than none — it holds the workload at `minReplicaCount` and reports healthy.

## 3. Size `maxReplicas` from the launcher pool

**FMA does not cap scaling. The launcher pool does.** Nothing about FMA's
presence stops ordinary scaling — a modelservice Deployment in the same namespace
scales to its own `maxReplicas` as usual.

What is bounded is the FMA half. A requester replica becomes capacity only if a
launcher can bind to it *and* a GPU is free. Past that ceiling the extra requester
pods sit `Pending` — and pending replicas still count toward *anticipated supply*,
so they suppress the very scale-up they were meant to provide. The system settles
into "I have asked for enough" while serving nothing new.

```
ceiling = min( Σ_nodes (launcherCount × maxInstances),   # warm slots
               Σ_nodes free GPUs )                        # actual accelerators
```

`launcherCount` comes from the `LauncherPopulationPolicy` (per matching node) and
`maxInstances` from the `LauncherConfig`. The populator maintains a **declared**
count — it does not grow the pool with demand — so this ceiling is real and
static until someone changes it.

The plan caps a requester entry's `maxReplicas` at the number of launcher pods
present, which is the first half of that `min()`. **It does not know the second
half**: free GPUs are not visible from discovery, and in an FMA namespace the GPU
picture is itself a lower bound (below). Treat the written value as a safe
starting point, not a computed answer, and check both terms before raising it.

## GPU accounting is a lower bound here

A launcher requests **no** `nvidia.com/gpu` — deliberately, since the requester
reserves the accelerator and requesting on both halves would double-book. While a
pair is bound that is exactly right.

The gap opens on unbind: the launcher keeps its vLLM instance resident, which is
what makes the next bind take seconds, and goes on occupying a physical GPU that
is charged to nobody. Measured with every requester at `replicas: 0`:

| | |
| --- | --- |
| GPU requests charged in the namespace | 1 |
| launcher pods running a vLLM instance | 9, on 9 distinct GPU UUIDs |

So a `ResourceQuota` on `requests.nvidia.com/gpu` cannot bound FMA's real
consumption, WVA's free-GPU view is optimistic by the size of the warm pool, and
the scheduler may place another workload on a GPU a sleeping instance will wake
onto. **There is no fix from outside FMA** — the annotations naming the GPU are
stripped at unbind, so the API server holds no record of it.

Treat every GPU number in an FMA namespace as a minimum, and subtract the warm
pool by hand when planning capacity. Detail:
[the GPU limiter guide](gpu-limiter.md), "FMA namespaces".

## Known limitations

- **One model per launcher is measurable.** FMA permits a launcher to host
  several vLLM instances, each on its own port, while a pod carries one
  `server-port` annotation — so a PodMonitor reaches at most one of them. Rows
  for a model the pairing does not name are rejected, not mis-attributed, and
  counted under `pairing_unresolved`.
- **Warm-pool GPUs are invisible**, as above.
- **`sleeping` is not a liveness signal.** The label reports the sleep state of
  the instances, so it reads `false` both when an instance is awake and when
  there is none. WVA keys on the pairing label instead. Observed on a live
  cluster: a pod labelled `sleeping=false` hosting zero instances, and one
  labelled `sleeping=true` hosting a running instance.

What we have asked the FMA project to change, and why:
[Requests to Fast Model Actuation](../proposals/fma-upstream-requests.md).

## Checking it works

```bash
# 1. launchers are scraped
kubectl get podmonitor -n "$NS"

# 2. WVA is attributing them — one line per cycle when the hop carries anything
kubectl logs -n "$WVA_NS" -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200 \
  | grep "Attributed FMA launcher pods"

# 3. nothing is being dropped that should not be
#    unbound_launcher    = warm spares, expected
#    pairing_unresolved  = a declared pairing that led nowhere — investigate
#    unresolved          = should be zero in an FMA namespace
kubectl logs -n "$WVA_NS" -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200 \
  | grep "Skipping pod"

# 4. demand reflects the launchers, not just a decode pod
kubectl logs -n "$WVA_NS" -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200 \
  | grep analyzer-result | tail -1
```

A healthy FMA namespace under load looks like this — supply counting the launcher
fleet, and demand non-zero:

```
analyzer-result  supply: 2137292  demand: 961383  util: 0.45
```

Before launcher attribution existed the same namespace reported
`supply: 550758, demand: 0, util: 0` — the decode pod alone — while four
launchers ran at 155, 136, 56 and 44 concurrent requests.
