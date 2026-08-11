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
| `WVA_SCOPE` | `cluster` or `namespace` — see [Scope](installation.md#scope-what-the-controller-may-manage) | `namespace` on OpenShift, `cluster` elsewhere |
| `WVA_LIMITER` | `none`, `gpu-inventory` or `quota` — declares the limiter in the scaling-policy ConfigMap | `none` |
| `WVA_PROJECT` | Repository root the script installs from | `$PWD` |
| `CONTROLLER_INSTANCE` | Instance name for running several WVAs on one cluster | `""` (single instance) |

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
| `LLMD_NS` | llm-d namespace | `llm-d-optimized-baseline` |

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
| `SCALER_BACKEND` | `keda` or `none` (use a pre-installed backend) | `keda` |
| `KEDA_NAMESPACE` | Namespace KEDA is installed in | `keda-system` |
| `KEDA_HELM_INSTALL` | Install KEDA with Helm rather than assuming it is present | `false` |
| `KEDA_CHART_VERSION` | KEDA Helm chart version | `2.19.0` |
| `UNDEPLOY` | Remove instead of install (`install.sh` doubles as the uninstaller) | `false` |
| `DELETE_NAMESPACES` | With `UNDEPLOY=true`, also delete the namespaces | `false` |

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

## Advanced

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_TLS_VERIFY` | Skip Prometheus TLS verification | `false`, forced to `true` on OpenShift and for in-cluster self-signed Prometheus |
| `WVA_LOG_LEVEL` | WVA logging level | `info` |
| `WVA_METRICS_SECURE` | Serve the WVA metrics endpoint over HTTPS with authn/authz | `true` |
| `PROMETHEUS_SECRET_NAME` | Secret holding the Prometheus serving cert | `prometheus-web-tls` |
| `PROMETHEUS_SECRET_NS` | Namespace of that secret | `$MONITORING_NAMESPACE` |
| `PROM_CA_CERT_PATH` | Where the extracted Prometheus CA is written | `/tmp/prometheus-ca.crt` |
| `GATEWAY_API_VERSION` | Gateway API version installed for llm-d | `v1.2.0` |
| `LWS_NAMESPACE` | Namespace for LeaderWorkerSet installation | `lws-system` |
| `LWS_CHART_VERSION` | LeaderWorkerSet Helm chart version | `0.8.0` |

## Optional: scaling band after `make deploy-e2e-infra`

If `SCALE_UP_THRESHOLD` and/or `SCALE_DOWN_BOUNDARY` are set in the environment, the Makefile patches the `wva-scaling-policy-config` ConfigMap after install. Note the patch replaces the whole `default` entry, so it writes `analyzerName: saturation` alongside the band.

