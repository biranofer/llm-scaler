# Example: K1/K2 Capacity Decision Report

Sample output of [`hack/benchmark/dump_k2_decisions.py`](../hack/benchmark/dump_k2_decisions.py),
run against a `prefill_heavy` benchmark (`Qwen/Qwen3-0.6B`, single variant, TP=1) on a real
OpenShift cluster. Kept here as a worked example of what the report looks like; run the script
yourself against a fresh results directory to get current numbers.

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

| Time                      | From    | To      | k2 (or k1 on P4) |
|---------------------------|---------|---------|------------------|
| 2026-08-15T09:17:41+00:00 | (start) | P4-k1   | 487065           |
| 2026-08-15T09:18:41+00:00 | P4-k1   | P1-obs  | 607552           |
| 2026-08-15T09:20:41+00:00 | P1-obs  | P2-hist | 607872           |
| 2026-08-15T09:20:41+00:00 | P2-hist | P1-obs  | 607872           |
| 2026-08-15T09:20:41+00:00 | P1-obs  | P2-hist | 607872           |
| 2026-08-15T09:21:41+00:00 | P2-hist | P1-obs  | 604736           |
| 2026-08-15T09:21:41+00:00 | P1-obs  | P2-hist | 607088           |

### Which bound governed (k1-memory vs k2-compute)

| Bound     | Cycles |
|-----------|--------|
| k1-memory | 79     |

k1 range: 487065 - 487065 tokens  
k2 range: 487065 - 608192 tokens

### Per-iteration detail

Every optimize cycle's k2 computation, with the inputs behind whichever fallback tier fired and the resulting k1-vs-k2 comparison. Rows sharing a timestamp are separate ready replicas of this variant in that cycle. Time is HH:MM:SS on the run date above; see legend for the Inputs/Bound codes.

Legend — Inputs: q=queueLength/queueThreshold, h=history-window sample count, in/out=avg input/output tokens this cycle, no-sig=no observed/historical/derived signal (fell all the way to k1).  Bound: k1=memory-bound won, k2=compute-bound won.

| Time     | Priority | Inputs    | k2     | k1     | Bound | Demand  | Queue |
|----------|----------|-----------|--------|--------|-------|---------|-------|
| 09:17:41 | P4-k1    | no-sig    | 487065 | 487065 | k1    | 0       | 0/5   |
| 09:18:41 | P1-obs   | q371/5 h1 | 607552 | 487065 | k1    | 2465520 | 371/5 |
| 09:19:41 | P1-obs   | q380/5 h2 | 608192 | 487065 | k1    | 2511232 | 380/5 |
| 09:20:41 | P2-hist  | h2        | 607872 | 487065 | k1    | 139343  | 0/5   |
| 09:20:41 | P1-obs   | q386/5 h3 | 607872 | 487065 | k1    | 2540960 | 386/5 |
| 09:20:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 132878  | 0/5   |
| 09:20:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 120653  | 0/5   |
| 09:20:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 244506  | 0/5   |
| 09:20:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 110348  | 0/5   |
| 09:21:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 324386  | 0/5   |
| 09:21:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 308320  | 0/5   |
| 09:21:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 356838  | 0/5   |
| 09:21:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 318305  | 0/5   |
| 09:21:41 | P2-hist  | h3        | 607872 | 487065 | k1    | 427885  | 0/5   |
| 09:21:41 | P1-obs   | q119/5 h4 | 604736 | 487065 | k1    | 1200688 | 119/5 |
| 09:21:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 336099  | 0/5   |
| 09:22:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 342436  | 0/5   |
| 09:22:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 294815  | 0/5   |
| 09:22:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 348581  | 0/5   |
| 09:22:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 391785  | 0/5   |
| 09:22:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 288862  | 0/5   |
| 09:22:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 313633  | 0/5   |
| 09:22:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 316897  | 0/5   |
| 09:22:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 273501  | 0/5   |
| 09:23:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 159313  | 0/5   |
| 09:23:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 129358  | 0/5   |
| 09:23:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 194900  | 0/5   |
| 09:23:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 165265  | 0/5   |
| 09:23:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 243610  | 0/5   |
| 09:23:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 163089  | 0/5   |
| 09:23:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 190100  | 0/5   |
| 09:23:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 148176  | 0/5   |
| 09:24:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 133646  | 0/5   |
| 09:24:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 112524  | 0/5   |
| 09:24:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 136590  | 0/5   |
| 09:24:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 117132  | 0/5   |
| 09:24:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 125901  | 0/5   |
| 09:24:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 121997  | 0/5   |
| 09:24:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 134606  | 0/5   |
| 09:24:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 132238  | 0/5   |
| 09:25:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 107595  | 0/5   |
| 09:25:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 134478  | 0/5   |
| 09:25:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 143055  | 0/5   |
| 09:25:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 143567  | 0/5   |
| 09:25:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 125069  | 0/5   |
| 09:25:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 114636  | 0/5   |
| 09:25:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 148048  | 0/5   |
| 09:25:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 125645  | 0/5   |
| 09:26:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 152016  | 0/5   |
| 09:26:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 134542  | 0/5   |
| 09:26:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 126349  | 0/5   |
| 09:26:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 119245  | 0/5   |
| 09:26:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 133710  | 0/5   |
| 09:26:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 110540  | 0/5   |
| 09:26:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 118028  | 0/5   |
| 09:26:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 134798  | 0/5   |
| 09:27:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 152592  | 0/5   |
| 09:27:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 180627  | 0/5   |
| 09:27:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 138575  | 0/5   |
| 09:27:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 196693  | 0/5   |
| 09:27:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 194772  | 0/5   |
| 09:27:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 116940  | 0/5   |
| 09:27:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 162513  | 0/5   |
| 09:27:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 136910  | 0/5   |
| 09:28:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:28:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:28:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:28:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 135758  | 0/5   |
| 09:28:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 182291  | 0/5   |
| 09:28:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 150032  | 0/5   |
| 09:28:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 182355  | 0/5   |
| 09:29:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:29:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:29:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:29:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:30:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:30:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:30:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |
| 09:30:41 | P2-hist  | h4        | 607088 | 487065 | k1    | 0       | 0/5   |

## No-cache-info fallback events

These pods never reported `vllm:cache_config_info`; capacity came from the capacity store instead of live KV-cache metrics.

| Time                      | Variant                               | Pod                                                 | Reason                                                  |
|---------------------------|----------------------------------------|-----------------------------------------------------|----------------------------------------------------------|
| 2026-08-15T09:28:41+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-v7sbt | no vllm:cache_config_info; using capacity-store record |
| 2026-08-15T09:29:41+00:00 | qwen-qwe-3db867ce-en3-0-6b-decode-wva | qwen-qwe-3db867ce-en3-0-6b-decode-5c6d8d69bd-r58zr | no vllm:cache_config_info; using capacity-store record |
