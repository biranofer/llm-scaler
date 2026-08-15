# Example: K1/K2 Capacity Decision Report

Sample output of [`hack/benchmark/dump_k2_decisions.py`](../hack/benchmark/dump_k2_decisions.py),
run against a `prefill_heavy` benchmark (`Qwen/Qwen3-0.6B`, single variant, TP=1) on a real
OpenShift cluster. Kept here as a worked example of what the report looks like; run the script
yourself against a fresh results directory to get current numbers.

Per-iteration rows are aggregated one-per-cycle (totalled across every ready replica that
cycle, column `N`), not one row per replica — a cycle with several replicas at different
fallback tiers is one row, with `Priority` listing every tier that fired. `LocalQ`/`EPPq` are
0 throughout this particular run because it predates the demand-breakdown logging
(`localQueueDemand` on `replica-capacity-decision`, and the `scheduler-queue-demand` line) —
`TotalDemand` is still correct since it used the pre-existing combined field.

---

# K1/K2 Capacity Decision Report

Window: 2026-08-15T09:17:06+00:00 -> 2026-08-15T09:31:26+00:00
Total events captured: 160

## Variant: qwen-qwe-3db867ce-en3-0-6b-decode-wva

### K2 fallback-tier distribution

| Priority | Meaning                                                             | Count | % of cycles |
|----------|---------------------------------------------------------------------|-------|-------------|
| P1-obs   | observed (queue saturated, tokensInUse used directly)               | 4     | 5.1%        |
| P2-hist  | historical (rolling average of prior observations)                  | 74    | 93.7%       |
| P4-k1    | fallback (no observed/historical/derived signal; memory-bound only) | 1     | 1.3%        |

### Tier transitions

| Time     | From           | To             | k2     |
|----------|----------------|----------------|--------|
| 09:17:41 | (start)        | P4-k1          | 487065 |
| 09:18:41 | P4-k1          | P1-obs         | 607552 |
| 09:20:41 | P1-obs         | P1-obs,P2-hist | 607872 |
| 09:22:41 | P1-obs,P2-hist | P2-hist        | 607088 |

### Which bound governed (k1-memory vs k2-compute)

| Bound     | Cycles |
|-----------|--------|
| k1-memory | 79     |

k1 range: 487065 - 487065 tokens  
k2 range: 487065 - 608192 tokens

### Per-iteration detail

One row per optimize cycle, totalled across every ready replica of this variant that cycle (N). KVinUse/LocalQ/EPPq/TotalDemand are all in tokens; Priority lists every tier that fired across N replicas this cycle (see the fallback-tier distribution above for what each code means). Time is HH:MM:SS on the run date above.

Legend — Bound: k1=memory-bound won, k2=compute-bound won.

| Time     | N | Priority       | k2     | k1     | Bound | KVinUse | LocalQ | EPPq | TotalDemand |
|----------|---|----------------|--------|--------|-------|---------|--------|------|-------------|
| 09:17:41 | 1 | P4-k1          | 487065 | 487065 | k1    | 0       | 0      | 0    | 0           |
| 09:18:41 | 1 | P1-obs         | 607552 | 487065 | k1    | 607552  | 0      | 0    | 2465520     |
| 09:19:41 | 1 | P1-obs         | 608192 | 487065 | k1    | 608192  | 0      | 0    | 2511232     |
| 09:20:41 | 6 | P1-obs,P2-hist | 607872 | 487065 | k1    | 1355600 | 0      | 0    | 3288688     |
| 09:21:41 | 7 | P1-obs,P2-hist | 607872 | 487065 | k1    | 2676569 | 0      | 0    | 3272521     |
| 09:22:41 | 8 | P2-hist        | 607088 | 487065 | k1    | 2570510 | 0      | 0    | 2570510     |
| 09:23:41 | 8 | P2-hist        | 607088 | 487065 | k1    | 1393811 | 0      | 0    | 1393811     |
| 09:24:41 | 8 | P2-hist        | 607088 | 487065 | k1    | 1014634 | 0      | 0    | 1014634     |
| 09:25:41 | 8 | P2-hist        | 607088 | 487065 | k1    | 1042093 | 0      | 0    | 1042093     |
| 09:26:41 | 8 | P2-hist        | 607088 | 487065 | k1    | 1029228 | 0      | 0    | 1029228     |
| 09:27:41 | 8 | P2-hist        | 607088 | 487065 | k1    | 1279622 | 0      | 0    | 1279622     |
| 09:28:41 | 7 | P2-hist        | 607088 | 487065 | k1    | 650436  | 0      | 0    | 650436      |
| 09:29:41 | 4 | P2-hist        | 607088 | 487065 | k1    | 0       | 0      | 0    | 0           |
| 09:30:41 | 4 | P2-hist        | 607088 | 487065 | k1    | 0       | 0      | 0    | 0           |

## No-cache-info fallback events

These pods never reported `vllm:cache_config_info`; capacity came from the capacity store instead of live KV-cache metrics.

| Time                      | Variant                               | Pod                                                 | Reason                                                  |
|---------------------------|----------------------------------------|-----------------------------------------------------|----------------------------------------------------------|
| 2026-08-15T09:28:41+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-v7sbt | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T09:29:41+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-r58zr | no vllm:cache_config_info; using capacity-store record |
