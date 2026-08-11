# Adding WVA to a cluster that already runs llm-d

Use this when llm-d is serving already and you want WVA to start scaling it. It
installs the controller and nothing else — no Prometheus, no llm-d namespace, no
model servers.

> Part of the [WVA deployment guide](../../deploy/README.md). Building a cluster
> from nothing instead? See [Installing on a new cluster](new-cluster.md).

## What WVA needs from the cluster you have

Three things, and the installer cannot guess any of them:

| it needs | because | how to say so |
| --- | --- | --- |
| **a Prometheus URL** | WVA reads vLLM/SGLang metrics from Prometheus, and **exits at startup if it cannot reach one** | `PROMETHEUS_URL=` |
| **your model servers' namespace** | that is where it looks for workloads and where you will create ScaledObjects | `LLMD_NS=` |
| **a ScaledObject per workload** | a ScaledObject IS the registration — WVA never sees a workload it is not called about | `make scaledobjects-plan` |

## Install

```bash
make deploy-wva-on-k8s \
  DEPLOY_PROMETHEUS=false \
  DEPLOY_LLMD_NS=false \
  LLMD_NS=<where your model servers run> \
  PROMETHEUS_URL=https://<your-prometheus>.<ns>.svc.cluster.local:9090
```

- `DEPLOY_PROMETHEUS=false` — keep the Prometheus you have.
- `DEPLOY_LLMD_NS=false` — do not create an llm-d namespace. Without this you get
  an empty one, which is worse than none: it looks like the place to deploy models,
  and WVA is not watching it.
- `PROMETHEUS_URL` — **this one is not optional.** It is written into the
  controller's config. Get it wrong and the pod CrashLoopBackOffs with
  `CRITICAL: Failed to connect to Prometheus`.

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
make scaledobjects-plan LLMD_NS=<your namespace>
```

That lists every Deployment and LeaderWorkerSet labelled
`llm-d.ai/inferenceServing=true`, the model each one serves, and the InferencePool
it sits behind, and writes an editable table. Nothing is applied.

| you want | do this |
| --- | --- |
| all of them, in one namespace | `make scaledobjects-apply LLMD_NS=<ns>` |
| all of them, cluster-wide | `make scaledobjects-apply WVA_DEFAULT_SO_NS=all` |
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
