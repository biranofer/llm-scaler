# Installing on a new cluster

The full stack: WVA, Prometheus, KEDA, and the namespaces to run llm-d in. Use this
when you are building a cluster up from nothing, or standing up a test environment.

> Part of the [WVA deployment guide](../../deploy/README.md).
>
> **Already running llm-d?** Do not use this page — it deploys a Prometheus and an
> llm-d namespace you do not want. See
> [Adding WVA to a cluster that already runs llm-d](existing-cluster.md).

## Choosing your install

Four decisions, all made at install time and all changeable afterwards. Defaults
are in **bold**.

| decision | variable | values | what it changes |
| --- | --- | --- | --- |
| namespace | `WVA_NS` | any (**`workload-variant-autoscaler-system`**) | where the controller runs |
| scope | `WVA_SCOPE` | `cluster` / `namespace` (**platform default**) | which namespaces it may manage |
| GPU limiter | `WVA_LIMITER` | **`none`** / `gpu-inventory` / `quota` | whether scaling is bounded |
| scale-to-zero | `ENABLE_SCALE_TO_ZERO` | **`true`** / `false` | whether a model may idle to zero (but `make deploy-e2e-infra` defaults it OFF — see [Deployment Flags](configuration.md#deployment-flags)) |

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

| scope | the controller reads | use when |
| --- | --- | --- |
| `cluster` | every namespace | one WVA for the whole cluster |
| `namespace` | only the namespace it is installed in | one WVA per team, each installed *in* the namespace with its models |

Both work on both platforms — `config/overlays/` carries all four combinations.
The default is `namespace` on OpenShift and `cluster` elsewhere.

> **Both scopes need cluster-admin to install.** `namespace` narrows what the
> controller *reads*, not what it is *granted*: either overlay creates 4
> ClusterRoles and 4 ClusterRoleBindings (6 on OpenShift).
>
> The grant is read-only — WVA never writes to the cluster, because KEDA performs
> the actuation. Its only write is Events. It reads nodes, pods, services,
> namespaces and `deployments/scale`, all `get`/`list`/`watch`.
>
> So `WVA_SCOPE=namespace` is **blast-radius reduction, not delegation**. A team
> lead without cluster rights cannot install it themselves; a cluster admin does the
> install, and the tenant gets a controller that only touches their namespace.

### How many WVAs a cluster has

WVA installs at one of two scopes, and a cluster uses one shape or the other:

| shape | what you get |
| --- | --- |
| **one cluster-scoped WVA** | a single controller managing model servers in every namespace |
| **one namespace-scoped WVA per namespace** | a controller per team, each managing only the namespace it is installed in |

They do not mix. A cluster-scoped controller already covers every namespace, so a
namespace-scoped one beside it would be a second controller for the same workloads;
and a cluster-scoped one added later covers the namespaces the others were handling.
The install checks for this and stops, naming what it found:

```
[ERROR] A cluster-scoped WVA is already installed: workload-variant-autoscaler-system/wva-controller-manager.
```

`make check-prereqs` runs the same check, so you can ask before installing.
Installing into a namespace that already has one is an upgrade, and is allowed.

#### What keeps two namespace-scoped controllers apart

Three things, none of which you configure:

- **Workloads.** Each install has its own `wva-external-scaler.<namespace>` Service,
  and a workload registers with the WVA whose address its trigger names. Two
  controllers never see the same workload.
- **Permissions.** Each install's ClusterRoleBindings are suffixed with a hash of
  its namespace, so they cannot take each other's.
- **Reach.** A namespace-scoped controller restricts its cache to its own namespace
  and cannot read a workload outside it.

The namespace is the identity; there is nothing else to name.

#### What they do share: GPUs

Each controller computes free GPU capacity from the same nodes and allocates against
it without seeing the others' claims, so several can oversubscribe one pool — which
surfaces as pods that will not schedule. If your tenants share GPUs, bound each
install with a quota limiter; if they do not, give each its own GPU nodes.

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

### Method 2: Kustomize (GitOps and direct installs)

The overlays are plain Kustomize, so you can apply them yourself — for Argo CD or
Flux, or to review exactly what lands. Scope, namespace and limiter are the same
decisions as above; see [Choosing your install](#choosing-your-install).

**The overlays alone are not a complete install.** `deploy/install.sh` does four
things around them that the raw apply does not, and each one has bitten somebody:

| What the script does | If you skip it |
| --- | --- |
| Writes `PROMETHEUS_URL` into the controller ConfigMap | The controller starts, looks healthy, and reads no metrics — it uses the shipped in-cluster default, which is not your Prometheus |
| Renames the shared ClusterRoleBindings per install | A second install **takes the bindings from the first**, which silently loses all its permissions |
| Checks no incompatible WVA is already installed | Two controllers scaling the same workloads |
| Refuses to delete the namespace and shared ClusterRoles on undeploy | `kubectl delete -k` removes the **Namespace** — taking the model servers in it — and the shared **ClusterRoles**, breaking every other WVA on the cluster |

So use this method when you are installing **one** WVA and wiring Prometheus
yourself, or when a GitOps tool owns the manifests. Otherwise prefer Method 1.

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

Then point the controller at your Prometheus — without this it will not collect
anything:

```bash
# NOTE: the overlays hardcode `namespace: wva-system`. Only deploy/install.sh
# rewrites that to $WVA_NS, so a raw `apply -k` lands in wva-system.
kubectl -n wva-system edit configmap wva-manager-config
# under config.yaml, set:
#   prometheus:
#     baseURL: https://<your-prometheus>.<ns>.svc.cluster.local:9090
kubectl -n wva-system rollout restart deployment wva-controller-manager
```

If you will run **more than one** WVA on this cluster, rename the shared
ClusterRoleBindings in your overlay first — otherwise the second install takes them
from the first. `deploy/lib/common.sh:wva_append_crb_name_patches` is the patch set
the script generates; the names are listed in `WVA_SHARED_CLUSTER_ROLE_BINDINGS`.

#### Undeploy

`kubectl delete -k` is **not** the inverse of the apply: the overlay contains a
Namespace and the shared ClusterRoles, and deleting those takes the workloads in
that namespace and the permissions of every other WVA with it. Delete the
install's own objects and leave the shared ones:

```bash
kubectl kustomize config/overlays/cluster-scoped/kubernetes \
  | yq 'select(.kind != "Namespace" and .kind != "ClusterRole")' \
  | kubectl delete -f - --ignore-not-found
```

Or just use `./deploy/install.sh --undeploy`, which does exactly this.


## Platform-specific guides

For platform-specific instructions and considerations:

- **[Kubernetes Guide](../../deploy/kubernetes/README.md)**: Detailed Kubernetes-specific instructions including kube-prometheus-stack setup, GPU operator installation, and ServiceMonitor configuration
- **[OpenShift Guide](../../deploy/openshift/README.md)**: OpenShift-specific instructions including User Workload Monitoring (Thanos), Routes, Security Context Constraints (SCC), and GPU operator on OpenShift
- **[Kind Guide (Local Testing)](../../deploy/kind-emulator/README.md)**: Local development and testing with Kind clusters and emulated GPUs

Each guide includes platform-specific examples, troubleshooting, and quick start commands. All guides use the same [Configuration Reference](configuration.md) documented below.

