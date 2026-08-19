# After the install

Verifying WVA works, watching what it decides, and the first things to check when it does not.

> Part of the [WVA deployment guide](../../deploy/).

## Verifying the install

Every command on this page uses `$NS` for the namespace the **controller** runs
in. That is whatever `WVA_NS` was at install time, not a fixed name — the default
is `workload-variant-autoscaler-system`, but a per-team install is somewhere else.
Find it rather than assume it:

```bash
# the namespace WVA is actually installed in
NS=$(kubectl get deploy -A -l app.kubernetes.io/name=workload-variant-autoscaler \
       -o jsonpath='{.items[0].metadata.namespace}')
echo "$NS"
```

```bash
kubectl get pods -n "$NS"                     # the controller is Running
kubectl get scaledobject -A                   # your managed workloads
kubectl get hpa -A                            # KEDA created one per ScaledObject
```

If that returns more than one namespace, this cluster is running one WVA per
namespace; pick the one managing the workload you are looking at.

A ScaledObject with a KEDA HPA whose `CurrentMetrics` is populated means the whole
chain works: WVA was called, decided, and KEDA received the answer. An empty
`CurrentMetrics` means KEDA never got one — check the trigger's `scalerAddress`
and that `modelID` is set.

If metrics are the problem, follow them forward:

```bash
# 1. the model server exposes them
kubectl port-forward -n <llm-namespace> <vllm-pod> 8000:8000
curl -s http://localhost:8000/metrics | grep vllm:

# 2. Prometheus scrapes them  (query vllm:num_requests_running)
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-prometheus 9090:9090

# 3. WVA reads them and decides
kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler   | grep -E "Collected replica metrics|scaling-decision"
```

## Watching what WVA decides

WVA writes no custom resource. Its decisions are visible in three places, and you
want them in this order: the **dashboard** for whether things are healthy, the
**metrics** for a specific question, the **logs** only for why a single decision
came out the way it did.

### The operational dashboard

The install publishes a Grafana dashboard, *WVA Operational Dashboard*, covering
the whole pipeline: GPU discovery, metric collection health and freshness,
saturation, capacity, scaling decisions and limiter impact.

```bash
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-grafana 3000:80
# then http://localhost:3000  (default admin password: prom-operator)
```

It ships as a labelled ConfigMap, so **an existing Grafana picks it up too** — you
do not need to have let this install deploy Prometheus:

```bash
# publish into whichever namespace your Grafana's sidecar watches
DASHBOARD_NS=my-monitoring ./deploy/install.sh   # DEPLOY_PROMETHEUS=false is fine
```

Skip it entirely with `DEPLOY_OPERATIONAL_DASHBOARD=false`.

Read the panels top-down. The upper row answers "is WVA seeing the cluster at all";
until those are healthy, the scaling panels below them are meaningless.

#### Who owns the dashboard

The dashboard is published as a ConfigMap into a **monitoring** namespace, which
is normally not the namespace WVA runs in — so publishing the shared one is a
**cluster-admin** action even though it happens during the tenant install step.

| | can do |
| --- | --- |
| Cluster admin | publish and update the shared dashboard in the monitoring namespace; decide the datasource, and therefore who sees what |
| Namespace admin | publish a private copy into their own namespace (`DASHBOARD_NS=<own>`); use the shared one through their `?var-namespace=` link |

A namespace admin running `make deploy-wva` without rights to the monitoring
namespace gets a message saying so — the install continues, and only the
dashboard step is skipped. Nothing about WVA's scaling depends on it.

#### One dashboard, many installs

The ConfigMap has a **fixed name in whatever namespace you publish it to**, so
every install on a cluster writes the same object. That is deliberate — the
dashboard is generic and driven by variables, and one copy per tenant would fill
the picker with identical dashboards — but it has consequences worth knowing.

**The first row is the admin's view.** *Installs present* counts WVA Deployments
from kube-state-metrics; *Controllers reporting metrics* counts the ones whose
metrics actually arrive. A gap between them is an install nobody is scraping,
and it is the difference that matters: a controller that is not scraped looks
exactly like a controller that is not scaling. *Scrape targets DOWN* names how
many, and the **Controller** variable says which.

**Your namespace's view is a link, not a default.** A Grafana variable's default
lives inside the dashboard JSON, and this object is shared by every install on
the cluster — so there is no per-tenant default to set: pinning it would mean
everyone sees whichever tenant installed last. Each namespace gets its own entry
point into the one dashboard instead, which the install prints:

```text
<your-grafana>/d/wva-operational/wva-operational-dashboard?var-namespace=<your-namespace>
```

Drop the query string for the cluster-wide view. If you would rather have your
own dashboard object — pinned, private, nobody else writing it — publish a copy
into your own namespace:

```bash
DASHBOARD_NS=<your-namespace> make deploy-wva   # pinned to that namespace
```

