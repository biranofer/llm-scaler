# Benchmark WVA

## Overview

Stands up an llm-d stack with WVA on a GPU cluster, drives load through it with
[llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark), and reports latency,
replica counts and cost per run.

Use it to compare scaling behaviour across configurations — thresholds, limiters,
one variant against two. It needs real GPUs; for correctness work without them,
see [Test WVA against a full llm-d stack](../testing-with-llm-d/).

## Prerequisites

A GPU cluster, the namespace and image to measure, and the benchmark CLI:

<!-- guide:env.static.target start -->
```bash
export BENCHMARK_NAMESPACE=<namespace>
export IMG=<your build>
```
<!-- guide:env.static.target end -->

<!-- guide:prerequisites.cli start -->
```bash
make benchmark-install
```
<!-- guide:prerequisites.cli end -->

### If the namespace runs Fast Model Actuation

Benchmarking an FMA namespace works, and standup warns you about the one trap
that matters: this stack renders a PodMonitor named `vllm-<model>`, the same name
the FMA guide uses, so a scenario without `fma.enabled` **overwrites the
FMA-aware one and the launchers stop being scraped, silently**. Most of the
traffic then goes unmeasured — which is how a variant once sat flat at one
replica through a 155-deep queue.

Two things follow for the numbers you get:

- `BENCHMARK_KEDA_MAX_REPLICAS` is a starting value, not the ceiling. Discovery
  caps an FMA variant at the **launcher pods present**, because only one instance
  per launcher is reachable today. Raising the variable past that buys nothing —
  the extra requester pods sit `Pending` and, counting toward anticipated supply,
  suppress the scale-up they were meant to provide.
- The launcher pool does not grow with the benchmark. It is a declared count per
  matching node, so load does not expand it.

Both are explained in [Autoscale a Fast Model Actuation stack](../fma/).

## Installation Instructions

### 1. Stand up the stack

<!-- guide:deploy.standup start -->
```bash
make benchmark-standup BENCHMARK_NAMESPACE=${BENCHMARK_NAMESPACE} IMG=${IMG}
```
<!-- guide:deploy.standup end -->

Stands up the model servers via llm-d-benchmark, then installs WVA from this
repo and registers the workloads. prometheus-adapter is deliberately not
installed: it and KEDA both claim the `external.metrics.k8s.io` APIService, of
which a cluster has one.

To keep llm-d-benchmark from installing it anyway, the standup creates the
`prometheus-adapter-resource-reader` ClusterRole that its "is prometheus-adapter
already here?" probe looks for. That object is cluster-scoped and unsuffixed, so
on a shared cluster it may already belong to a **real** prometheus-adapter
release: when it does, and that release lives in another namespace, the standup
leaves it alone and says so rather than rewriting its Helm ownership — which
would break that release's next `helm upgrade`.

### 2. Run a workload

<!-- guide:verify.run start -->
```bash
make benchmark-run BENCHMARK_NAMESPACE=${BENCHMARK_NAMESPACE}
```
<!-- guide:verify.run end -->

<!-- guide:verify.report start -->
```bash
make benchmark-report
```
<!-- guide:verify.report end -->

Restart the controller between runs — `make benchmark-restart-controller` —
or learned per-replica capacity from the previous run carries into the next.

## Cleanup

