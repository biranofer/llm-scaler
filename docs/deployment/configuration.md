# Deployment configuration reference

Every option `deploy/install.sh` reads. Verified against the script: each entry here is read by something, and every `VAR=${VAR:-default}` in `install.sh` and `deploy/lib/*.sh` appears below.

> Part of the [WVA deployment guide](../../deploy/README.md).

### Environment Variables (Script)

#### Required

| Variable | Description | Required For |
|----------|-------------|--------------|
| `HF_TOKEN` | HuggingFace token | llm-d deployment |

#### Core Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENVIRONMENT` | Deployment environment (`kubernetes` or `openshift`) | `kubernetes` |
| `WVA_SCOPE` | `cluster` or `namespace` — see [Scope](installation.md#scope-what-the-controller-may-manage) | `namespace` on OpenShift, `cluster` elsewhere |
| `WVA_LIMITER` | `none`, `gpu-inventory` or `quota` — declares the limiter in the scaling-policy ConfigMap | `none` |
| `WVA_PROJECT` | Repository root the script installs from | `$PWD` |
| `CONTROLLER_INSTANCE` | Instance name for running several WVAs on one cluster | `""` (single instance) |

#### Image Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_IMAGE_REPO` | WVA image repository | `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` |
| `WVA_IMAGE_TAG` | WVA image tag | `latest` |
| `WVA_IMAGE_PULL_POLICY` | Image pull policy | `Always` |

#### Namespace Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_NS` | WVA controller namespace | `workload-variant-autoscaler-system` |
| `MONITORING_NAMESPACE` | Prometheus namespace | `workload-variant-autoscaler-monitoring` |
| `LLMD_NS` | llm-d namespace | `llm-d-optimized-baseline` |

#### Deployment Flags (`install.sh`)

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

#### Default ScaledObjects

A ScaledObject is how a workload REGISTERS with WVA — the controller has no watch
and no listing, so an install with none anywhere never scales anything and looks
healthy doing it. These create one per llm-d model server (Deployments labelled
`llm-d.ai/inferenceServing=true`), reading each one's model from its
`--served-model-name`, or `--model` where it sets no other.

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_DEFAULT_SO` | Create a default ScaledObject per llm-d model server | `false` |
| `WVA_DEFAULT_SO_NS` | Namespace to do it in, or `all` for every namespace holding model servers. `all` requires a cluster-scoped install; a namespace-scoped one warns and falls back to `LLMD_NS` | `$LLMD_NS` |
| `WVA_DEFAULT_SO_MIN` | `minReplicaCount` on the generated objects. Not 0 even where scale-to-zero is on: parking a model costs its next request a cold start, which is a decision about that workload's users | `1` |
| `WVA_DEFAULT_SO_MAX` | `maxReplicaCount` on the generated objects | `10` |

Two things it will not do. It **never touches a Deployment that already has a
ScaledObject** — that one may be hand-tuned or GitOps-managed, and two
ScaledObjects on one target is two HPAs fighting. And it **skips any workload whose
model it cannot determine** rather than guessing, because a wrong `modelID` groups
a workload with a model it does not serve and mis-scales both. Both outcomes are
reported per workload, with a count at the end.

The generated objects use an `external-push` trigger, so KEDA holds a stream open
and WVA pushes activation the moment it decides — which is what lets a workload
parked at zero wake in about the detection interval rather than a poll period.

#### Advanced (`install.sh`)

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

#### Optional: scaling band after `make deploy-e2e-infra`

If `SCALE_UP_THRESHOLD` and/or `SCALE_DOWN_BOUNDARY` are set in the environment, the Makefile patches the `wva-scaling-policy-config` ConfigMap after install. Note the patch replaces the whole `default` entry, so it writes `analyzerName: saturation` alongside the band.

