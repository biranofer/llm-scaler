# Workload-Variant-Autoscaler Deployment Guide

> **Note:** This guide is developer/operator-oriented and covers deploying WVA from source. If you are an end user looking to install WVA as part of llm-d, see the [llm-d workload-autoscaling guide](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/README.wva.md) instead.

Complete guide for deploying the Workload-Variant-Autoscaler (WVA) on Kubernetes, OpenShift, and Kind clusters.

> **Central Documentation Hub**: This is the main deployment guide containing comprehensive information about deployment methods and configuration reference. Platform-specific guides ([Kubernetes](kubernetes/README.md), [OpenShift](openshift/README.md), [Kind](kind-emulator/README.md)) provide additional platform-specific details and examples.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites) — start with `make check-prereqs`
- [Choosing your install](#choosing-your-install)
- [Deployment Methods](#deployment-methods)
  - [Method 1: Automated Deployment Script](#method-1-automated-deployment-script-recommended)
  - [Method 2: Kustomize (direct controller install)](#method-2-kustomize-direct-controller-install)
- [Platform-Specific Guides](#platform-specific-guides)
- [Configuration Reference](#configuration-reference)
- [Post-Deployment](#post-deployment)
- [Troubleshooting](#troubleshooting)

## Overview

This guide covers two deployment procedures:

1. **Automated Script**: Complete end-to-end and customizable deployment including WVA, llm-d infrastructure, Prometheus, and HPA
2. **Kustomize**: Install the WVA controller directly into an existing cluster

## Prerequisites

### Required Tools

Don't check by hand — ask:

```bash
make check-prereqs                          # kubernetes (default)
make check-prereqs ENVIRONMENT=openshift
make check-prereqs WVA_LIMITER=gpu-inventory
```

It runs the **same** check the install runs, so a pass here and a prerequisite
failure mid-install is not a reachable state. It is read-only, and it fails with
the specific tool, the version found, and the version needed.

What it looks for depends on what you are installing:

| tool | when | minimum |
| --- | --- | --- |
| `kubectl` | always | 1.24 |
| `helm` | always (LWS, KEDA, Prometheus stack) | 3.8 |
| `git` | always | — |
| `yq` | `WVA_LIMITER` set, or `ENABLE_SCALE_TO_ZERO=true` (the default) — both patch the shipped ConfigMaps | — |
| `oc` | `ENVIRONMENT=openshift` | — |

It also confirms your current context reaches a cluster, because discovering that
after the first namespace and ClusterRoleBinding exist is worse than discovering
it now.

`kustomize` and `controller-gen` are **not** prerequisites: the Makefile installs
them into `./bin` on demand. Add `kind` only for the local kind-emulator flow.

### Cluster Requirements

**Minimum cluster specifications**:

- Kubernetes 1.24+ or OpenShift 4.12+
- Metrics server or Prometheus available
- For non-emulated LLM workloads: GPU availability

**Cluster access**:

- Cluster admin privileges (for full deployment script)
- Or namespace admin + ability to create ClusterRole/ClusterRoleBinding (for Kustomize direct install)

### Workload Requirements

Two things, and neither is an annotation or a label on the workload.

**1. A KEDA ScaledObject whose trigger points at WVA.** That call is what makes a
workload managed — WVA has no watch and no listing, so a workload it is never
called about does not exist to it. The trigger metadata carries the per-workload
configuration, and `modelID` is the only required field. See
[Choosing your install](#choosing-your-install) for the full trigger and for what
is derived rather than declared.

**2. Prometheus must scrape your model-server pods.** WVA reads vLLM/SGLang
metrics from Prometheus, filtered by namespace, so the series must exist and carry
a `namespace` label. A standard `ServiceMonitor` or `PodMonitor` over the model
service gives you both:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: my-model-monitor
  namespace: <monitoring-namespace>
spec:
  namespaceSelector:
    matchNames: [<workload-namespace>]
  selector:
    matchLabels:
      app: <your-model-service>
  endpoints:
    - port: metrics
```

Without those metrics WVA has no demand signal and will not scale the workload.

### Required Tokens

- **HuggingFace Token** (for llm-d deployment): after getting access to a model, set a token on [HuggingFace](https://huggingface.co/settings/tokens)
  - Required for: Full deployment script with llm-d

## Choosing your install

Four decisions, all made at install time and all changeable afterwards. Defaults
are in **bold**.

| decision | variable | values | what it changes |
| --- | --- | --- | --- |
| namespace | `WVA_NS` | any (**`workload-variant-autoscaler-system`**) | where the controller runs |
| scope | `WVA_SCOPE` | `cluster` / `namespace` (**platform default**) | which namespaces it may manage |
| GPU limiter | `WVA_LIMITER` | **`none`** / `gpu-inventory` / `quota` | whether scaling is bounded |
| scale-to-zero | `ENABLE_SCALE_TO_ZERO` | **`true`** / `false` | whether a model may idle to zero (but `make deploy-e2e-infra` defaults it OFF — see [Deployment Flags](#deployment-flags-installsh)) |

```bash
export HF_TOKEN="hf_xxxxxxxxxxxxx"

# Simplest: cluster-wide, default namespace, unbounded
make deploy-wva-on-k8s

# A team's own namespace, managing only that namespace, bounded by real GPUs
make deploy-wva-on-k8s WVA_NS=team-a-wva WVA_SCOPE=namespace WVA_LIMITER=gpu-inventory
```

Pass the same `WVA_NS` and `WVA_SCOPE` to `undeploy-wva-on-*`: an uninstall
resolves the overlay exactly as the install did, so a mismatch leaves behind
precisely the resources the other overlay owns.

### Scope: what the controller may manage

| scope | RBAC | manages | use when |
| --- | --- | --- | --- |
| `cluster` | ClusterRole | every namespace | one WVA for the whole cluster |
| `namespace` | Role | its own namespace | a tenant without cluster-wide RBAC, or one WVA per team |

Both work on both platforms — `config/overlays/` carries all four combinations.
The default preserves the historical behaviour: `namespace` on OpenShift,
`cluster` elsewhere.

### Before you turn on the GPU limiter

The shipped configuration declares **no limiter**: a fresh install scales
unconstrained, and a scale-from-zero wake is published without a capacity check.
That default is deliberate. A GPU-aware optimizer allocates out of per-accelerator
pools, so a variant whose accelerator it cannot resolve is charged to no pool, gets
no budget, and **never scales up** — silently, because nothing errors. Enabling it
by default would freeze exactly the workloads that are least carefully configured.

WVA resolves a variant's accelerator from, in order:

1. a **GPU product key in the workload's `nodeSelector` or `nodeAffinity`** — the
   only source that works before any pod exists, and therefore the only one that
   works for a workload parked at zero;
2. the **node its pods are running on**, once it has ready pods.

On a **single-accelerator** cluster neither is needed: there is one pool, so an
unconstrained workload is deduced onto it. On a **heterogeneous** cluster it
matters, and not as bookkeeping — a pod with no GPU nodeSelector can be scheduled
onto *any* GPU node, so a workload that does not state its accelerator genuinely
has not chosen one. Pin it:

```yaml
# in the model server's pod template
spec:
  nodeSelector:
    nvidia.com/gpu.product: NVIDIA-A100-PCIE-80GB
```

```bash
kubectl get nodes -L nvidia.com/gpu.product          # what your nodes advertise
kubectl logs -n workload-variant-autoscaler-system   -l app.kubernetes.io/name=workload-variant-autoscaler | grep -i "Accelerator not resolved"
```

The install warns too: enabling `WVA_LIMITER=gpu-inventory` counts the distinct GPU
products the cluster advertises and tells you whether pinning is needed. An
unresolved variant is logged once per change and counted in
`wva_unattributed_gpus`. See
[GPU Capacity Accounting](../docs/developer-guide/gpu-capacity-accounting.md).

### What the controller needs from a workload

WVA discovers a workload when **KEDA calls its external scaler** — being called is
what being managed means, so there is no annotation or label to add. The trigger
carries everything WVA needs per workload:

```yaml
triggers:
  - type: external
    metadata:
      scalerAddress: wva-external-scaler.workload-variant-autoscaler-system:9090
      modelID: meta/llama-70b        # required
      scalingPolicy: interactive     # optional: a policy tier
      variantCost: "30.0"            # optional
```

**`modelID` is the only required field.** Everything else about the workload is
derived: the accelerator from its `nodeSelector` (or the nodes its pods run on),
the role from its engine args or `llm-d.ai/role` label, GPUs per replica from its
resource requests, and the InferencePool from matching its pod-template labels
against each pool's selector. Nothing that can be observed is asked for.

`modelID` cannot be derived, and that is not an oversight: it is the grouping key
for every multi-variant decision, and a workload parked at zero has no pods and no
metrics to infer it from — which is exactly when it matters most.

## Deployment Methods

### Method 1: Automated Deployment Script (Recommended)

The deployment script provides a complete, automated setup including:

- WVA controller with RBAC configuration
- Prometheus stack (or connects to existing)
- llm-d infrastructure (Gateway, Scheduler, vLLM)
- KEDA for external metrics (ScaledObject-driven)
- ServiceMonitors for metric collection
- ScaledObjects are yours to create — they are how a workload reaches WVA
- Automatic GPU detection
- Environment-specific optimizations

#### Quick Start with Make

```bash
# Set required environment variable
export HF_TOKEN="hf_xxxxxxxxxxxxx"

# Deploy to Kubernetes
make deploy-wva-on-k8s

# Deploy to OpenShift
make deploy-wva-on-openshift

# Deploy to Kind (with emulated GPUs)
CREATE_CLUSTER=true make deploy-e2e-infra
```

#### Manual Script Execution

```bash
# Navigate to deploy directory
cd deploy

# Set environment variables
export HF_TOKEN="hf_xxxxxxxxxxxxx"
export ENVIRONMENT="kubernetes"  # or "openshift", "kind-emulator"

# Run deployment script
bash install.sh
```

#### Script Configuration Options

The script accepts both command-line flags and environment variables:

**Command-line flags** (`deploy/install.sh`):

```bash
bash install.sh [OPTIONS]

Options:
  -i, --wva-image IMAGE    WVA container image (default: ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest)
  -u, --undeploy           Undeploy WVA, monitoring, and scaler (not llm-d)
  -e, --environment ENV    kubernetes | openshift | kind-emulator
  -h, --help               Show help
```

**llm-d stack** (gateway, EPP, ModelService): deploy using the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) directly. For EPP-only setup (llm-d-router-standalone chart + tokenreview RBAC), use `deploy/install-epp.sh` after `install.sh`.

**Environment variables**: every option the script reads is tabulated in
[Configuration Reference](#configuration-reference).

#### Script deployment examples

**ScaledObjects** are not created by `install.sh` — create them with `kubectl apply`, or let your tests/operators manage them. KEDA creates and owns the HPA behind each one; you never create an HPA yourself.

##### Example 1: Base WVA infra + EPP

```bash
./deploy/install.sh -e kubernetes
# EPP (llm-d-router-standalone chart + RBAC):
LLM_D_ROUTER_VERSION=v0.9.0 GAIE_VERSION=v1.5.0 LLMD_NS=llm-d-optimized-baseline \
  ./deploy/install-epp.sh
# Model server: follow llm-d/llm-d guides/optimized-baseline
```

##### Example 2: E2E-style stack (same as `make deploy-e2e-infra`)

```bash
make deploy-e2e-infra ENVIRONMENT=kind-emulator IMG=localhost/llm-d-workload-variant-autoscaler:dev
```

##### Example 3: WVA + monitoring only (no llm-d)

```bash
export DEPLOY_WVA=true
export DEPLOY_PROMETHEUS=true
export DEPLOY_OPERATIONAL_DASHBOARD=true
export SCALER_BACKEND=keda
./deploy/install.sh -e kubernetes
```

##### Example 4: Install with LeaderWorkerSet (for full e2e suite)

```bash
export DEPLOY_LWS=true
./deploy/install.sh -e kubernetes
```

### Method 2: Kustomize (direct controller install)

Install the WVA controller directly into an existing cluster using Kustomize. This is the recommended method when you already have Prometheus and want to manage the controller install without the full automated script.

Scope, namespace and limiter are the same four decisions as above — see
[Choosing your install](#choosing-your-install). This method just applies the
overlays by hand instead of through `make`.

#### Applying the overlays directly

```bash
# Set the controller image
cd config/base/manager
kustomize edit set image controller=ghcr.io/llm-d/llm-d-workload-variant-autoscaler:v0.7.0

# Apply the overlay for your scope and platform
kubectl apply -k ../../overlays/cluster-scoped/kubernetes
#             ../../overlays/namespace-scoped/kubernetes
#             ../../overlays/cluster-scoped/openshift
#             ../../overlays/namespace-scoped/openshift
```

#### Undeploy

```bash
kubectl delete -k config/overlays/cluster-scoped/kubernetes    # match the overlay you installed
```

## Platform-Specific Guides

For platform-specific instructions and considerations:

- **[Kubernetes Guide](kubernetes/README.md)**: Detailed Kubernetes-specific instructions including kube-prometheus-stack setup, GPU operator installation, and ServiceMonitor configuration
- **[OpenShift Guide](openshift/README.md)**: OpenShift-specific instructions including User Workload Monitoring (Thanos), Routes, Security Context Constraints (SCC), and GPU operator on OpenShift
- **[Kind Guide (Local Testing)](kind-emulator/README.md)**: Local development and testing with Kind clusters and emulated GPUs

Each guide includes platform-specific examples, troubleshooting, and quick start commands. All guides use the same [Configuration Reference](#configuration-reference) documented below.

## Configuration Reference

### Environment Variables (Script)

#### Required

| Variable | Description | Required For |
|----------|-------------|--------------|
| `HF_TOKEN` | HuggingFace token | llm-d deployment |

#### Core Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENVIRONMENT` | Deployment environment (`kubernetes` or `openshift`) | `kubernetes` |
| `WVA_SCOPE` | `cluster` or `namespace` — see [Scope](#scope-what-the-controller-may-manage) | `namespace` on OpenShift, `cluster` elsewhere |
| `WVA_LIMITER` | `none`, `gpu-inventory` or `quota` — declares the limiter in the scaling-policy ConfigMap | `none` |
| `WVA_PROJECT` | Repository root the script installs from | `$PWD` |
| `CONTROLLER_INSTANCE` | Instance name for running several WVAs on one cluster | `""` (single instance) |

#### Image Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_IMAGE_REPO` | WVA image repository | `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` |
| `WVA_IMAGE_TAG` | WVA image tag | `latest` |
| `WVA_IMAGE_PULL_POLICY` | Image pull policy | `Always` |

#### Namespace Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_NS` | WVA controller namespace | `workload-variant-autoscaler-system` |
| `MONITORING_NAMESPACE` | Prometheus namespace | `workload-variant-autoscaler-monitoring` |
| `LLMD_NS` | llm-d namespace | `llm-d-optimized-baseline` |

#### Deployment Flags (`install.sh`)

| Variable | Description | Default |
|----------|-------------|---------|
| `DEPLOY_PROMETHEUS` | Deploy Prometheus stack | `true` |
| `DEPLOY_OPERATIONAL_DASHBOARD` | Deploy Grafana and operational dashboard | `true` |
| `DEPLOY_WVA` | Deploy WVA controller | `true` |
| `DEPLOY_LWS` | Deploy LeaderWorkerSet (needed only for full e2e suite; skip for smoke, benchmarks, or pre-installed clusters) | `false` |
| `DEPLOY_ALERTING_RULES` | Install the PrometheusRule alerts | `false` |
| `ENABLE_SCALE_TO_ZERO` | Allow a model to be parked at zero replicas, and enable the EPP `flowControl` gate that makes waking it possible | `true` |
| `SKIP_CHECKS` | Skip prerequisite checks | `false` |
| `SCALER_BACKEND` | `keda` or `none` (use a pre-installed backend) | `keda` |
| `KEDA_NAMESPACE` | Namespace KEDA is installed in | `keda-system` |
| `KEDA_HELM_INSTALL` | Install KEDA with Helm rather than assuming it is present | `false` |
| `KEDA_CHART_VERSION` | KEDA Helm chart version | `2.19.0` |
| `UNDEPLOY` | Remove instead of install (`install.sh` doubles as the uninstaller) | `false` |
| `DELETE_NAMESPACES` | With `UNDEPLOY=true`, also delete the namespaces | `false` |

> `make deploy-e2e-infra` passes `ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED)`,
> whose Makefile default is **`false`** — the opposite of `install.sh`'s. So an
> e2e deploy has scale-to-zero OFF unless you pass
> `SCALE_TO_ZERO_ENABLED=true`, while a plain `make deploy-wva-on-k8s` has it ON.
> Three e2e suites skip silently when it is off.

ScaledObjects, HPA stabilization (`spec.advanced.horizontalPodAutoscalerConfig.behavior`) and vLLM ModelService tuning are not controlled by `install.sh`; manage them via `kubectl apply` directly (see the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) for reference manifests).

#### Advanced (`install.sh`)

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_TLS_VERIFY` | Skip Prometheus TLS verification | `false`, forced to `true` on OpenShift and for in-cluster self-signed Prometheus |
| `WVA_LOG_LEVEL` | WVA logging level | `info` |
| `WVA_METRICS_SECURE` | Serve the WVA metrics endpoint over HTTPS with authn/authz | `true` |
| `PROMETHEUS_SECRET_NAME` | Secret holding the Prometheus serving cert | `prometheus-web-tls` |
| `PROMETHEUS_SECRET_NS` | Namespace of that secret | `$MONITORING_NAMESPACE` |
| `PROM_CA_CERT_PATH` | Where the extracted Prometheus CA is written | `/tmp/prometheus-ca.crt` |
| `GATEWAY_API_VERSION` | Gateway API version installed for llm-d | `v1.2.0` |
| `LWS_NAMESPACE` | Namespace for LeaderWorkerSet installation | `lws-system` |
| `LWS_CHART_VERSION` | LeaderWorkerSet Helm chart version | `0.8.0` |

#### Optional: scaling band after `make deploy-e2e-infra`

If `SCALE_UP_THRESHOLD` and/or `SCALE_DOWN_BOUNDARY` are set in the environment, the Makefile patches the `wva-scaling-policy-config` ConfigMap after install. Note the patch replaces the whole `default` entry, so it writes `analyzerName: saturation` alongside the band.

## Post-Deployment

### Verification

```bash
NS=workload-variant-autoscaler-system
kubectl get pods -n $NS                       # the controller is Running
kubectl get scaledobject -A                   # your managed workloads
kubectl get hpa -A                            # KEDA created one per ScaledObject
```

A ScaledObject with a KEDA HPA whose `CurrentMetrics` is populated means the whole
chain works: WVA was called, decided, and KEDA received the answer. An empty
`CurrentMetrics` means KEDA never got one — check the trigger's `scalerAddress`
and that `modelID` is set.

If metrics are the problem, follow them forward:

```bash
# 1. the model server exposes them
kubectl port-forward -n <llm-namespace> <vllm-pod> 8000:8000
curl -s http://localhost:8000/metrics | grep vllm:

# 2. Prometheus scrapes them  (query vllm:num_requests_running)
kubectl port-forward -n <monitoring-namespace> svc/prometheus-k8s 9090:9090

# 3. WVA reads them and decides
kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler   | grep -E "Collected replica metrics|scaling-decision"
```

### Monitoring WVA

The log lines worth knowing, all at Info:

| grep for | tells you |
| --- | --- |
| `scaling-decision` | what WVA decided for a model, and the replica counts |
| `Effective scaling policy` | which policy tier a model resolved to |
| `Collected replica metrics` | metrics are arriving |
| `GPU limiter (re)built from config` | a `limiters:` edit took effect, live |

```bash
kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler -f
```

WVA writes no custom resource, so its decisions are visible only in these logs, in
the metrics it publishes ([Prometheus metrics](../docs/developer-guide/prometheus.md)),
and in the HPA state KEDA derives from them.

### Testing autoscaling

Drive load at the model and watch the ScaledObject and its HPA react. Full
procedures, including the simulator and the e2e suites, are in
[Testing](../docs/developer-guide/testing.md).

## Troubleshooting

| symptom | most likely cause | check |
| --- | --- | --- |
| WVA pod not `Running` | image pull, resources, or Prometheus unreachable | `kubectl describe pod -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler` |
| "Metrics unavailable" in the logs | the ServiceMonitor does not select your model pods, so the series never reach Prometheus | `kubectl get servicemonitor -A`, then Prometheus `/targets` |
| HPA exists but `CurrentMetrics` is empty | KEDA never got an answer — usually the trigger's `scalerAddress` or a missing `modelID` | `kubectl describe hpa -n <ns> keda-hpa-<so-name>` |
| nothing scales, no errors | a limiter is declared and the workload's accelerator does not resolve, so it gets no GPU budget | `kubectl logs -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler \| grep -i accelerator` |
| a model never wakes from zero | the EPP flow-control queue is not reaching WVA | see [Troubleshooting](../docs/developer-guide/troubleshooting.md) |

First stop for any of these:

```bash
kubectl logs -n workload-variant-autoscaler-system   -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200
```

Deeper diagnosis — EPP metrics, scale-from-zero, slow scale-up — is in
[Troubleshooting](../docs/developer-guide/troubleshooting.md).

### Useful Commands Cheatsheet

```bash
# === WVA Controller ===
kubectl get pods -n workload-variant-autoscaler-system
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler -f
kubectl describe deployment controller-manager -n workload-variant-autoscaler-system

# === Managed workloads (a ScaledObject IS the registration) ===
kubectl get scaledobject -A
kubectl describe scaledobject <name> -n <namespace>

# === Metrics and Monitoring ===
kubectl get servicemonitor -A
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1" | jq
kubectl port-forward -n <monitoring-namespace> svc/prometheus-k8s 9090:9090

# === ScaledObjects / HPA ===
kubectl get scaledobject -A
kubectl describe scaledobject <name> -n <namespace>
kubectl get hpa -A
kubectl describe hpa <name> -n <namespace>

# === KEDA ===
kubectl get pods -n keda-system
kubectl logs -n keda-system -l app=keda-operator

# === vLLM / Application ===
kubectl get pods -n <app-namespace>
kubectl logs -n <app-namespace> <vllm-pod>
kubectl port-forward -n <app-namespace> <vllm-pod> 8000:8000

# === Configuration ===
kubectl get configmap -n workload-variant-autoscaler-system
kubectl get configmap service-classes -n workload-variant-autoscaler-system -o yaml
kubectl get configmap model-accelerator-data -n workload-variant-autoscaler-system -o yaml
```

## Additional Resources

- **Main Project**: [README.md](../README.md)
- **Kubernetes Guide**: [kubernetes/README.md](kubernetes/README.md)
- **OpenShift Guide**: [openshift/README.md](openshift/README.md)
- **Kustomize overlays**: [config/overlays/cluster-scoped/kubernetes](../config/overlays/cluster-scoped/kubernetes/), [config/overlays/namespace-scoped/openshift](../config/overlays/namespace-scoped/openshift/)
- **API Reference**: [api/v1alpha1](../api/v1alpha1/)
- **Architecture**: [docs/design/modeling-optimization.md](../docs/design/modeling-optimization.md)