<!-- guide:cleanup.teardown start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=${BENCHMARK_NAMESPACE}
```
<!-- guide:cleanup.teardown end -->

Removes WVA first, then the llm-d releases: a namespace-scoped install still
creates cluster-scoped RBAC, which deleting the namespace would leave behind.

## Configuration

Optional, except `BENCHMARK_NAMESPACE`.

| Parameter | Default | Example |
| --- | --- | --- |
| `BENCHMARK_NAMESPACE` | — (required) | `my-bench` |
| `IMG` | a build of this branch | `ghcr.io/you/wva:dev` |
| `BENCHMARK_SPEC` | `guides/workload-autoscaling` | `guides/two-variant-wva` |
| `BENCHMARK_HARNESS` | `guidellm` | `inference-perf` |
| `MODEL_ID` | `Qwen/Qwen3-0.6B` | `Qwen/Qwen3-32B` |
| `BENCHMARK_REPO_REF` | `v0.7.8` | `main` |
| `BENCHMARK_IMAGE_TAG` | the value of `BENCHMARK_REPO_REF` | `nightly` |
| `BENCHMARK_HARNESS_RUN_AS_USER` | `0` | `1000` |
| `BENCHMARK_KEDA_SCALE_UP_PERIOD` | `5` | `15` |
| `BENCHMARK_KEDA_SCALE_UP_STABILIZATION` | `0` | `60` |
| `BENCHMARK_KEDA_SCALE_DOWN_PERIOD` | `120` | `60` |
| `BENCHMARK_KEDA_SCALE_DOWN_STABILIZATION` | `300` | `120` |

**The harness image must match `BENCHMARK_REPO_REF`.** The harness pod always
runs the checkout's scripts — they are copied in from a ConfigMap built from the
tree — while the image comes from llm-d-benchmark's `defaults.yaml`, which pins
`v0.7.0` regardless of ref. Mixing them fails at the first treatment, e.g.
v0.7.8's scripts call `guidellm run` and the v0.7.0 image's guidellm only has
`benchmark`: `Error: No such command 'run'`. `BENCHMARK_IMAGE_TAG` follows the
ref for you; set it only to pin something else.

**`BENCHMARK_HARNESS_RUN_AS_USER` is 0 because the harness writes to
`/usr/local/bin` at startup.** llm-d-benchmark stopped forcing that UID in
v0.7.8, so on OpenShift the pod gets a namespace UID and dies with
`cp: cannot create regular file ...: Permission denied`. Granting `anyuid` to
its ServiceAccount may not be enough — a cluster SCC with a higher priority can
still win and impose `MustRunAsRange`; asking for UID 0 is what makes that SCC
unable to admit the pod.

**Scaling knobs: the stabilization window is the one that matters.** HPA takes
its most conservative recommendation across that window, so `scaleUp`
stabilization is what delays a scale-up — the `periodSeconds` values are rate
limits, and nothing reacts faster than the HPA control loop's sync period (15s
by default). Scale-up therefore acts on the current recommendation (`0`), while
scale-down waits 300s: removing a replica too eagerly costs a cold start on the
next request, and keeping one too long only costs money.

**The default model is small on purpose.** These runs measure WVA's scaling
behaviour, and a 0.6B model exercises the same path a 32B one does — discovery,
the ScaledObject plan, the scale decision, the report — while pulling far less
and holding one GPU per replica instead of several. Pass
`MODEL_ID=Qwen/Qwen3-32B` when you want the scenario's own model, or
`BENCHMARK_MODEL_ID=` (empty) to defer to whatever the scenario names.

Latency and throughput numbers are model-specific, so a 0.6B run tells you
nothing about a 32B model's TTFT. It tells you whether WVA scaled correctly,
which is what this suite is for.

**`IMG` decides what is measured**, and nothing in the results afterwards says
which binary produced them. The default is a build of this branch rather than a
release: released images reject `--external-scaler-bind-address`, which these
manifests pass, so a run against one measures a CrashLoopBackOff. Set `IMG` to
your own build whenever you have changed controller code.

## Snapshotting a run

A run's charts live in Prometheus, which ages them out. Capture them while they
are still there:

```bash
hack/benchmark/snapshot.py \
  --namespace ${BENCHMARK_NAMESPACE} \
  --prometheus-url https://thanos-querier-openshift-monitoring.apps.<cluster>/ \
  --token "$(oc whoami -t)" --insecure \
  --since 30m --out <run-dir>/snapshot
```

That writes `panels.json`: every query in `deploy/grafana/benchmark-dashboard.json`,
run over the window, with its results. It is stdlib-only, so it works in a shell
whose python has no pip — including the one the benchmark venv provides.

Then render the images, offline, through the real dashboard:

```bash
hack/benchmark/snapshot-images/render.sh <run-dir>/snapshot
```

Grafana and its image renderer come up in docker, provisioned with this repo's
own dashboard, and read the snapshot through a shim that speaks the Prometheus
API. The output is one PNG per panel plus the whole dashboard. Because the data
is a file, a run can be re-rendered months later with no cluster at all, and the
images cannot drift from the dashboard — they *are* the dashboard.

The dashboard takes a **Namespace** variable, populated from the vLLM metrics
Prometheus actually holds, so the KV-cache and queue-depth panels work wherever
the benchmark runs. Pick your namespace at the top of the dashboard when viewing
it live; `--namespace` does it for a capture, and the render passes it through.

## Two variants

Comparing two variants of one model at different costs has its own scenario and
post-processing — replica timelines per variant, weighted cost, a full-pipeline
plot. See [the two-variant benchmark](../../developer-guide/two-variant-wva-benchmark.md).
