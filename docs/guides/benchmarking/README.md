# Benchmark WVA

## Overview

Stands up an llm-d stack with WVA on a GPU cluster, drives load through it with
[llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark), and reports latency,
replica counts and cost per run.

Use it to compare scaling behaviour across configurations — thresholds, limiters,
one variant against two. It needs real GPUs; for correctness work without them,
see [Test WVA against a full llm-d stack](../testing-with-llm-d/README.md).

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
| `IMG` | the published image | `ghcr.io/you/wva:dev` |
| `BENCHMARK_SPEC` | `guides/workload-autoscaling` | `guides/two-variant-wva` |
| `BENCHMARK_HARNESS` | `guidellm` | `inference-perf` |

**`IMG` decides what is measured.** It defaults to a published image, so a run
that does not set it benchmarks a release rather than your branch — and nothing
in the results afterwards says which binary produced them.

## Two variants

Comparing two variants of one model at different costs has its own scenario and
post-processing — replica timelines per variant, weighted cost, a full-pipeline
plot. See [the two-variant benchmark](../../developer-guide/two-variant-wva-benchmark.md).
