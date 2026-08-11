# Workload-Variant-Autoscaler (WVA)

[![Go Report Card](https://goreportcard.com/badge/github.com/llm-d/llm-d-workload-variant-autoscaler)](https://goreportcard.com/report/github.com/llm-d/llm-d-workload-variant-autoscaler)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2Fllm-d%2Fllm-d-workload-variant-autoscaler.svg?type=shield)](https://app.fossa.com/projects/git%2Bgithub.com%2Fllm-d%2Fllm-d-workload-variant-autoscaler?ref=badge_shield)


The Workload Variant Autoscaler (WVA) is a Kubernetes-based global autoscaler for inference model servers serving LLMs. WVA works alongside the standard Kubernetes HPA and external autoscalers like KEDA to drive the scale subresource of inference deployments. The high-level details of the algorithms are documented [here](https://llm-d.ai/docs/architecture/advanced/autoscaling). It determines optimal replica counts for a given request traffic load by considering constraints such as GPU availability, energy budget, and performance budget (latency/throughput).

### What is a Variant?

WVA introduces the concept of **variants** — multiple model servers in an InferencePool that all serve the same base model but differ in hardware configuration (e.g., GPU type), serving configuration (e.g., tensor parallelism, max batch size, quantization), or both.

Use cases include:

- **P/D disaggregation**: prefill is one variant, decode is another — variant = role in a disaggregated pipeline.
- **[batch-gateway](https://github.com/llm-d-incubation/batch-gateway)**: variants distinguish batch vs. interactive workloads sharing the same pool.
- **Autoscaler**: a costed serving configuration the autoscaler chooses among.

## Key Features

- **Intelligent Autoscaling**: Optimizes replica count by observing the current state of the system
- **Cost Optimization**: Minimizes infrastructure costs by picking the correct accelerator variant

## Documentation

See the [architecture and autoscaling design](https://llm-d.ai/docs/architecture/advanced/autoscaling) docs for high-level algorithm details.

See the [docs](docs/README.md) directory for design docs, developer guide, and more.

## How It Works

**Prerequisites:** deploy llm-d infrastructure (model servers), have Prometheus
scraping them, and create a **KEDA ScaledObject** per workload whose trigger points
at WVA's external scaler.

**WVA then:**

1. Learns which workloads it manages **from the KEDA calls themselves** — there is
   no watch, no listing and no opt-in annotation. Being called is being managed,
   and the trigger `metadata` is the per-workload configuration.
2. Continuously reads request rates and server performance from Prometheus.
3. Runs its capacity model — KV-cache utilization, queue depth, token throughput —
   to decide the replica count each model needs, across all its variants at once
   and within the GPU budget any declared limiter allows.
4. Returns that decision to KEDA over the external-scaler gRPC contract. KEDA owns
   the HPA and actuates it; WVA never writes the scale subresource.

## Example

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: llama-8b-scaler
  namespace: llm-inference
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: llama-8b
  pollingInterval: 5
  minReplicaCount: 1        # 0 to allow scale-to-zero (alpha)
  maxReplicaCount: 10
  triggers:
  - type: external-push     # push: WVA wakes a parked workload immediately
    name: wva-external-scaler
    metadata:
      scalerAddress: wva-external-scaler.workload-variant-autoscaler-system.svc.cluster.local:9090
      modelID: meta/llama-3-8b   # required — the only field you must supply
      scalingPolicy: interactive # optional — a named policy tier
      variantCost: "10.0"        # optional — defaults to "10.0"
```

`modelID` is the one required field. The accelerator, the role, GPUs per replica
and the InferencePool are all **derived** from the workload itself, so they cannot
drift from reality. See
[deploy/README.md](deploy/README.md#what-the-controller-needs-from-a-workload).

More examples in [config/samples/keda/](config/samples/keda/).

> **Note:** WVA used to publish a `wva_desired_replicas` metric for an HPA to read
> through prometheus-adapter, with an `llm-d.ai/managed: "true"` annotation to opt
> in. Both are gone: WVA is a KEDA external scaler, and a trigger naming it is what
> registers a workload.

## Upgrading

### Upgrading to v0.9.0 — V2 saturation analyzer is now the default

**Behavioral change.** The default saturation analyzer changes from **V1**
(percentage/spare-capacity-based) to **V2** (token/capacity-based). The shipped
`default` entry in the saturation ConfigMap now includes an `analyzers:` section,
which selects V2. No code change or image rebuild is involved — analyzer selection
is driven entirely by config.

V2 may produce different scaling decisions than V1 for the same workload. Review
your dashboards and alert thresholds after upgrading.

**Staying on V1 (opt-out).** Remove the `analyzers:` section (and the V2-only
`scaleUpThreshold` / `scaleDownBoundary` fields) from the `default` entry of your
saturation ConfigMap. The remaining `kvCacheThreshold`, `queueLengthThreshold`,
`kvSpareTrigger`, and `queueSpareTrigger` fields drive V1:

```yaml
data:
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3
```

Apply with `kubectl apply -f deploy/configmap-scaling-policy.yaml`; the change
takes effect immediately (the controller watches the ConfigMap).

> V1 is deprecated and scheduled for removal in a future release. See the
> [saturation scaling configuration guide](docs/developer-guide/scaling-policy-config.md#analyzer-selection-v1-vs-v2)
> for threshold ownership (which fields each analyzer reads) and migration details.

## Contributing

We welcome contributions! See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

Join the [llm-d autoscaling community meetings](https://llm-d.ai/slack) to get involved.

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.


[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2Fllm-d%2Fllm-d-workload-variant-autoscaler.svg?type=large)](https://app.fossa.com/projects/git%2Bgithub.com%2Fllm-d%2Fllm-d-workload-variant-autoscaler?ref=badge_large)

## Related Projects

- [llm-d infrastructure](https://github.com/llm-d/llm-d-infra)
- [llm-d main repository](https://github.com/llm-d/llm-d)

## References
- [WVA paper](https://arxiv.org/abs/2603.09730)
- [WVA use case doc](https://docs.google.com/document/d/1ZcMXO0x42qn4X5cu6efgMomYC4pKPwm6r7L79y1AQH4/edit?tab=t.0)
- [Saturation based design discussion](https://docs.google.com/document/d/1iGHqdxRUDpiKwtJFr5tMCKM7RF6fbTfZBL7BTn6UkwA/edit?tab=t.0#heading=h.mdte0lq44ul4)