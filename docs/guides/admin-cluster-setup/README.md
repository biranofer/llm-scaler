# Cluster-admin setup: prerequisites for a namespace

**For a cluster admin.** Someone owns a namespace, runs model servers in it, and
wants WVA. This is the one command you run for them, and what it creates.

> Part of the [WVA deployment guide](../../deploy/README.md).
> Their side of it: [Install WVA in your namespace](install-in-namespace.md).

## The command

```bash
make setup-prereqs-namespace-on-k8s WVA_NS=team-a
# on OpenShift:
make setup-prereqs-namespace-on-openshift WVA_NS=team-a
```

Once per namespace. Not once per upgrade — after this, that namespace's owner
installs and upgrades their own controller with no cluster-scoped rights, and you
are not in the loop again unless a WVA release changes what it needs cluster-wide.

## What it creates, and why each piece needs you

| object | why a namespace admin cannot create it |
| --- | --- |
| the namespace | cluster-scoped to create |
| `wva-metrics-auth-role` + binding | the metrics filter issues **TokenReview** and **SubjectAccessReview** |
| `wva-metrics-reader` + binding | lets your Prometheus scrape the controller |
| `wva-epp-metrics-reader-role` + binding | needs `nonResourceURLs: /metrics`, which a Role cannot express |
| `wva-node-reader-role` + binding | resolving a variant's accelerator reads **Nodes** |
| the ServiceMonitor | the stock `admin` ClusterRole does not grant `monitoring.coreos.com` |
| Prometheus / KEDA | only if the cluster has none; an existing one is used as it is |

That last row is worth knowing about: the ServiceMonitor is namespaced, so it
*looks* like something a namespace admin could create. On a real cluster they
usually cannot — the built-in `admin` role covers core and apps resources, not
Prometheus Operator CRDs. It was the single denial standing between a namespace
admin and a working install.

**Every cluster-scoped object is named with a hash of the namespace** —
`wva-metrics-auth-role-1d8cfc15` and so on. Two installs on one cluster therefore
cannot take each other's, and an uninstall removes only its own. On a shared
cluster this matters: ten WVA installs sharing four fixed-name ClusterRoles means
whichever installs last silently rewrites everyone's permissions.

## Checking before you run it

```bash
make check-prereqs-namespace-on-k8s WVA_NS=team-a
```

Read-only. It renders the manifests this install would apply and asks the API
server whether the caller may create each kind, rather than inferring from the
scope name.

## Keeping the controller out of the tenant's reach

By default the controller runs *in* the namespace it manages, which means whoever
administers that namespace administers the controller — its Deployment, args, env
and image. **Nothing carried on the controller can bound the person who can edit
the controller.**

Where the bound must actually hold, run it in a namespace you own and point it at
theirs:

```bash
# WVA_NS is yours — where the controller runs.
# WVA_WATCH_NS is theirs — what it manages.
make setup-prereqs-namespace-on-k8s  WVA_NS=wva-team-a
make deploy-wva-namespace-on-k8s     WVA_NS=wva-team-a WVA_WATCH_NS=team-a
```

This is the recommended multi-tenant shape. The tenant keeps their workloads and
their ScaledObjects; they do not get to edit what bounds them.

## Undoing it

```bash
make undeploy-wva-on-k8s WVA_NS=team-a WVA_SCOPE=namespace
```

Removes the controller and this namespace's own cluster-scoped objects. The
namespace, Prometheus, KEDA and EPP stay — they are shared, and this install may
not have created them.

## Next

- [Bounding GPU usage](admin-gpu-bounding.md) — one command to make every WVA on
  the cluster respect a real GPU budget
- [Configuration reference](configuration.md)
