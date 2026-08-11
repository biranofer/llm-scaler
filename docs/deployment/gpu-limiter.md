# Bounding scaling: the GPU limiter

WVA scales without a GPU budget unless you give it one. This is how, and what has
to be true first.

> Part of the [WVA deployment guide](../../deploy/README.md).

## Turning it on

```bash
make deploy-wva-on-k8s WVA_LIMITER=gpu-inventory   # bound by GPUs actually free
make deploy-wva-on-k8s WVA_LIMITER=quota           # bound by declared caps
```

or later, by adding a `limiters:` entry to the `default` entry of the
scaling-policy ConfigMap — applied live, no restart. **Read the next section
first.**

## Who is allowed to change the bound

By default the limiter is read from the scaling-policy ConfigMap in the
controller's own namespace. For a **cluster-scoped** install that namespace belongs
to a cluster admin, so the bound and its subject already have different owners, and
there is nothing more to arrange.

It is the **namespace-scoped** install that needs care. There the controller runs
in the namespace it manages, so whoever administers that namespace administers the
controller — its Deployment, its args, its env, its ServiceAccount. Nothing carried
on the controller can bound the person who can edit the controller. A quota its
subject can edit is not a quota.

So WVA does not accept its limits from anything that person can write.

### What an admin sets, and where

Everything authoritative is a **cluster-scoped object**: a namespace admin holds
RBAC *inside* their namespace and can neither create a `Namespace` nor edit one's
annotations.

```bash
# Once per cluster. Every WVA on the cluster reads this, and none can opt out.
kubectl create namespace wva-policy
kubectl -n wva-policy create configmap wva-scaling-policy-config \
  --from-file=default=cluster-limiters.yaml

# Let a tenant's controller read it (read only — never write)
kubectl -n wva-policy create role wva-policy-reader \
  --verb=get,list,watch --resource=configmaps
kubectl -n wva-policy create rolebinding team-a-wva \
  --role=wva-policy-reader \
  --serviceaccount=team-a:wva-controller-manager
```

Per-namespace variations are annotations on the tenant's `Namespace`:

```bash
# this namespace draws its limits from somewhere else
kubectl annotate namespace team-a wva.llmd.ai/policy-namespace=platform-policy

# or: this namespace is explicitly allowed to scale unbounded
kubectl annotate namespace team-b wva.llmd.ai/unbounded=allowed
```

### What the controller does with that

Policy is resolved once at startup, in order:

1. `wva.llmd.ai/policy-namespace` on the managed Namespace
2. `WVA_POLICY_NS` given at install time
3. the `wva-policy` namespace, if it exists *and* the controller manages its own
   namespace
4. the controller's own namespace

Limiters written in the controller's own ConfigMap are then ignored *and logged* —
a limiter that reads as enforcing and enforces nothing is worse than either
enforcing or erroring. Thresholds, tiers and per-model settings stay with the team
running the workload: those are tuning, not entitlement.

If the policy ConfigMap **exists but cannot be read** — usually a missing
RoleBinding — the controller refuses to start rather than run unbounded while an
admin believes a quota applies.

### Read this before relying on it

**This is a guardrail, not an enforcement boundary.**

If the tenant owns the controller's Deployment — which they do whenever the
controller runs *inside* the namespace it manages — then they own its args, its
env, its ServiceAccount and its image. No rule evaluated inside that process can
bind them. An earlier design here tried to refuse to start in that situation; it
was withdrawn because it did not work (the condition it keyed on came from a flag
on the Deployment the tenant controls) and because it broke every namespace-scoped
install, which is the default on OpenShift.

What this mechanism honestly buys you:

- the GPU budget is authoritative for every controller *actually running WVA* —
  covering misconfiguration, drift, and copied manifests, which is most real
  incidents;
- an admin can direct a controller they did not deploy, via an annotation on a
  cluster-scoped object.

**For a tenant you do not trust, enforce at admission instead.** A `ResourceQuota`
on the GPU resource cannot be argued with by anything inside the namespace, and it
bounds every path — not just WVA:

```bash
kubectl -n team-a create quota gpus --hard=requests.nvidia.com/gpu=8
```

Note also that WVA's limiter bounds what *WVA* asks for. The ScaledObject is the
tenant's: they can raise `maxReplicaCount` or add a second KEDA trigger, and the
HPA takes the maximum across triggers. The quota is what stops that.

### The arrangement where the bound does hold

Run the controller **outside** the namespace it manages, so the tenant never owns
it:

```bash
make deploy-wva-on-k8s WVA_SCOPE=namespace \
  WVA_NS=wva-team-a \        # admin-owned: where the controller runs
  WVA_WATCH_NS=team-a        # tenant-owned: what it manages
```

This is the recommended multi-tenant shape. Everything above then applies with the
Deployment out of the tenant's reach.

## Why it ships off

The shipped configuration declares **no limiter**: a fresh install scales
unconstrained, and a scale-from-zero wake is published without a capacity check.
That default is deliberate. A GPU-aware optimizer allocates out of per-accelerator
pools, so a variant whose accelerator it cannot resolve is charged to no pool, gets
no budget, and **never scales up** — silently, because nothing errors. Enabling it
by default would freeze exactly the workloads that are least carefully configured.

## What has to be true first: every accelerator must resolve

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

## Permission: nodes

The limiter reads **nodes** to learn what GPUs exist, and nodes are cluster-scoped.

WVA reads nodes on every cycle regardless of the limiter — a variant's accelerator
is resolved from the nodes its pods run on, and that identity is what the capacity
model keys learned per-replica capacity by. What the limiter changes is whether
that identity is also used to charge the variant to a GPU **budget**.

The consequence of a missing node permission therefore depends on the limiter:

| limiter | controller can list nodes | outcome |
| --- | --- | --- |
| `none` | no | degraded: accelerators stay unresolved, so metrics lose the accelerator label and the capacity model cannot reuse learned capacity across variants |
| `gpu-inventory` | no | **install refused.** Every variant would be charged to no pool, receive no budget and stop scaling up, silently |
| `gpu-inventory` | yes | as intended |

`make check-prereqs WVA_LIMITER=gpu-inventory` checks this before you install.

### If the limiter is turned on later

An admin can add a `limiters:` entry to the scaling-policy ConfigMap at any time,
and it is applied live — including to a controller that was installed without node
permission. That combination is the one failure with no natural symptom, so it is
reported three ways:

- an **error log**, naming what has stopped working;
- `wva_node_access_denied` set to **1**;
- the **`WVANodeAccessDenied`** alert (critical, fires after 2m) if you installed
  with `DEPLOY_ALERTING_RULES=true`.

## Checking

The install warns too: enabling `WVA_LIMITER=gpu-inventory` counts the distinct GPU
products the cluster advertises and tells you whether pinning is needed. An
unresolved variant is logged once per change and counted in
`wva_unattributed_gpus`. See
[GPU Capacity Accounting](../developer-guide/gpu-capacity-accounting.md).

