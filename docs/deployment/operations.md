# After the install

Verifying WVA works, watching what it decides, and the first things to check when it does not.

> Part of the [WVA deployment guide](../../deploy/README.md).

## Verifying the install

```bash
NS=workload-variant-autoscaler-system
kubectl get pods -n $NS                       # the controller is Running
kubectl get scaledobject -A                   # your managed workloads
kubectl get hpa -A                            # KEDA created one per ScaledObject
```

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

The log lines worth knowing, all at Info:

| grep for | tells you |
| --- | --- |
| `scaling-decision` | what WVA decided for a model, and the replica counts |
| `Effective scaling policy` | which policy tier a model resolved to |
| `Collected replica metrics` | metrics are arriving |
| `GPU limiter (re)built from config` | a `limiters:` edit took effect, live |

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
| WVA pod not `Running` | image pull, resources, or Prometheus unreachable | `kubectl describe pod -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler` |
| "Metrics unavailable" in the logs | the ServiceMonitor does not select your model pods, so the series never reach Prometheus | `kubectl get servicemonitor -A`, then Prometheus `/targets` |
| HPA exists but `CurrentMetrics` is empty | KEDA never got an answer — usually the trigger's `scalerAddress` or a missing `modelID` | `kubectl describe hpa -n <ns> keda-hpa-<so-name>` |
| nothing scales, no errors | a limiter is declared and the workload's accelerator does not resolve, so it gets no GPU budget | `kubectl logs -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler \| grep -i accelerator` |
| a model never wakes from zero | the EPP flow-control queue is not reaching WVA | see [Troubleshooting](../developer-guide/troubleshooting.md) |

First stop for any of these:

```bash
kubectl logs -n workload-variant-autoscaler-system   -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200
```

Deeper diagnosis — EPP metrics, scale-from-zero, slow scale-up — is in
[Troubleshooting](../developer-guide/troubleshooting.md).

## Command cheatsheet

```bash
# === WVA Controller ===
kubectl get pods -n workload-variant-autoscaler-system
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler -f
kubectl describe deployment controller-manager -n workload-variant-autoscaler-system

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
kubectl get configmap -n workload-variant-autoscaler-system
kubectl get configmap service-classes -n workload-variant-autoscaler-system -o yaml
kubectl get configmap model-accelerator-data -n workload-variant-autoscaler-system -o yaml
```

