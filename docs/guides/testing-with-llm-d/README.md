# Test WVA against a full llm-d stack

## Overview

Brings up llm-d and WVA together on a local kind cluster with **emulated GPUs**,
so the whole chain — model server, EPP, KEDA, WVA — can be exercised on a laptop.
The model servers are simulators: they answer requests and emit vLLM metrics
without a GPU.

For a real cluster, install llm-d from its own
[guides](https://github.com/llm-d/llm-d/tree/main/guides) and then follow
[Install WVA in a namespace](../install-in-namespace/README.md). WVA does not
install llm-d: which model, which accelerator and whose HuggingFace token are the
deployment decision, not an autoscaler's.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `CLUSTER_GPU_TYPE` | `nvidia-mix` | `nvidia-a100` |
| `CLUSTER_NODES` | `3` | `1` |
| `CLUSTER_GPUS` | `4` | `8` |

<!-- guide:env.static.cluster start -->
```bash
export CLUSTER_GPU_TYPE=nvidia-mix CLUSTER_NODES=3 CLUSTER_GPUS=4
```
<!-- guide:env.static.cluster end -->

## Prerequisites

kind, kubectl, helm, jq and a container runtime. No GPUs.

<!-- guide:prerequisites.cluster start -->
```bash
make create-kind-cluster
```
<!-- guide:prerequisites.cluster end -->

Nodes are labelled with emulated GPU capacity, which is what lets WVA resolve
accelerators and compute budgets.

## Installation

<!-- guide:deploy.stack start -->
```bash
make deploy-e2e-infra SCALER_BACKEND=keda USE_SIMULATOR=true SCALE_TO_ZERO_ENABLED=true
```
<!-- guide:deploy.stack end -->

Deploys Prometheus, KEDA, the EPP and WVA. Then a model server to scale:

<!-- guide:deploy.workload start -->
```bash
kubectl apply -k config/samples/simulator/nodeSelector/decode/
```
<!-- guide:deploy.workload end -->

The sample includes its own ScaledObject, which is the registration.

## Verification

<!-- guide:verify.chain start -->
```bash
kubectl get scaledobject,hpa -n llm-d-sim
kubectl logs -n workload-variant-autoscaler-system deploy/wva-controller-manager | grep scaling-decision
```
<!-- guide:verify.chain end -->

Or run the suite:

<!-- guide:verify.suite start -->
```bash
make test-e2e-smoke
```
<!-- guide:verify.suite end -->

`make test-e2e-full` runs everything. Pass `SCALE_TO_ZERO_ENABLED=true` to
include the scale-from-zero suites — they skip silently without it, and a run
that skips them still reports success.

## Cleanup

<!-- guide:cleanup.cluster start -->
```bash
make destroy-kind-cluster
```
<!-- guide:cleanup.cluster end -->

## Next

- [Testing](../../developer-guide/testing.md) — the suites, their labels and flags
- [Benchmarking](../benchmarking/README.md)
