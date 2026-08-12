# Cluster-admin setup for a namespace

## Overview

Creates, for one namespace, the things a namespace admin cannot: the namespace
itself, the cluster-scoped RBAC, and the ServiceMonitor. After this, that
namespace's owner installs and upgrades the controller with no cluster-scoped
rights, and you are not in the loop again unless a WVA release changes what it
needs cluster-wide.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | the namespace running llm-d, discovered | `team-a` |
| `WVA_WATCH_NS` | the namespace it runs in | `team-a` |

## Prerequisites

Cluster-admin rights.

<!-- guide:prerequisites.check start -->
```bash
make check-prereqs
```
<!-- guide:prerequisites.check end -->

## Installation

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
make deploy-wva      WVA_NS=wva-${NAMESPACE} WVA_WATCH_NS=${NAMESPACE} INSTALL_PHASE=wva
```
<!-- guide:deploy.out_of_reach end -->

with `WVA_NS` yours and `WVA_WATCH_NS` theirs. This is the recommended
multi-tenant shape.

Pass `WVA_WATCH_NS` to **both** commands. The controller needs permissions in the
namespace it manages, and writing RBAC into someone else's namespace is an
admin's act by definition — so `setup-prereqs` creates that Role and RoleBinding,
and the uninstall removes them. Without it the controller starts, cannot read the
namespace it was pointed at, and crash-loops on its first ConfigMap read.

## Cleanup

<!-- guide:cleanup.undo start -->
```bash
make undeploy-wva
```
<!-- guide:cleanup.undo end -->

The namespace, Prometheus, KEDA and EPP stay.

## Next

- [Bounding GPU usage](../admin-gpu-bounding/README.md)
- [Install WVA in a namespace](../install-in-namespace/README.md) — what you are
  enabling someone else to do