**Namespace label:** `wva_*` metrics carry both `namespace` and
`exported_namespace`. `exported_namespace` is the *workload's* namespace and
`namespace` is the *controller's* — the same for a namespace-scoped install,
different for a cluster-scoped one, where grouping by `namespace` would collapse
every workload onto the controller's namespace. The dashboard defaults to
`exported_namespace` for that reason; the benchmark dashboard defaults to
`namespace`, because vLLM metrics carry only that one.

**Versions.** The ConfigMap records the WVA version that published it, and an
older install will not overwrite a newer dashboard — it says so and leaves it.
Panels for metrics a given version does not emit stay empty for that install:
on a cluster running several versions, an empty panel may mean "older
controller", not "nothing happening". Force a republish with:

```bash
kubectl delete configmap wva-operation-dashboard -n <dashboard-namespace>
```

**Who can see what is the datasource's decision, not WVA's.** On OpenShift,
`thanos-querier:9091` is cluster-monitoring-view — anything querying through it
reads every namespace, whatever scope WVA was installed with. For a tenant
Grafana that must only see its own namespace, point the datasource at
`thanos-querier:9092`, which enforces per-namespace RBAC.

That is also what makes the shared dashboard safe rather than merely tidy: with
a per-namespace datasource the **Namespace** variable only lists namespaces the
viewer may read, so "All" already means "all of mine". Pinning a default never
provided isolation — it only hid names from the dropdown while every query still
ran with the datasource's own permissions.

### The metrics that answer specific questions

All are exposed by the controller and scraped by the ServiceMonitor the install
creates. Full list: [Prometheus metrics](../developer-guide/prometheus.md).

**Is WVA working at all?**

| metric | healthy | what it means when it is not |
| --- | --- | --- |
| `wva_models_processed` | > 0 | no workload has registered — no ScaledObject names this scaler |
| `wva_metrics_pods_discovered` | > 0 per model | WVA cannot find the pods behind a model |
| `wva_metrics_freshness_status{status="fresh"}` | equals the pod count | pods sitting in `status="stale"` or `"missing"` are being decided on with old data, or none at all |
| `wva_errors_total` | flat | rising means the optimization cycle is failing |

`wva_metrics_freshness_status` is a per-`(variant_name, status)` gauge holding *how
many pods* are in that state — not a 0/1 flag. Compare the series:

```promql
wva_metrics_freshness_status{status!="fresh"} > 0
```

**Why is nothing scaling up?** — the two silent-stall causes, both of which look
like "WVA is fine" everywhere else:

| metric | meaning |
| --- | --- |
| `wva_node_access_denied` | `1` = a GPU limiter is configured but nodes are unreadable. Every variant gets no budget and **will not scale up**. |
| `wva_decisions_limited_total` | rising = a limiter is capping decisions. Pair with `wva_available_gpus` and `wva_spare_capacity`. |
| `wva_unattributed_gpus` | GPUs in use that could not be charged to a pool — usually a workload whose accelerator did not resolve |

**Is the decision itself sane?** — `wva_desired_replicas` vs `wva_current_replicas`,
with `wva_saturation_utilization` and `wva_analyzer_demand` / `wva_analyzer_target`
showing what drove it. A desired that never becomes current is an actuation
problem (KEDA, the HPA, or the workload), not a decision problem.

```bash
# read them straight off the controller
kubectl port-forward -n $NS svc/wva-controller-manager-metrics-service 8443:8443
curl -sk https://localhost:8443/metrics | grep -E '^wva_'
```

### The logs

Useful when a metric tells you *which* model is wrong and you want to know *why*.

| grep for | tells you | level |
| --- | --- | --- |
| `scaling-decision` | what WVA decided for a model, and the replica counts | Info |
| `Effective scaling policy` | which policy tier a model resolved to | Info |
| `GPU limiter (re)built from config` | a `limiters:` edit took effect, live | Info |
| `Collected replica metrics` | metrics are arriving | **`-v=4`** |

The controller runs at `-v=2` by default, so `Collected replica metrics` prints
nothing and grepping for it proves nothing either way. Use
`wva_metrics_pods_discovered` and `wva_metrics_freshness_status` for that question
instead — they are always on. If you do want the line, raise verbosity on the
container and put it back afterwards:

```bash
kubectl patch deployment -n $NS wva-controller-manager --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--v=4"}]'
```

```bash
kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler -f
```

WVA writes no custom resource, so its decisions are visible only in these logs, in
the metrics it publishes ([Prometheus metrics](../developer-guide/prometheus.md)),
and in the HPA state KEDA derives from them.

## Testing autoscaling

Drive load at the model and watch the ScaledObject and its HPA react. Full
procedures, including the simulator and the e2e suites, are in
[Testing](../developer-guide/testing.md).

## First-line troubleshooting

