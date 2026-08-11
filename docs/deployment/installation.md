# Installing WVA

The two install methods, and the four decisions each one takes.

> Part of the [WVA deployment guide](../../deploy/README.md).

## Choosing your install

Four decisions, all made at install time and all changeable afterwards. Defaults
are in **bold**.

| decision | variable | values | what it changes |
| --- | --- | --- | --- |
| namespace | `WVA_NS` | any (**`workload-variant-autoscaler-system`**) | where the controller runs |
| scope | `WVA_SCOPE` | `cluster` / `namespace` (**platform default**) | which namespaces it may manage |
| GPU limiter | `WVA_LIMITER` | **`none`** / `gpu-inventory` / `quota` | whether scaling is bounded |
| scale-to-zero | `ENABLE_SCALE_TO_ZERO` | **`true`** / `false` | whether a model may idle to zero (but `make deploy-e2e-infra` defaults it OFF — see [Deployment Flags](configuration.md#deployment-flags-installsh)) |

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

### Bounding scaling with a GPU limiter

The shipped configuration declares **no limiter**, so a fresh install scales
unconstrained. Turning one on has a precondition that will silently freeze
workloads if you skip it — see
[Bounding scaling: the GPU limiter](gpu-limiter.md).

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
[Configuration Reference](configuration.md).

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
[Choosing your install](installation.md#choosing-your-install). This method just applies the
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

- **[Kubernetes Guide](../../deploy/kubernetes/README.md)**: Detailed Kubernetes-specific instructions including kube-prometheus-stack setup, GPU operator installation, and ServiceMonitor configuration
- **[OpenShift Guide](../../deploy/openshift/README.md)**: OpenShift-specific instructions including User Workload Monitoring (Thanos), Routes, Security Context Constraints (SCC), and GPU operator on OpenShift
- **[Kind Guide (Local Testing)](../../deploy/kind-emulator/README.md)**: Local development and testing with Kind clusters and emulated GPUs

Each guide includes platform-specific examples, troubleshooting, and quick start commands. All guides use the same [Configuration Reference](configuration.md) documented below.

