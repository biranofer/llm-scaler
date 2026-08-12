# Install WVA in your namespace

**The common path.** You own a namespace, your model servers run in it, and you
want WVA to size them. You do not need to be a cluster admin — you need one
thing from one, once.

> Part of the [WVA deployment guide](../../deploy/README.md).

## What you get

WVA watches how saturated your model servers are and tells KEDA how many replicas
each variant needs. KEDA owns the HPA and does the scaling. WVA never writes to
the cluster itself; its only write is Events.

It manages **only your namespace**. Another team's WVA cannot see your workloads,
and yours cannot see theirs.

> **Your models are in this namespace.** That is the shape this path assumes:
> `WVA_NS` is where the controller runs *and* what it manages, so set it to your
> llm-d namespace. To run the controller somewhere else and point it here, see
> [Cluster-admin setup](admin-cluster-setup.md#keeping-the-controller-out-of-the-tenants-reach).

## Before you start

| you need | how to check |
| --- | --- |
| your namespace | `kubectl get ns <your-namespace>` |
| KEDA on the cluster | `kubectl get crd scaledobjects.keda.sh` |
| a Prometheus that scrapes your model servers | the check below finds it and prints it — you do not pass a URL |
| model servers labelled `llm-d.ai/inferenceServing=true` | `kubectl get deploy -n <ns> -o yaml \| grep inferenceServing` |

Then run the read-only check. It renders the exact manifests this install would
apply and asks the API server whether you may create each kind — so it answers
for *your* rights on *this* cluster, rather than assuming:

```bash
make check-prereqs-namespace-on-k8s WVA_NS=<your-namespace>
# on OpenShift:
make check-prereqs-namespace-on-openshift WVA_NS=<your-namespace>
```

## Step 1 — ask an admin to run one command

WVA needs a few cluster-scoped objects: the metrics authn filter issues
TokenReviews, EPP metrics need `nonResourceURLs`, and resolving which GPU a
variant runs on reads Nodes. None of that can be expressed in a Role, so a
namespace admin cannot create it — and it is not something you should be granted
just to install an autoscaler.

Send your admin this:

```bash
make setup-prereqs-namespace-on-k8s WVA_NS=<your-namespace>
```

It is **once per namespace**, not once per upgrade. They can read exactly what it
creates in [Cluster-admin setup](admin-cluster-setup.md).

## Step 2 — install the controller

Yours to run, now and for every future upgrade, with no cluster-scoped rights:

```bash
make deploy-wva-namespace-on-k8s WVA_NS=<your-namespace>
# or, if you already name your llm-d namespace that way:
make deploy-wva-namespace-on-k8s LLMD_NS=<your-namespace>
```

That is the whole command. The Prometheus is detected — on OpenShift it is the
platform's Thanos Querier, at a fixed address that needs no permission to know.
Pass `PROMETHEUS_URL=<url>` only to override what the check reported, or to point
at a Prometheus outside the cluster.

If Step 1 has not happened, this stops and names every object that is missing, so
you have a precise list to hand back rather than a permissions error.

## Step 3 — register your workloads

**Nothing scales until you do this.** WVA has no watch and no listing: it learns
a workload exists only when KEDA calls it about one. A ScaledObject is that
registration. Until one exists, the controller runs, reports healthy, and scales
nothing.

```bash
make scaledobjects-plan WVA_NS=<your-namespace> WVA_DEFAULT_SO_NS=<your-namespace>
```

That writes an editable table — one row per model server it found — and applies
nothing:

```
#apply  namespace  kind        name              modelID     inferencePool       min  max
yes     team-a     Deployment  llama-decode      meta/llama  optimized-baseline  1    10
```

Edit it (flip `yes`/`no`, fix a `modelID`, change the bounds), then:

```bash
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=/tmp/wva-scaledobject-plan.XXXX
```

## Step 4 — check it works

```bash
kubectl get scaledobject,hpa -n <your-namespace>
```

The HPA is KEDA's — it creates one per ScaledObject. **An HPA whose
`CurrentMetrics` is populated means the whole chain works**: KEDA called WVA, WVA
decided, KEDA got the answer.

To see the decisions themselves:

```bash
kubectl logs -n <your-namespace> deploy/wva-controller-manager | grep scaling-decision
```

```
scaling-decision {"modelID":"meta/llama","decisions":[{"name":"llama-decode-wva","curr":1,"tgt":3,"action":"scale-up"}]}
```

## Upgrading

Step 2 again, with a newer image. No admin needed — the prerequisites stand until
a WVA version changes what it needs cluster-wide, which is rare and called out in
release notes.

## Uninstalling

```bash
make undeploy-wva-on-k8s WVA_NS=<your-namespace> WVA_SCOPE=namespace
```

Your namespace and your workloads stay. **Your ScaledObjects also stay, and that
is a trap worth knowing**: their trigger points at a scaler that no longer exists,
so KEDA keeps the HPA and keeps calling nothing. A workload parked at zero cannot
then be woken. Delete them along with the controller unless you are reinstalling.

## What this path does not give you

| not included | why, and what to do |
| --- | --- |
| a GPU bound | your controller scales to each workload's `maxReplicaCount` unless a cluster policy bounds it. That policy is an admin's to publish — see [Bounding GPU usage](admin-gpu-bounding.md) |
| cross-namespace scaling | by construction: this controller reads one namespace |
| control over your own limits | deliberate. WVA reads limiters from a namespace you cannot edit, so a bound placed on you holds |

## Next

- [After the install](operations.md) — what to watch, and the metrics that answer
  specific questions
- [Configuration reference](configuration.md) — every variable the installer reads
- [Tuning how it scales](../developer-guide/scaling-policy-config.md) — thresholds
  and policy tiers, which are yours to set