| symptom | most likely cause | check |
| --- | --- | --- |
| WVA pod not `Running` | image pull, resources, or Prometheus unreachable | `kubectl describe pod -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler` |
| "Metrics unavailable" in the logs | the ServiceMonitor does not select your model pods, so the series never reach Prometheus | `kubectl get servicemonitor -A`, then Prometheus `/targets` |
| HPA exists but `CurrentMetrics` is empty | KEDA never got an answer — usually the trigger's `scalerAddress` or a missing `modelID` | `kubectl describe hpa -n <ns> keda-hpa-<so-name>` |
| nothing scales, no errors | a limiter is declared and the workload's accelerator does not resolve, so it gets no GPU budget | `kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler \| grep -i accelerator` |
| a model never wakes from zero | the EPP flow-control queue is not reaching WVA | see [Troubleshooting](../developer-guide/troubleshooting.md) |
| `READY False` on the ScaledObject, and the HPA's `TARGETS` reads `cpu: <unknown>/80%` | KEDA could not fetch the metric spec from WVA, so it fell back to a CPU metric. The trigger names a scaler it cannot reach — most often a `scalerAddress` naming a **different install's namespace** than the controller actually runs in. `make scaledobjects-repoint` fixes exactly this: it rewrites `scalerAddress` on objects that ask for WVA but name a namespace where no scaler runs, and leaves one pointing at a second live install alone | `kubectl get scaledobject -A -o custom-columns=NAME:.metadata.name,ADDR:.spec.triggers[0].metadata.scalerAddress` then `kubectl get svc -A \| grep external-scaler` |
| demand looks far too low for the load you are driving, and `has N ready pod(s) but none attributed` appears each cycle | FMA is in the namespace and nothing is scraping its launcher pods, so the traffic they serve is invisible | see [FMA launcher pods](#fma-launcher-pods) |

### `no children to pick from` after a reinstall

KEDA's gRPC client backs off on a name that did not resolve, and keeps backing
off for far longer than an uninstall/reinstall takes. So a ScaledObject that
outlived a WVA uninstall can stay `READY False` against a scaler that is now
running perfectly — the name was NXDOMAIN while WVA was gone, and KEDA has not
re-resolved it yet.

```bash
kubectl rollout restart deploy/keda-operator -n keda
```

Verified on kind: the ScaledObjects went `READY True` within a poll interval of
the restart, with nothing else changed. Deleting the ScaledObjects before
uninstalling avoids it — which `make undeploy-wva` now does for the ones it
created.

### FMA launcher pods

A namespace running Fast Model Actuation needs two things WVA does not do by
default, and both fail silently when missing:

- **the launcher pods must be scraped.** They declare no container ports, so a
  PodMonitor selecting by port name generates no target for them at all — not a
  failing target, no target. Fix with
  `kubectl apply -k config/fma-launcher-metrics -n <ns>`.
- **the plan must target the requester**, not a decode Deployment, when the
  requester is the only serving workload there.

Symptoms: demand far lower than the load you are driving, `has N ready pod(s)
but none attributed` once per cycle, or a variant flat at `minReplicaCount`
while the queue grows.

The whole story — how attribution works, how to size `maxReplicas` from the
launcher pool, why GPU accounting is a lower bound, and what to check — is in
[WVA with Fast Model Actuation](../guides/fma/).

First stop for any of these:

```bash
kubectl logs -n "$NS"   -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200
```

Deeper diagnosis — EPP metrics, scale-from-zero, slow scale-up — is in
[Troubleshooting](../developer-guide/troubleshooting.md).

## Command cheatsheet

```bash
# === WVA Controller ===
kubectl get pods -n "$NS"
kubectl logs -n "$NS" -l app.kubernetes.io/name=workload-variant-autoscaler -f
kubectl describe deployment wva-controller-manager -n "$NS"

# === Managed workloads (a ScaledObject IS the registration) ===
kubectl get scaledobject -A
kubectl describe scaledobject <name> -n <namespace>

# === Metrics and Monitoring ===
kubectl get servicemonitor -A
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1" | jq
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-prometheus 9090:9090

# === ScaledObjects / HPA ===
kubectl get scaledobject -A
kubectl describe scaledobject <name> -n <namespace>
kubectl get hpa -A
kubectl describe hpa <name> -n <namespace>

# === KEDA ===
kubectl get pods -n keda-system
kubectl logs -n keda-system -l app=keda-operator

# === vLLM / Application ===
kubectl get pods -n <app-namespace>
kubectl logs -n <app-namespace> <vllm-pod>
kubectl port-forward -n <app-namespace> <vllm-pod> 8000:8000

# === Configuration ===
kubectl get configmap -n "$NS"
kubectl get configmap wva-manager-config -n "$NS" -o yaml          # Prometheus URL, intervals
kubectl get configmap wva-scaling-policy-config -n "$NS" -o yaml   # thresholds, tiers, limiters
```

