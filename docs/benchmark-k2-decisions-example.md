# Example: K1/K2 Capacity Decision Report

Sample output of [`hack/benchmark/dump_k2_decisions.py`](../hack/benchmark/dump_k2_decisions.py),
run against a `prefill_heavy` benchmark (`Qwen/Qwen3-0.6B`, single variant, TP=1) on a real
OpenShift cluster, with `GLOBAL_OPT_INTERVAL=15s` and the EPP `flowControl` gate enabled. Kept
here as a worked example of what the report looks like; run the script yourself against a fresh
results directory to get current numbers.

Per-iteration rows are aggregated one-per-cycle (totalled across every ready replica that
cycle, column `N`), not one row per replica — a cycle with several replicas at different
fallback tiers is one row, with `Priority` listing every tier that fired. `EPPq` here is real,
non-zero data during the ramp-up (13:04–13:06) — the EPP flow-control queue actually built up
before enough replicas came online to drain it.

Note on the trailing scale-down (13:12–13:14): this run hit 146 request errors (not visible in
this log-derived report, but worth knowing when reading the replica-count column) — almost all
`RemoteProtocolError` from decode pods killed mid-response-stream. `llm-d-model-service`'s decode
Deployment ships no `terminationGracePeriodSeconds`/`preStop` at all, so any scale-down can sever
an in-flight request; the fast `15s` cadence used here reacts to falling demand sooner, so it
triggers scale-down more often than a slower cadence would. Not a bug in this report or the
logging — a pre-existing chart-level gap that a faster reconcile loop exercises more.

---

# K1/K2 Capacity Decision Report

Window: 2026-08-15T13:03:01+00:00 -> 2026-08-15T13:16:56+00:00
Total events captured: 741

## Variant: qwen-qwe-3db867ce-en3-0-6b-decode-wva

### K2 fallback-tier distribution

| Priority | Meaning                                                             | Count | % of cycles |
|----------|---------------------------------------------------------------------|-------|-------------|
| P1-obs   | observed (queue saturated, tokensInUse used directly)               | 13    | 3.9%        |
| P2-hist  | historical (rolling average of prior observations)                  | 318   | 94.4%       |
| P4-k1    | fallback (no observed/historical/derived signal; memory-bound only) | 6     | 1.8%        |

### Tier transitions

| Time     | From           | To             | k2     |
|----------|----------------|----------------|--------|
| 13:03:15 | (start)        | P4-k1          | 487065 |
| 13:04:30 | P4-k1          | P1-obs         | 605760 |
| 13:05:15 | P1-obs         | P1-obs,P4-k1   | 605760 |
| 13:05:30 | P1-obs,P4-k1   | P1-obs,P2-hist | 606784 |
| 13:06:46 | P1-obs,P2-hist | P2-hist        | 607308 |

### Which bound governed (k1-memory vs k2-compute)

| Bound     | Cycles |
|-----------|--------|
| k1-memory | 337    |

k1 range: 487065 - 487065 tokens  
k2 range: 487065 - 608512 tokens

### Per-iteration detail

One row per optimize cycle, totalled across every ready replica of this variant that cycle (N). KVinUse/LocalQ/EPPq/TotalDemand are all in tokens; Priority lists every tier that fired across N replicas this cycle (see the fallback-tier distribution above for what each code means). Time is HH:MM:SS on the run date above.

Legend — Bound: k1=memory-bound won, k2=compute-bound won.

