# Bounding GPU usage across the cluster

**For a cluster admin.** WVA scales without a GPU budget unless you give it one.
This is the one command that gives it one, and what it does to the cluster.

> Part of the [WVA deployment guide](../../../deploy/README.md).
> The reasoning behind the design: [the GPU limiter](../../deployment/gpu-limiter.md).

## The command

```bash
make enable-physical-limiter
```

Every WVA on the cluster is now bounded by the GPUs that are actually free —
allocatable minus what is already requested, counting pods WVA does not manage.
No controller can opt out.

```bash
make disable-physical-limiter          # back to unbounded
make enable-physical-limiter WVA_LIMITER_TYPE=quota   # bound by declared caps instead
```

## What it actually does

Declaring the limiter is a ConfigMap. Three things around it are what make it
work, and each one, left out, fails as something else:

1. **The policy is published where a tenant cannot edit it.** A limit its subject
   can change is not a limit.
2. **Every controller is granted read access to it.** A controller that can see
   the policy exists but cannot read it refuses to start, rather than run
   unbounded while you believe a limit applies.
3. **Every controller is granted the node read.** This is the one with blast
   radius: a GPU-aware optimizer that cannot resolve a variant's accelerator
   charges it to no pool, gives it no budget, and it never scales up again —
   silently. So the grants happen **first**, to every controller found on the
   cluster, and only then is the policy published.

Controllers are **discovered**, not configured, because the hazard is the install
you forgot. `WVA_LIMITER_TARGETS="ns1 ns2"` narrows it, and then the ones left out
are named in a warning — they will report NotReady, because they are now reading a
policy they cannot honour.

## Where the policy lands

Not always the same place, and the command works this out per controller:

| the controller | reads policy from |
| --- | --- |
| namespace-scoped, managing its own namespace | the well-known `wva-policy` namespace |
| cluster-scoped | **its own** ConfigMap |
| namespace-scoped, pointed at someone else's namespace | **its own** ConfigMap |

The second and third are deliberate. If `wva-policy` won there, creating that
namespace for one tenant would silently switch a correctly bounded cluster-scoped
install to a different source — and if that source had no limiter yet, from
bounded to **unbounded**, with no edit to the install that changed.

One consequence to know: policy-*namespace* resolution runs once, at controller
startup. A controller that came up before `wva-policy` existed is reading its own
ConfigMap and will never look again, so the command restarts exactly those — and
only those, since restarting controllers that do not need it is its own outage.

The ConfigMap contents, by contrast, are live: an edit reaches every controller
within a reconcile, with no restart.

## Before you turn it on: accelerators must resolve

A GPU-aware optimizer allocates out of per-accelerator pools. A variant whose
accelerator it cannot resolve gets no budget and stops scaling up. WVA resolves it
from, in order:

1. a **GPU product key in the workload's `nodeSelector`/`nodeAffinity`** — the
   only source that works before any pod exists;
2. the **nodes its running pods are on**, when placement is unconstrained and at
   least one replica is ready.

So the workloads most at risk are the ones with no GPU nodeSelector and no running
pods. Check before enabling:

```bash
kubectl logs -n <wva-namespace> deploy/wva-controller-manager | grep -i AcceleratorNotResolved
```

WVA also emits an `AcceleratorNotResolved` **warning Event** per variant that does
not resolve. If any appear, fix the workloads first — this is exactly the failure
mode that has no natural symptom.

## What it bounds, and what it does not

**A guardrail, not an enforcement boundary.** Whoever owns a controller's
Deployment owns its args, env and image, so a tenant running WVA *inside their own
namespace* can change what bounds them. What this guarantees is that the GPU
budget is authoritative for every controller actually running WVA — which covers
misconfiguration, drift and copied manifests, the things that happen far more
often than malice.

It also bounds only what **WVA** asks for. The ScaledObject belongs to the
workload's owner: raising `maxReplicaCount`, or adding a second KEDA trigger,
bypasses it entirely, because the HPA takes the maximum across triggers.

**For a tenant you do not trust, bound the namespace at admission instead** —
there it cannot be argued with:

```bash
kubectl -n team-a create quota gpus --hard=requests.nvidia.com/gpu=8
```

And run their controller [outside their namespace](../admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach),
so the Deployment that bounds them is not one they can edit.

## Declare one kind, not both

`limiters:` is a list and reads like a set of bounds that all apply. It is not.
One limiter is built, and a quota entry wins: declaring `quota` alongside
`gpu-inventory` gives you the declared caps and **no physical limiter at all**, so
nothing checks whether the GPUs exist. WVA names what it dropped (`notEnforced`)
at ConfigMap parse and at startup rather than doing it silently. Bounding by
`min(physical, quota)` is [issue #1003](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1003).

## Checking it took

```bash
# every controller should still be Ready — one that is not is one this missed
kubectl get pods -A -l app.kubernetes.io/name=workload-variant-autoscaler

# and the budget it is now working against
kubectl logs -n <wva-namespace> deploy/wva-controller-manager | grep "GPU budgets available"
```

```
GPU budgets available for placement {"namespace":"team-a","gpuBudgets":{"A100":0,"H100":4},"gpusInUse":{"NVIDIA-A100-PCIE-80GB":4}}
```

## Next

- [The GPU limiter](../../deployment/gpu-limiter.md) — why policy lives where it does, what the
  controller does when it cannot honour it, and the quota form
- [Cluster-admin setup](../admin-cluster-setup/README.md) — per-namespace prerequisites
