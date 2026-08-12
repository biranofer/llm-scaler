# Install WVA in your namespace

**The common path.** You own a namespace, your model servers run in it, and you
want WVA to size them. You do not need to be a cluster admin — you need one thing
from one, once.

> Part of the [WVA guides](../README.md).

## What you get

WVA watches how saturated your model servers are and tells KEDA how many replicas
each variant needs. KEDA owns the HPA and does the scaling. WVA never writes to
your workloads; its only write is Events.

It manages **only your namespace**. Another team's WVA cannot see your workloads,
and yours cannot see theirs.

## Environment

Source the shared environment, then name your namespace — or do not, and let the
install find it:

```bash
source docs/guides/env.sh
```

<!-- guide:env.static.namespace start -->
```bash
# The namespace WVA installs into and manages.
# Export it FIRST if you are a namespace admin: finding it automatically
# means listing workloads cluster-wide, which you are not permitted to do,
# and the check will tell you so rather than guess. With it exported,
# everything the check does is a read inside your own namespace.
# NAMESPACE is llm-d's own variable, so if you followed one of its guides
# it is already set.
export NAMESPACE=<your-namespace>
```
<!-- guide:env.static.namespace end -->

## Prerequisites

| you need | who provides it |
| --- | --- |
| KEDA | the install adds it if the cluster has none; platform-managed on OpenShift |
| Prometheus | detected. On OpenShift it is the platform's Thanos Querier, at a fixed address |
| model servers labelled `llm-d.ai/inferenceServing=true` | llm-d's own install does this |

Check all of it, read-only, before committing to anything:

<!-- guide:prerequisites.check start -->
```bash
# Read-only. Renders the manifests this install would apply, asks the API
# server whether you may create each kind, and reports the namespace it
# resolved and the Prometheus it found.
make check-prereqs-namespace-on-k8s
```
<!-- guide:prerequisites.check end -->

It answers the things you would otherwise guess at:

```
[SUCCESS] Namespace: my-llmd  (found: it is the only namespace running llm-d model servers)
[SUCCESS]   my-llmd holds 3 llm-d model server(s) for it to manage.
[SUCCESS] Prometheus: https://thanos-querier.openshift-monitoring.svc.cluster.local:9091
[INFO]      You do not need to pass PROMETHEUS_URL. Set it only to override this.
```

If the namespace it names is empty, it says so loudly — a namespace-scoped
controller pointed at the wrong namespace installs cleanly, reports healthy, and
scales nothing.

### One command from an admin

WVA needs a few cluster-scoped objects: the metrics filter issues TokenReviews,
EPP metrics need `nonResourceURLs`, and resolving which GPU a variant runs on
reads Nodes. None of that fits in a Role, so a namespace admin cannot create it —
and it is not something you should be granted just to install an autoscaler.

Send your admin this. It is **once per namespace**, not once per upgrade:

<!-- guide:prerequisites.admin start -->
```bash
# Run by a CLUSTER ADMIN, once per namespace. Creates the cluster-scoped RBAC
# and the ServiceMonitor that a namespace admin cannot create for themselves.
# See ../admin-cluster-setup/README.md.
make setup-prereqs-namespace-on-k8s WVA_NS=${NAMESPACE}
```
<!-- guide:prerequisites.admin end -->

## Deploy

<!-- guide:deploy.controller start -->
```bash
# Yours to run, now and for every upgrade, with no cluster-scoped rights.
# Add IMG=<your build> to install an unmerged branch.
make deploy-wva-namespace-on-k8s
```
<!-- guide:deploy.controller end -->

On OpenShift use `deploy-wva-namespace-on-openshift`. If the admin step has not
happened, this stops and names every missing object, so you have a list to hand
back rather than a permissions error.

### Register your workloads

**Nothing scales until you do this.** WVA has no watch and no listing: it learns
a workload exists only when KEDA calls it about one. Until then the controller
runs, reports healthy, and scales nothing.

<!-- guide:deploy.register start -->
```bash
# Nothing scales until a ScaledObject exists — WVA is only ever asked about
# workloads KEDA calls it about. The plan is an editable table; apply exactly
# what you leave in it.
make scaledobjects-plan
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=/tmp/wva-scaledobject-plan.XXXX
```
<!-- guide:deploy.register end -->

The plan lists one row per model server found, and applies nothing until you say
so:

```
#apply  namespace  kind        name          modelID     inferencePool       min  max
yes     team-a     Deployment  llama-decode  meta/llama  optimized-baseline  1    10
```

## Verify

<!-- guide:verify.objects start -->
```bash
# The HPA is KEDA's — it creates one per ScaledObject.
kubectl get scaledobject,hpa -n ${NAMESPACE}
```
<!-- guide:verify.objects end -->

**An HPA whose `CurrentMetrics` is populated means the whole chain works**: KEDA
called WVA, WVA decided, KEDA got the answer.

<!-- guide:verify.decisions start -->
```bash
# What the controller decided, and why.
kubectl logs -n ${NAMESPACE} deploy/wva-controller-manager | grep scaling-decision
```
<!-- guide:verify.decisions end -->

```
scaling-decision {"modelID":"meta/llama","decisions":[{"name":"llama-decode-wva","curr":1,"tgt":3,"action":"scale-up"}]}
```

## Upgrading

Run the deploy step again with a newer `IMG`. No admin needed — the prerequisites
stand until a WVA version changes what it needs cluster-wide, which is rare and
called out in release notes.

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
# Delete the ScaledObjects too unless you are reinstalling: their trigger
# points at a scaler that no longer exists, so KEDA keeps the HPA and keeps
# calling nothing, and a workload parked at zero can never be woken.
kubectl delete scaledobject --all -n ${NAMESPACE}
make undeploy-wva-on-k8s WVA_NS=${NAMESPACE} WVA_SCOPE=namespace
```
<!-- guide:cleanup.uninstall end -->

Your namespace and your workloads stay.

## What this guide does not give you

| not included | why, and what to do |
| --- | --- |
| a GPU bound | your controller scales to each workload's `maxReplicaCount` unless cluster policy bounds it. That policy is an admin's to publish — see [Bounding GPU usage](../admin-gpu-bounding/README.md) |
| cross-namespace scaling | by construction: this controller reads one namespace |
| control over your own limits | deliberate. WVA reads limiters from a namespace you cannot edit, so a bound placed on you holds |

## Next

- [After the install](../../deployment/operations.md) — what to watch, and the
  metrics that answer specific questions
- [Configuration reference](../../deployment/configuration.md)
- [Tuning how it scales](../../developer-guide/scaling-policy-config.md) —
  thresholds and policy tiers, which are yours to set
