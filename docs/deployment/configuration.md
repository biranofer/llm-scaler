# Deployment configuration reference

Every option `deploy/install.sh` reads. Verified against the script: each entry here is read by something, and every `VAR=${VAR:-default}` in `install.sh` and `deploy/lib/*.sh` appears below.

> Part of the [WVA deployment guide](../../deploy/README.md).

## Required

| Variable | Description | Required For |
|----------|-------------|--------------|
| `HF_TOKEN` | HuggingFace token | llm-d deployment |

## Core configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENVIRONMENT` | Deployment environment (`kubernetes` or `openshift`) | `kubernetes` |
| `WVA_SCOPE` | `cluster` or `namespace` — see [Scope](new-cluster.md#scope-what-the-controller-may-manage) | `namespace` on OpenShift, `cluster` elsewhere |
| `WVA_LIMITER` | `none`, `gpu-inventory` or `quota` — declares the limiter in the scaling-policy ConfigMap | `none` |
| `WVA_PROJECT` | Repository root the script installs from | `$PWD` |

## Image

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_IMAGE_REPO` | WVA image repository | `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` |
| `WVA_IMAGE_TAG` | WVA image tag | `latest` |
| `WVA_IMAGE_PULL_POLICY` | Image pull policy | `Always` |

## Namespaces

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_NS` | WVA controller namespace | `workload-variant-autoscaler-system` |
| `MONITORING_NAMESPACE` | Prometheus namespace | `workload-variant-autoscaler-monitoring` |
| `LLMD_NS` | Where your model servers run. **Used by the installer only** — it is not passed to the controller. It decides which namespace `scaledobjects-*` scans, and (with `DEPLOY_LLMD_NS=true`) which namespace is created. See [Which namespace is which](#which-namespace-is-which) | `llm-d-optimized-baseline` |

## Deployment flags

| Variable | Description | Default |
|----------|-------------|---------|
| `DEPLOY_PROMETHEUS` | Deploy Prometheus stack | `true` |
| `DEPLOY_OPERATIONAL_DASHBOARD` | Deploy Grafana and operational dashboard | `true` |
| `DEPLOY_WVA` | Deploy WVA controller | `true` |
| `DEPLOY_LWS` | Deploy LeaderWorkerSet (needed only for full e2e suite; skip for smoke, benchmarks, or pre-installed clusters) | `false` |
| `DEPLOY_ALERTING_RULES` | Install the PrometheusRule alerts | `false` |
| `ENABLE_SCALE_TO_ZERO` | Allow a model to be parked at zero replicas, and enable the EPP `flowControl` gate that makes waking it possible | `true` |
| `SKIP_CHECKS` | Skip prerequisite checks | `false` |
| `WVA_ALLOW_COEXIST` | Install alongside an existing WVA. Refused by default: both controllers would allocate from the same pool of free GPUs without seeing each other's claims, oversubscribing the cluster | `false` |
| `SCALER_BACKEND` | `keda` or `none` (use a pre-installed backend) | `keda` |
| `KEDA_NAMESPACE` | Namespace KEDA is installed in | `keda-system` |
| `KEDA_HELM_INSTALL` | Install KEDA with Helm rather than assuming it is present | `false` |
| `KEDA_CHART_VERSION` | KEDA Helm chart version | `2.19.0` |
| `UNDEPLOY` | Remove instead of install (`install.sh` doubles as the uninstaller) | `false` |
| `DELETE_NAMESPACES` | With `UNDEPLOY=true`, also delete the namespaces | `false` |
| `WVA_REPLICAS` | Controller replicas. The manifest already elects a leader, so extra replicas are **warm standbys, not extra throughput** — only the leader runs the optimization loops. Two turns a node drain from "no decisions until rescheduled" into a lease timeout | `1` |
| `UNDEPLOY_SHARED` | With `UNDEPLOY=true`, also remove Prometheus, the scaler backend and EPP. **Off by default**: they are shared, this install may not have created them, and removing them takes out everything else on the cluster that uses them | `false` |

> `make deploy-e2e-infra` passes `ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED)`,
> whose Makefile default is **`false`** — the opposite of `install.sh`'s. So an
> e2e deploy has scale-to-zero OFF unless you pass
> `SCALE_TO_ZERO_ENABLED=true`, while a plain `make deploy-wva-on-k8s` has it ON.
> Three e2e suites skip silently when it is off.

ScaledObjects, HPA stabilization (`spec.advanced.horizontalPodAutoscalerConfig.behavior`) and vLLM ModelService tuning are not controlled by `install.sh`; manage them via `kubectl apply` directly (see the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) for reference manifests).

## Default ScaledObjects

**Read this if WVA is installed and nothing is scaling.** A ScaledObject is how a
workload *registers* with WVA: the controller has no watch and no listing, so it
only ever learns about workloads KEDA calls it about. An install with no
ScaledObject anywhere is a controller that is never asked anything — idle, and
reporting itself healthy.

These options create one per llm-d model server, so you do not have to hand-write
them. A model server is any Deployment or LeaderWorkerSet labelled
`llm-d.ai/inferenceServing=true`; its model is read from `--served-model-name`, or
`--model` where it sets no other.

It works as **plan then apply**, because creating autoscaling objects across a
cluster is not something to discover the shape of afterwards.

```bash
# 1. See what would be created. Nothing is applied.
make scaledobjects-plan

# 2. Edit the plan it wrote: set the first column to yes or no, correct a modelID,
#    change min/max, delete rows you do not want.
$EDITOR /tmp/wva-scaledobject-plan.XXXX

# 3. Apply exactly that file.
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=/tmp/wva-scaledobject-plan.XXXX
```

The plan is a tab-separated table you can read and edit in place:

```
#apply  namespace  kind        name      modelID             inferencePool       min  max
yes     llm-d-sim  Deployment  sotest-a  e2ewva/dummy-model  optimized-baseline  1    10
no      llm-d-sim  Deployment  sotest-b  UNKNOWN             -                   1    10
# ^ llm-d-sim/sotest-b: no --served-model-name or --model; set modelID by hand to include it
```

`inferencePool` is shown for orientation: it is the EPP queue that workload sits
behind, resolved by matching pod labels against each pool's selector — the same way
WVA resolves it. A `-` means no pool has adopted the workload.

Rows that cannot be applied are marked `no` **and kept**, with the reason, rather
than dropped: the list you are shown is then the whole truth about what was found,
and turning a `no` into a `yes` is a deliberate act.

If you would rather review interactively, `make scaledobjects-edit` opens the same
plan in `$EDITOR` and asks before applying. It needs a terminal; everything it can
do is also reachable through plan-then-apply, which does not.

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_DEFAULT_SO` | `false` (do nothing), `plan` (list and stop), `edit` (list, `$EDITOR`, confirm), `true` (apply everything found) | `false` |
| `WVA_DEFAULT_SO_NS` | Namespace to scan. `wva` for WVA's own namespace, `all` for every namespace holding model servers — `all` needs a cluster-scoped install, and a namespace-scoped one warns and falls back | `$LLMD_NS` |
| `WVA_DEFAULT_SO_PLAN` | An existing file is applied as-is, edits included. Otherwise, where the generated plan is written | a temp file |
| `WVA_DEFAULT_SO_MIN` | `minReplicaCount` on generated objects. Not `0` even with scale-to-zero on: parking a model costs its next request a cold start, which is a decision about that workload's users | `1` |
| `WVA_DEFAULT_SO_MAX` | `maxReplicaCount` on generated objects | `10` |
| `WVA_DEFAULT_SO_ADOPT` | Repoint a workload's **existing** ScaledObject at WVA instead of leaving it alone. Patches only its `triggers`; envelope and behavior are untouched, and no second object is created | `false` |
| `WVA_DEFAULT_SO_TEMPLATE` | Your own ScaledObject template, substituted per workload. Placeholders: `{{NAMESPACE}}` `{{NAME}}` `{{KIND}}` `{{APIVERSION}}` `{{MODEL_ID}}` `{{SCALER_ADDRESS}}` `{{MIN}}` `{{MAX}}`. Start from `config/samples/keda/external-scaler/scaledobject-template.yaml` | the shipped shape |

Set them on a deploy to do this during install:

```bash
make deploy-wva-on-k8s WVA_DEFAULT_SO=plan   # install, then show what it would create
make deploy-wva-on-k8s WVA_DEFAULT_SO=true WVA_DEFAULT_SO_NS=all
```

Two things it will never do. It **does not touch a workload that already has a
ScaledObject** — that one may be hand-tuned or GitOps-managed, and two
ScaledObjects on one target is two HPAs fighting over a replica count. And it
**skips any workload whose model it cannot determine** rather than guessing,
because a wrong `modelID` groups a workload with a model it does not serve and
mis-scales both.

Generated objects use an `external-push` trigger, so KEDA holds a stream open and
WVA pushes activation the moment it decides — the difference between waking a
parked workload in about the detection interval and waiting out a poll.

## Pointing at an existing Prometheus

Required whenever this install did not deploy Prometheus itself. `PROMETHEUS_URL`
is written into the controller's config; without it the controller keeps the
shipped default and **exits at startup** if nothing answers there
(`CRITICAL: Failed to connect to Prometheus`).

| Variable | Description | Default |
|----------|-------------|---------|
| `PROMETHEUS_URL` | Full base URL, e.g. `https://prom.monitoring.svc.cluster.local:9090` | the kube-prometheus-stack this install deploys |
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | Connect without verifying the server certificate | `true` in the shipped config |
| `DEPLOY_PROMETHEUS` | Deploy a Prometheus stack. `false` to use yours | `true` |
| `DEPLOY_LLMD_NS` | Create the llm-d namespace. `false` when llm-d runs elsewhere — an empty one looks like the place to deploy models, and WVA would not be watching it | `true` |

## Which namespace is which

Three namespaces appear in these options and they do different jobs:

| variable | what lives there | who reads it |
| --- | --- | --- |
| `WVA_NS` | the controller, its ConfigMaps and its external-scaler Service | the installer, and the controller (as `POD_NAMESPACE`) |
| `LLMD_NS` | your model servers | **the installer only** — never passed to the controller |
| `MONITORING_NAMESPACE` | Prometheus and Grafana, if this install deploys them | the installer |

`LLMD_NS` not reaching the controller is not an oversight. WVA has no watch and no
listing: it learns about a workload when KEDA calls its external scaler about it,
from any namespace. It never goes looking in a namespace, so it has no use for the
name of one.

### What the scope actually changes

`WVA_SCOPE` controls the controller's **cache**, via `--watch-namespace`:

| scope | `--watch-namespace` | consequence |
| --- | --- | --- |
| `cluster` | unset | reads Deployments, pods, InferencePools and nodes in **any** namespace. `LLMD_NS` can be anywhere |
| `namespace` | its own (`POD_NAMESPACE`) | reads only **its own namespace** |

> **The constraint that follows, and it is easy to get wrong:** a namespace-scoped
> WVA can only manage model servers **in its own namespace**. Installing one into
> `wva-system` while your models run in `llm-d-prod` gives you a controller that
> KEDA will call and that cannot read the workload it is being asked about. For a
> namespace-scoped install, put the controller in the namespace with the models —
> `WVA_NS` and `LLMD_NS` are the same namespace.
>
> Cluster-scoped has no such constraint: models can be anywhere.

## Living beside what the cluster already has

**KEDA is never overwritten.** On `kubernetes` and `openshift` the install does not
Helm-install KEDA at all — it checks that `scaledobjects.keda.sh` exists and fails
with instructions if it does not. Your KEDA, its version and its configuration are
untouched. `KEDA_HELM_INSTALL=true` is the opt-in that would install one; even then
it skips when a working KEDA (CRD + running operator + metrics APIService) is
already there. `SCALER_BACKEND=none` skips the check entirely.

**Uninstalling WVA does not uninstall KEDA, Prometheus or EPP.** Removing WVA is the
job; removing what WVA was pointed at is a separate decision, and an explicit one —
`UNDEPLOY_SHARED=true`.

**A second WVA is refused.** Their workloads would be separate — a workload
registers with the scaler address its trigger names — but their GPU budgets would
not. See
[One WVA per cluster](existing-cluster.md#one-wva-per-cluster-or-one-per-namespace--never-two-managing-the-same-workloads).

## Adding a model later

Deploy the model server, then re-run:

```bash
make scaledobjects-apply LLMD_NS=<your namespace>
```

It creates a ScaledObject for the **new** workload and leaves every existing one
alone — a workload that already has one is reported as such and skipped, so this is
safe to run as often as you like:

```
[SUCCESS]   llm-d-sim/model-new (Deployment) -> ScaledObject model-new-wva (modelID: org/new-model)
[SUCCESS] Default ScaledObjects: 1 created, 1 not applied
```

Use `make scaledobjects-plan` first if you want to see the list before anything is
created. Nothing about the controller needs restarting or reconfiguring: it learns
about the workload from the first KEDA call.

## High availability

The controller elects a leader (`--leader-elect=true`, with tunable lease, renew and
retry durations), so more than one replica is safe — but the extra replicas are
**standbys**. Only the leader runs the collection and optimization loops; the others
wait on the lease.

```bash
make deploy-wva-on-k8s WVA_REPLICAS=2
```

What that buys is failover: a node drain or a crash costs you a lease timeout rather
than the time to reschedule a pod. What it does not buy is throughput — WVA's cycle
is one process reasoning about the whole fleet at once, deliberately, because a GPU
budget cannot be split across controllers that cannot see each other's decisions —
which is also why there is no supported way to run two.

## Advanced

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_TLS_VERIFY` | Skip Prometheus TLS verification | `false`, forced to `true` on OpenShift and for in-cluster self-signed Prometheus |
| `WVA_LOG_LEVEL` | WVA logging level | `info` |
| `PROMETHEUS_SECRET_NAME` | Secret holding the Prometheus serving cert | `prometheus-web-tls` |
| `PROMETHEUS_SECRET_NS` | Namespace of that secret | `$MONITORING_NAMESPACE` |
| `PROM_CA_CERT_PATH` | Where the extracted Prometheus CA is written | `/tmp/prometheus-ca.crt` |
| `GATEWAY_API_VERSION` | Gateway API version installed for llm-d | `v1.2.0` |
| `LWS_NAMESPACE` | Namespace for LeaderWorkerSet installation | `lws-system` |
| `LWS_CHART_VERSION` | LeaderWorkerSet Helm chart version | `0.8.0` |

## Optional: scaling band after `make deploy-e2e-infra`

If `SCALE_UP_THRESHOLD` and/or `SCALE_DOWN_BOUNDARY` are set in the environment, the Makefile patches the `wva-scaling-policy-config` ConfigMap after install. Note the patch replaces the whole `default` entry, so it writes `analyzerName: saturation` alongside the band.

