# Comparison Table — burst_4k1000_6_14_6_14_6 — WVA vs KEDA well-lit path

Qwen/Qwen3-0.6B on H100, single variant TP=1. 4000/1000 in/out tokens,
staged burst 6 -> 14 -> 6 -> 14 -> 6 RPS, 720s each. Both arms ran the
identical workload against the same Deployment; only the ScaledObject
driving it differed (WVA's external-push scaler was fully removed for
the KEDA WL Path run, then restored afterward).

KEDA WL Path is llm-d's well-lit-path default: scale up at 50 queued
requests / 16 running requests per pod (Prometheus AverageValue
triggers against inference_extension_flow_control_queue_size and
inference_objective_running_requests), scale down 100%/15s. Adapted
from workload-variant-autoscaler's own comparison study
(hack/benchmark/scenarios/keda-epp/scaledobject-t50.yaml there) onto
this cluster's thanos-querier + bearer auth.


| Metric                   | WVA    | KEDA WL Path |
|--------------------------|--------|--------------|
| Avg TTFT (ms)            | 1,098  | 2,035        |
| P50 TTFT (ms)            | 65     | 56           |
| P95 TTFT (ms)            | 8,529  | 12,294       |
| P99 TTFT (ms)            | 18,480 | 41,976       |
| Avg TPOT (ms/token)      | 12.31  | 11.26        |
| P50 TPOT (ms/token)      | 10.65  | 10.16        |
| P95 TPOT (ms/token)      | 24.54  | 22.71        |
| P99 TPOT (ms/token)      | 32.63  | 28.77        |
| Avg replicas             | 2.78   | 4.25         |
| Max replicas             | 5      | 10           |
| GPU time (GPU-min)       | 235.7  | 362.1        |
| Avg KV cache utilization | 21.9%  | 12.6%        |
| Avg queue depth (EPP)    | 1.4    | 0.6          |
| Error count              | 312    | 363          |
| Avg pod startup (s)      | 65     | 66           |


No Avg column: these are two different arms (WVA vs stock KEDA), not
repeated trials of the same one, so averaging them isn't a meaningful
number.

WVA held fewer average/max replicas and less GPU time, with a
cleaner tail (P99 TTFT 18.5s vs 42.0s) and fewer errors (312 vs 363),
despite KEDA WL Path's higher error count going toward the SAME
transport timeout signature both arms show (see each run's own
comparison_table footnote) rather than a different failure mode.

GPU time uses each run's own full replica-sampler window (harness
start to stop), not just the 60 min of active load-gen -- see
comparison_table.py's docstring. Both runs independently hit a ~1-2.5h
idle gap between the harness finishing its own work and Kubernetes
reporting the pod Completed -- reproduced on 3 separate runs now,
cause still not root-caused (pod cleanup happens too fast to catch
live); it inflates each run's wall-clock but not its GPU-time number,
since that's derived from the replica-sampler's own window, not the
orchestrator's completion-detection wait.
