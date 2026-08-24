# Comparison Table — WVA (main-sync) vs WVA (feat/clamp-plus-floor)

Same workload as the main table (burst_4k1000_6_14_6_14_6, 4000/1000 in/out,
staged burst 6->14->6->14->6 RPS, 720s each), same Qwen/Qwen3-0.6B on H100
TP=1 deployment. Only the WVA controller image differs: WVA (main-sync) is
the baseline (ghcr.io/biranofer/llm-scaler:main-sync, same run as in
biran-20260822-021153-340's comparison_table.md); WVA (clamp+floor) is
built from upstream ev-shindin/llm-scaler branch feat/clamp-plus-floor,
which combines the arrival-rate demand floor
(internal/engines/analyzers/saturation_v2/arrival_demand.go) with the
conceded-replica clamp (steadystate.clampReplicaCountToScaleTarget).


| Metric                   | WVA (main-sync) | WVA (clamp+floor) |
|--------------------------|-----------------|-------------------|
| Avg TTFT (ms)            | 1,098           | 10,110            |
| P50 TTFT (ms)            | 65              | 63                |
| P95 TTFT (ms)            | 8,529           | 59,461            |
| P99 TTFT (ms)            | 18,480          | 73,870            |
| Avg TPOT (ms/token)      | 12.31           | 13.68             |
| P50 TPOT (ms/token)      | 10.65           | 10.88             |
| P95 TPOT (ms/token)      | 24.54           | 25.20             |
| P99 TPOT (ms/token)      | 32.63           | 44.89             |
| Avg replicas             | 2.78            | 3.49              |
| Max replicas             | 5               | 10                |
| GPU time (GPU-min)       | 235.7           | 305.9             |
| Avg KV cache utilization | 21.9%           | 18.1%             |
| Avg queue depth (EPP)    | 1.4             | 3.4               |
| Error count              | 312             | 253               |
| Avg pod startup (s)      | 65              | 75                |


No Avg column: different controller code, not repeated trials.

The aggregate numbers make clamp+floor look worse (P99 TTFT 73.9s vs
18.5s), but that's driven by one specific event, not a general
regression across the board. Per-transition analysis of the two
14->6 RPS drops in each run:

  - Both runs' FIRST 14->6 transition: main-sync collapsed to 1
    replica, causing a real (EPPq~238K) queue backup and an overshoot
    to 5. clamp+floor held at 2 replicas for 4.5 minutes with ZERO
    queue backup -- the demand floor visibly doing its documented job.
  - Both runs' SECOND 14->6 transition: both are clean (main-sync
    settles at 1, clamp+floor at 2; neither shows queue buildup).
  - The 6->14 RAMP immediately after the FIRST transition, in
    clamp+floor, produced a queue spike (~11.3M peak EPPq) roughly
    10x larger than the equivalent ramp in main-sync (~1.15M peak),
    despite starting from a numerically SAFER position (2 replicas
    vs main-sync's 1). This one event dominates clamp+floor's P95/P99
    TTFT for the whole run.

Not yet diagnosed: whether the clamp mechanism (capping supply at what
a scale-down already committed to) is inadvertently throttling RC
(replicas-to-add) reactivity to the following demand surge, or whether
this is run-to-run variance (the -340 vs -398 baseline comparison
already showed the SAME transition can behave very differently run to
run on unmodified code). One run each -- not enough to separate a
systematic effect of this branch from noise.
