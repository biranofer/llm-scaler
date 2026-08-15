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

- WVA installed ([in a namespace](../install-in-namespace/README.md) or
  [cluster-wide](../install-cluster-wide/README.md))
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

```
ceiling = min( Σ_nodes (launcherCount × maxInstances),   # warm slots
               Σ_nodes free GPUs )                        # accelerators
```

The plan caps the entry at the launcher pods present, which is the first term
only. The second is not visible from discovery, and in an FMA namespace the GPU
picture is itself a lower bound ([why](#gpu-accounting-is-a-lower-bound)).

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

## Next

- [What we have asked FMA to change](../../proposals/fma-upstream-requests.md)
- [How attribution works](../../proposals/fma-aware-attribution.md)
- [After the install](../../deployment/operations.md)
