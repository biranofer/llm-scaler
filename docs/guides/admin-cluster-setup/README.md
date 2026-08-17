# Cluster-admin setup for a namespace

## Overview

Creates, for one namespace, the things a namespace admin cannot: the namespace
itself, the cluster-scoped RBAC, and the ServiceMonitor. After this, that
namespace's owner installs and upgrades the controller with no cluster-scoped
rights, and you are not in the loop again unless a WVA release changes what it
needs cluster-wide.

## Prerequisites

Cluster-admin rights, and the namespace you are preparing:

<!-- guide:env.static.namespace start -->
```bash
export NAMESPACE=team-a
```
<!-- guide:env.static.namespace end -->

<!-- guide:prerequisites.check start -->
```bash
make check-prereqs
```
<!-- guide:prerequisites.check end -->

## Installation Instructions

<!-- guide:deploy.prereqs start -->
```bash
make setup-prereqs
```
<!-- guide:deploy.prereqs end -->

Once per namespace, not once per upgrade.

### What it creates, and why it needs you

| object | why a namespace admin cannot create it |
| --- | --- |
| the namespace | cluster-scoped |
| `wva-metrics-auth-role` + binding | the metrics filter issues **TokenReview** |
| `wva-metrics-reader` + binding | lets Prometheus scrape the controller |
| `wva-epp-metrics-reader-role` + binding | needs `nonResourceURLs: /metrics` |
| `wva-node-reader-role` + binding | resolving a variant's accelerator reads **Nodes** |
| the ServiceMonitor | the stock `admin` ClusterRole does not grant `monitoring.coreos.com` |
| the controller's `Role` + `RoleBinding` | namespaced, but RBAC forbids granting permissions you do not hold yourself, and the controller's Role names resources a namespace admin does not have |
| Prometheus / KEDA | only if the cluster has none |

Every cluster-scoped object is named with a hash of the namespace, so two
installs cannot take each other's and an uninstall removes only its own.

### Keeping the controller out of the tenant's reach

By default the controller runs *in* the namespace it manages, so whoever
administers that namespace can edit its Deployment — and **nothing carried on the
controller can bound the person who can edit the controller**. Where the bound
must hold, run it in a namespace you own:

<!-- guide:deploy.out_of_reach start -->
```bash
make setup-prereqs   WVA_NS=wva-${NAMESPACE} WVA_WATCH_NS=${NAMESPACE}
make deploy-wva      WVA_NS=wva-${NAMESPACE} WVA_WATCH_NS=${NAMESPACE}
```
<!-- guide:deploy.out_of_reach end -->

with `WVA_NS` yours and `WVA_WATCH_NS` theirs. This is the recommended
multi-tenant shape.

Pass `WVA_WATCH_NS` to **both** commands. The controller needs permissions in the
namespace it manages, and writing RBAC into someone else's namespace is an
admin's act by definition — so `setup-prereqs` creates that Role and RoleBinding,
and the uninstall removes them. Without it the controller starts, cannot read the
namespace it was pointed at, and crash-loops on its first ConfigMap read.

## Verification

<!-- guide:verify.objects start -->
```bash
kubectl get servicemonitor -n ${NAMESPACE}
kubectl get clusterrole,clusterrolebinding | grep wva-
```
<!-- guide:verify.objects end -->

The cluster-scoped names carry a hash of the namespace, so what you see here is
this namespace's set and no other's. Hand over once they exist — the namespace's
owner can then run `make deploy-wva` without you.

## Cleanup

<!-- guide:cleanup.undo start -->
```bash
# Undeploy targets ONE install, and this guide may have created two. Name the
# namespace each controller runs in — a bare `make undeploy-wva` defaults to
# workload-variant-autoscaler-system, which is neither of them, and leaves
# the RBAC it granted in ${NAMESPACE} behind.
make undeploy-wva WVA_NS=wva-${NAMESPACE}
make undeploy-wva WVA_NS=${NAMESPACE}
```
<!-- guide:cleanup.undo end -->

The namespace, Prometheus, KEDA and EPP stay.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | the namespace running llm-d, discovered | `team-a` |
| `WVA_WATCH_NS` | the namespace it runs in | `team-a` |

## Next

- [Bounding GPU usage](../admin-gpu-bounding/)
- [Install WVA in a namespace](../install-in-namespace/) — what you are
  enabling someone else to do
