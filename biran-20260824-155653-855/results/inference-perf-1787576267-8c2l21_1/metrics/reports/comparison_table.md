# Comparison Table — WVA (clamp+floor)

burst_4k1000_6_14_6_14_6, 4000/1000 in/out, staged burst 6->14->6->14->6
RPS, 720s each. Qwen/Qwen3-0.6B on H100, TP=1. Controller built from
upstream ev-shindin/llm-scaler feat/clamp-plus-floor
(ghcr.io/biranofer/llm-scaler:clamp-plus-floor).


| Metric                   | WVA (clamp+floor) |
|--------------------------|-------------------|
| Avg TTFT (ms)            | 10,110            |
| P50 TTFT (ms)            | 63                |
| P95 TTFT (ms)            | 59,461            |
| P99 TTFT (ms)            | 73,870            |
| Avg TPOT (ms/token)      | 13.68             |
| P50 TPOT (ms/token)      | 10.88             |
| P95 TPOT (ms/token)      | 25.20             |
| P99 TPOT (ms/token)      | 44.89             |
| Avg replicas             | 3.49              |
| Max replicas             | 10                |
| GPU time (GPU-min)       | 305.9             |
| Avg KV cache utilization | 18.1%             |
| Avg queue depth (EPP)    | 3.4               |
| Error count              | 253               |
| Avg pod startup (s)      | 75                |


See comparison_table_vs_main_sync.md in this same directory for the
head-to-head against the baseline run and per-transition analysis.
