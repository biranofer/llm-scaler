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

A controller that manages the namespace it runs in resolves, in order:

1. `wva.llmd.ai/policy-namespace` on that Namespace
2. the `wva-policy` namespace, if it exists
3. `wva.llmd.ai/unbounded=allowed` on that Namespace
4. **otherwise it refuses to start**

Step 4 is the point. "No policy found" never quietly becomes "no limits" — running
unbounded is always something an admin decided, never something that happened
because configuration was missing. The startup error names all four routes out, so
whoever can fix it is told exactly what to do.

Note that the fixed name in step 2 outranks any `WVA_POLICY_NS` given at install
time. That inversion is deliberate: install-time settings live on the controller's
Deployment, which the tenant owns, so a name they cannot configure is a name they
cannot redirect.

It also refuses to start if it cannot **read** the policy that applies to it —
usually a missing RoleBinding. Starting would run the tenant unbounded while an
admin had every reason to believe a quota was in force.

### What stays with the tenant

- **limiters and quotas** — admin only. A `limiters:` block in the tenant's own
  ConfigMap is ignored *and logged*: a limiter that reads as enforcing and enforces
  nothing is the worst of the three outcomes.
- **thresholds, tiers and per-model settings** — the tenant's. Those are tuning,
  not entitlement, and the team running the workload should own them.

### The honest limit

None of this contains a hostile tenant. Anyone who can change the controller's
**image** runs code that consults none of it. What this gives you is that the GPU
budget is authoritative for every WVA that is actually running WVA — which covers
misconfiguration, drift, and copied manifests, the things that actually happen.

For enforcement against a tenant you do not trust, use Kubernetes' own admission
path — a `ResourceQuota` on the GPU resource in their namespace — and let WVA's
limiter be what keeps it from ever getting there.

The stronger arrangement, if you want both: run the controller **outside** the
namespace it manages, so the tenant never owns it at all.

```bash
make deploy-wva-on-k8s WVA_SCOPE=namespace \
  WVA_NS=wva-team-a \        # admin-owned: where the controller runs
  WVA_WATCH_NS=team-a        # tenant-owned: what it manages
```

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

