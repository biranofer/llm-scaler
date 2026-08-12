# Adding WVA to a cluster that already runs llm-d

Use this when llm-d is serving already and you want WVA to start scaling it. It
installs the controller and nothing else — no Prometheus, no llm-d namespace, no
model servers.

> Part of the [WVA deployment guide](../../deploy/README.md). Building a cluster
> from nothing instead? See [Installing on a new cluster](install-cluster-wide.md).

## What WVA needs from the cluster you have

Three things, and the installer cannot guess any of them:

| it needs | because | how to say so |
| --- | --- | --- |
| **a Prometheus URL** | WVA reads vLLM/SGLang metrics from Prometheus, and **exits at startup if it cannot reach one** | `PROMETHEUS_URL=` |
| **the right scope** | cluster-scoped manages models in any namespace; namespace-scoped manages only its own, so it must be installed *in* the namespace with the models | `WVA_SCOPE=` |
| **a ScaledObject per workload** | a ScaledObject IS the registration — WVA never sees a workload it is not called about | `make scaledobjects-plan` |

## How many WVAs a cluster has

One cluster-scoped controller, or one per namespace — never both, and the install
refuses a combination that would leave two controllers deciding for the same
workloads. See
[How many WVAs a cluster has](install-cluster-wide.md#how-many-wvas-a-cluster-has).

## Install

```bash
make deploy-wva-on-k8s \
  PROMETHEUS_URL=https://<your-prometheus>.<ns>.svc.cluster.local:9090
```

That is the whole command. `PROMETHEUS_URL` is the one thing the installer cannot
work out for itself: it is written into the controller's config, and a wrong value
shows up as a pod in CrashLoopBackOff with `CRITICAL: Failed to connect to
Prometheus`.

Everything else is decided by looking at the cluster:

| the install finds | it does |
| --- | --- |
| a Prometheus outside its own monitoring namespace | uses it — no second stack, no monitoring namespace |
| Gateway API / GAIE CRDs already present | keeps their versions; only missing CRDs are installed |
| KEDA already installed | leaves it alone |

It creates exactly one namespace: the controller's own.

**There is no setting for "the namespace my models run in"**, and that is not an
omission. WVA has no watch and no listing — it learns of a workload only when KEDA
calls it about one, so a namespace name would tell it nothing. The ScaledObject is
what puts a workload in scope.

If your Prometheus serves TLS with a certificate WVA cannot verify, either point at
the secret holding its CA:

```bash
  PROMETHEUS_SECRET_NAME=<secret> PROMETHEUS_SECRET_NS=<namespace>
```

or accept it unverified:

```bash
  PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true
```

Check before you commit to it — this is read-only:

```bash
make check-prereqs
```

## Then: connect your workloads

Nothing scales until a ScaledObject points at WVA. Look before you leap:

```bash
make scaledobjects-plan
```

That lists every Deployment and LeaderWorkerSet labelled
`llm-d.ai/inferenceServing=true`, the model each one serves, and the InferencePool
it sits behind, and writes an editable table. Nothing is applied.

| you want | do this |
| --- | --- |
| everything this install can manage | `make scaledobjects-apply` — cluster-scoped scans every namespace, namespace-scoped scans its own |
| one namespace only | `make scaledobjects-apply WVA_DEFAULT_SO_NS=<ns>` |
| only some | edit the plan, then `make scaledobjects-apply WVA_DEFAULT_SO_PLAN=<file>` |
| review each one interactively | `make scaledobjects-edit` |
| **take over ScaledObjects that already exist** | add `WVA_DEFAULT_SO_ADOPT=true` |
| **your own ScaledObject shape** | add `WVA_DEFAULT_SO_TEMPLATE=<file>` |

### Taking over existing ScaledObjects

A cluster running llm-d often already scales those workloads on CPU, a Prometheus
query, or a cron schedule. By default the plan marks those `no` and leaves them
alone — they may be hand-tuned or GitOps-managed.

`WVA_DEFAULT_SO_ADOPT=true` marks them for update instead. Adoption **patches the
object that is already there**, replacing only its `triggers`:

- its `minReplicaCount`, `maxReplicaCount`, `behavior` and everything else are
  left exactly as whoever tuned them left them — the only thing changing is who
  decides the count;
- no second ScaledObject is created, because two on one target is two HPAs writing
  the same replica count;
- the old trigger is **replaced, not added to**. That is deliberate: two scalers
  feeding one HPA means the larger answer silently wins, and you would not be able
  to tell which one produced any given decision.

The plan shows exactly which objects this will touch before it touches them.

### Using your own ScaledObject shape

If your fleet has conventions — a fallback policy, particular stabilization
windows, labels your tooling expects — supply a template instead of editing the
shipped shape back afterwards:

```bash
make scaledobjects-apply WVA_DEFAULT_SO_TEMPLATE=$PWD/my-scaledobject.yaml
```

Start from
[`config/samples/keda/external-scaler/scaledobject-template.yaml`](../../config/samples/keda/external-scaler/scaledobject-template.yaml).
Placeholders are substituted per workload: `{{NAMESPACE}}`, `{{NAME}}`,
`{{KIND}}`, `{{APIVERSION}}`, `{{MODEL_ID}}`, `{{SCALER_ADDRESS}}`, `{{MIN}}`,
`{{MAX}}`. Substitution is literal, so the template is also a valid manifest as
written — apply one by hand first to check the shape.

## Verify

```bash
kubectl get scaledobject -n <your namespace>
kubectl get hpa -n <your namespace>          # KEDA creates one per ScaledObject
```

A KEDA HPA with `CurrentMetrics` populated means the whole chain works: WVA was
called, decided, and KEDA received the answer. Empty means KEDA never got one —
check the trigger's `scalerAddress` and that `modelID` is set.

More in [After the install](operations.md).