| Time     | N | Priority       | k2     | k1     | Bound | KVinUse | LocalQ  | EPPq      | TotalDemand |
|----------|---|----------------|--------|--------|-------|---------|---------|-----------|-------------|
| 13:03:15 | 1 | P4-k1          | 487065 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:03:30 | 1 | P4-k1          | 487065 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:03:45 | 1 | P4-k1          | 487065 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:04:00 | 1 | P4-k1          | 487065 | 487065 | k1    | 601663  | 0       | 613507    | 1215170     |
| 13:04:15 | 1 | P4-k1          | 487065 | 487065 | k1    | 601663  | 0       | 246561    | 848224      |
| 13:04:30 | 1 | P1-obs         | 605760 | 487065 | k1    | 605760  | 1277040 | 794884.25 | 2677684.25  |
| 13:04:45 | 1 | P1-obs         | 605760 | 487065 | k1    | 605760  | 1277040 | 1421034   | 3303834     |
| 13:05:00 | 1 | P1-obs         | 605760 | 487065 | k1    | 605760  | 1277040 | 1731492.5 | 3614292.5   |
| 13:05:15 | 2 | P1-obs,P4-k1   | 605760 | 487065 | k1    | 605760  | 1277040 | 60607.75  | 1943407.75  |
| 13:05:30 | 3 | P1-obs,P2-hist | 606784 | 487065 | k1    | 1215296 | 600960  | 127816    | 1944072     |
| 13:05:45 | 5 | P1-obs,P2-hist | 606784 | 487065 | k1    | 1216512 | 600960  | 155140.25 | 1972612.25  |
| 13:06:00 | 7 | P1-obs,P2-hist | 607594 | 487065 | k1    | 1216512 | 565904  | 47243.25  | 1829659.25  |
| 13:06:16 | 8 | P1-obs,P2-hist | 607594 | 487065 | k1    | 2095453 | 1161856 | 0         | 3257309     |
| 13:06:31 | 9 | P1-obs,P2-hist | 607232 | 487065 | k1    | 3120264 | 595952  | 0         | 3716216     |
| 13:06:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 3462637 | 0       | 0         | 3462637     |
| 13:07:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 3087941 | 0       | 0         | 3087941     |
| 13:07:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 2783780 | 0       | 0         | 2783780     |
| 13:07:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 2476036 | 0       | 0         | 2476036     |
| 13:07:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 2457986 | 0       | 0         | 2457986     |
| 13:08:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 2092507 | 0       | 0         | 2092507     |
| 13:08:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 1318602 | 0       | 0         | 1318602     |
| 13:08:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 1240834 | 0       | 0         | 1240834     |
| 13:08:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 1260998 | 0       | 0         | 1260998     |
| 13:09:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 1171772 | 0       | 0         | 1171772     |
| 13:09:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 1141367 | 0       | 0         | 1141367     |
| 13:09:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 1153209 | 0       | 0         | 1153209     |
| 13:09:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 1176571 | 0       | 0         | 1176571     |
| 13:10:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 1178555 | 0       | 0         | 1178555     |
| 13:10:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 1071024 | 0       | 0         | 1071024     |
| 13:10:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 994985  | 0       | 0         | 994985      |
| 13:10:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 866523  | 0       | 0         | 866523      |
| 13:11:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 897309  | 0       | 0         | 897309      |
| 13:11:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 891293  | 0       | 0         | 891293      |
| 13:11:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 922657  | 0       | 0         | 922657      |
| 13:11:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 1080945 | 0       | 0         | 1080945     |
| 13:12:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 1021676 | 0       | 0         | 1021676     |
| 13:12:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 1084146 | 0       | 0         | 1084146     |
| 13:12:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 1065712 | 0       | 0         | 1065712     |
| 13:12:46 | 8 | P2-hist        | 607308 | 487065 | k1    | 909983  | 0       | 0         | 909983      |
| 13:13:01 | 8 | P2-hist        | 607308 | 487065 | k1    | 1115701 | 0       | 0         | 1115701     |
| 13:13:16 | 7 | P2-hist        | 607308 | 487065 | k1    | 973798  | 0       | 0         | 973798      |
| 13:13:31 | 4 | P2-hist        | 607308 | 487065 | k1    | 1189757 | 0       | 0         | 1189757     |
| 13:13:46 | 4 | P2-hist        | 607308 | 487065 | k1    | 1359951 | 0       | 0         | 1359951     |
| 13:14:01 | 4 | P2-hist        | 607308 | 487065 | k1    | 1359951 | 0       | 0         | 1359951     |
| 13:14:16 | 4 | P2-hist        | 607308 | 487065 | k1    | 1359951 | 0       | 0         | 1359951     |
| 13:14:31 | 4 | P2-hist        | 607308 | 487065 | k1    | 367975  | 0       | 0         | 367975      |
| 13:14:46 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:15:01 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:15:16 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:15:31 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:15:46 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:16:01 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:16:16 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:16:31 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |
| 13:16:46 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           |

## No-cache-info fallback events

These pods never reported `vllm:cache_config_info`; capacity came from the capacity store instead of live KV-cache metrics.

| Time                      | Variant                               | Pod                                                 | Reason                                                  |
|---------------------------|----------------------------------------|-----------------------------------------------------|----------------------------------------------------------|
| 2026-08-15T13:12:46+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-zmt4k | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:01+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-zmt4k | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:16+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-clbpc | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:31+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-clbpc | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:31+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-q6rzt | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:31+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-hjrgv | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:31+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-nznct | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:46+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-nznct | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:46+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-q6rzt | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:13:46+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-hjrgv | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:14:01+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-nznct | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T13:14:01+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-q6rzt | no vllm:cache_config_info; using capacity-store record |
