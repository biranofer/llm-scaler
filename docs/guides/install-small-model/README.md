# Install a small llm-d model

## Overview

Deploys one small model — `Qwen/Qwen3-0.6B` — with an EPP and an InferencePool,
on a single GPU, so there is something real for WVA to scale. Use it to try WVA,
to reproduce a scaling question, or as the target for
[`make benchmark-smoke`](../benchmarking/).

It exists because there is no upstream equivalent. llm-d's own guides deploy
models that need a fleet: [Optimized Baseline][ob] — the inference-scheduling
entry point — defaults to `Qwen/Qwen3-32B` across **16 GPUs** (8 replicas, TP=2),
and the disaggregation guides target `gpt-oss-120b`. Those are the right defaults
for what those guides teach. They are the wrong ones for "I have one card and I
want to see this work".

For correctness work with no GPU at all, use
[Test WVA against a full llm-d stack](../testing-with-llm-d/) instead — it runs
simulators on kind.

## Prerequisites

- one GPU, and a node that will schedule onto it
- `kubectl`, and rights to create workloads in your namespace
- the benchmark CLI's prerequisites, which the standup installs for you

<!-- guide:env.static.namespace start -->
```bash
export NAMESPACE=<your-namespace>
export MODEL_ID=Qwen/Qwen3-0.6B
```
<!-- guide:env.static.namespace end -->

<!-- guide:prerequisites.gpu start -->
```bash
# One GPU, and a node that will schedule onto it. This deploys a real model
# server, not a simulator.
kubectl get nodes -o custom-columns=NODE:.metadata.name,GPU:.status.allocatable.'nvidia\.com/gpu' --no-headers
```
<!-- guide:prerequisites.gpu end -->

## Installation Instructions

<!-- guide:deploy.standup start -->
```bash
# Deploys llm-d (model server + EPP + InferencePool) and WVA, then registers
# the workload. Set IMG to a build of this branch.
make benchmark-standup BENCHMARK_NAMESPACE=${NAMESPACE} MODEL_ID=${MODEL_ID} IMG=${IMG}
```
<!-- guide:deploy.standup end -->

This deploys the model server, the EPP and its InferencePool, installs WVA from
this repo, and registers the workload as a ScaledObject. `IMG` should be a build
of this branch: released images reject `--external-scaler-bind-address`, which
these manifests pass.

### The one setting that matters for a small model

**`gpuMemoryUtilization` has to follow the model.** vLLM reads it as a fraction
of the card's **total** memory, not of what is free, and it then fills that
budget with KV cache. A 32B model wants 0.95 of an 80GB card. A 0.6B model —
1.12 GiB of weights — does not, and asking for 0.95 anyway makes it claim
**73.5 GiB of KV cache** and then fail:

```
Available KV cache memory: 73.53 GiB
torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 594.00 MiB.
GPU 0 has 79.18 GiB of which 500.69 MiB is free.
```

The standup sets this for you now: `GPU_MEM_UTIL` defaults to `0.30` when
`MODEL_ID` names the small model and `0.95` otherwise. At 0.30 the same model
takes 22 GiB of KV cache — 206,592 tokens, far more than a 16k context needs —
and leaves 56 GiB of the card for everyone else.

**Why it looks intermittent.** 0.95 works on an empty GPU, so this failed only
sometimes and looked like flakiness. It is not: the memory that is already gone
belongs to pods the *scheduler cannot see*. FMA launcher pods request **zero**
GPUs while holding the engine — that is the whole design — so Kubernetes places
your model onto a card it believes is empty, and vLLM then measures 77.3 of 79.18
GiB free and budgets as though it had all of it. Any co-tenant that holds memory
without requesting a GPU does the same thing.

Override it when you need to:

```bash
make benchmark-standup BENCHMARK_NAMESPACE=${NAMESPACE} MODEL_ID=${MODEL_ID} \
     IMG=${IMG} GPU_MEM_UTIL=0.50
```

## Verification

<!-- guide:verify.serving start -->
```bash
kubectl get pods -n ${NAMESPACE}
```
<!-- guide:verify.serving end -->

Both the decode pod and the EPP should be `Running`. A decode pod in
`CrashLoopBackOff` is almost always the memory budget above — check its log for
`torch.OutOfMemoryError` before looking anywhere else.

<!-- guide:verify.model start -->
```bash
# Ask the endpoint what it loaded. This is the name every request must use.
kubectl run probe -n ${NAMESPACE} --rm -i --restart=Never --quiet \
  --image=registry.k8s.io/e2e-test-images/agnhost:2.47 --command -- \
  /bin/sh -c "curl -s http://$(kubectl get svc -n ${NAMESPACE} -o name | grep epp | head -1 | cut -d/ -f2).${NAMESPACE}.svc.cluster.local:80/v1/models"
```
<!-- guide:verify.model end -->

The `id` it returns is the name every request must use. It is the *served* name,
which is not always the path the weights were loaded from.

<!-- guide:verify.smoke start -->
```bash
# Drive load through it and snapshot the dashboard.
make benchmark-smoke NAMESPACE=${NAMESPACE}
```
<!-- guide:verify.smoke end -->

Drives decode-heavy load for five minutes and snapshots the dashboard. See
[Benchmark WVA](../benchmarking/) for what to look at.

## Cleanup

<!-- guide:cleanup.teardown start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=${NAMESPACE}
```
<!-- guide:cleanup.teardown end -->

Removes WVA first, then the llm-d releases: a namespace-scoped install still
creates cluster-scoped RBAC, which deleting the namespace would leave behind.

## Configuration

| Parameter | Default | Notes |
| --- | --- | --- |
| `BENCHMARK_NAMESPACE` | — (required) | where everything lands |
| `MODEL_ID` | `Qwen/Qwen3-0.6B` | the served model |
| `IMG` | a build of this branch | released images reject this branch's flags |
| `GPU_MEM_UTIL` | `0.30` small model / `0.95` otherwise | fraction of **total** GPU memory |
| `WARM_REPLICAS` | `0` | FMA warm pool size, if the scenario uses one |

## Next

- [Benchmark WVA](../benchmarking/) — drive load and compare runs
- [Install WVA in a namespace](../install-in-namespace/) — the install on its own
- [After the install](../../deployment/operations.md) — what the metrics mean

[ob]: https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline
