# Tell the operator when scale-to-zero can never wake

**Status:** proposal

## 1. The failure this prevents

A variant configured with `minReplicaCount: 0` parks at zero and never comes
back, and nothing anywhere says why. The autoscaler looks healthy: it is
running, it logs, it emits metrics, it reconciles. The model is simply gone.

The cause is that scale-from-zero has exactly one input — the EPP flow-control
queue — and EPP only exports it when the `flowControl` feature gate is enabled in
the config file it actually loads. Two things make that easy to get wrong:

- the gate lives in one of several config files in the same ConfigMap, and only
  the one named by `--config-file` counts;
- an EPP with flow control off is otherwise completely normal. It routes, it
  serves, it exports 900+ other metrics.

So the operator sees a working EPP, a working WVA, and a model that will not
start.

## 2. That this is not hypothetical

Measured on kind, 152 samples across a full spec run: no
`llm_d_epp_flow_control_queue_size` series ever appeared — nor
`pool_saturation`, nor the enqueue counter — while
`llm_d_epp_request_error_total{error_code="ServiceUnavailable"}` climbed by 40.
Requests arrived, were refused, and no wake was possible. The e2e reported this
as a WVA failure for at least a week (the same specs fail in gate logs dated
2026-08-10), and three separate investigations blamed the wrong component before
the signal was traced.

The same code on pokprod works: queue depth held 40, the engine logged
`Published scale-from-zero activation for Target Workload`, and the deployment
went `0/0 -> 1/1`. Identical EPP image digest
(`sha256:873179...af6f5`), identical feature gate in the loaded config. Whatever
the environmental difference is, WVA cannot control it — but it can *report* it.

## 3. What to detect

Warn when both hold for a model:

1. **the model can reach zero**, by either of the two independent routes:
   - **WVA parks it** — `config.ResolveScaleToZeroEnabled(policy)` is true for
     that model. Scale-to-zero is a **model/tier** property, not a per-variant
     one: it is resolved from the scaling entry (with the
     `WVA_SCALE_TO_ZERO` fallback), and the allocation enforcer is the only
     thing that consults it. A variant whose model forbids scale-to-zero must
     never be parked by WVA, so on its own it is not a reason to warn.
   - **KEDA parks it** — the ScaledObject carries `minReplicaCount: 0`. KEDA owns
     the replica bounds and will honour that whatever WVA's policy says, so this
     route reaches zero without WVA's involvement and still needs a wake signal.

   Checking only the first misses workloads parked by KEDA; checking only the
   second warns about variants WVA would never park. Both, ORed.

2. **the wake signal is absent** — no flow-control queue series exists for the
   pool serving that model, under either name
   (`llm_d_epp_flow_control_queue_size` or the deprecated
   `inference_extension_flow_control_queue_size`).

All three inputs are available where the engine runs: the policy is already
resolved per model, the replica bounds come from the registry's `Target`, and the
metric values are the same `results` slice `pendingRequestsForModel` reads.

**Absence, not zero.** The distinction matters more than it looks: an idle queue
reports `0` and a queue that does not exist reports nothing at all. Every wrong
conclusion reached while diagnosing this came from conflating them. The existing
`pendingRequestsForModel` already tracks `exported` for exactly this reason, so
the information is in hand — it is simply discarded today.

## 4. How to report it

Three surfaces, because each reaches a different reader.

**Metric** — for alerting, and the only one that survives a restart:

```
wva_scale_from_zero_signal_missing{namespace, model} 1|0
```

A gauge, not a counter: the condition is a state, and an operator wants to know
it is true *now*. It also gives the natural alert:

```
WVAScaleFromZeroCannotWake:
  expr: wva_scale_from_zero_signal_missing == 1
  for: 10m
```

`for: 10m` because the condition is briefly true and harmless during startup,
before EPP has been scraped at all.

**Event on the ScaledObject** — for `kubectl describe`, where an operator
debugging a stuck workload actually looks. Use `variant.EventTarget()`, which
returns a `*kedav1alpha1.ScaledObject`: events on the synthesized
`VariantAutoscaling` are silently dropped with `no kind is registered for the
type variant.VariantAutoscaling in scheme`, which is a live bug seen in the
pokprod logs today — a wake succeeded and left no Event behind.

**Log** — at WARN, and *only on transition*. The scale-from-zero loop runs at
10Hz; an unconditional line would emit ~36,000 warnings an hour and bury itself.

## 5. Wording

The message must name the component, the consequence, and the fix. This is the
whole point of the proposal — a warning that says "no pending requests" would be
true and useless.

```
Scale-to-zero is enabled for model %q but EPP exports no flow-control queue
metric, so this model can never be woken. Enable the flowControl feature gate in
the EPP config file named by --config-file (several may exist in the same
ConfigMap; only the loaded one counts), or set minReplicas >= 1.
```

## 6. Rate limiting

State-change only, held per `(namespace, model)`:

- emit on false -> true, and on true -> false ("signal restored")
- re-emit no more than once an hour while it stays true, so a long-running
  operator sees it without the log being swamped

The engine already holds per-model state under a mutex for the limiter, so this
adds a map, not a mechanism.

## 7. Hardening: wake without flow control

Warning is not enough on its own — a model that cannot wake is still down. So
add a **fallback signal that does not depend on the feature gate**:

```
llm_d_epp_request_error_total{error_code="ServiceUnavailable", target_model_name="..."}
```

It is the only signal observed in **both** environments. On kind, where flow
control never runs and no queue series exists, this counter still moved **+40**
during the failing spec. It is per-model and carries `target_model_name`, the
label `pendingRequestsForModel` already filters on.

**Primary and fallback, not equals.** Queue depth stays authoritative wherever it
exists; rejections are consulted only when the queue family is absent. A
rejection is weaker evidence — requests are refused for reasons other than "no
capacity" — so it must not override a queue that is present and reporting zero.

**It is a counter, so activate on an increase.** Three consequences the
implementation must get right, and the reason this is not a one-line change:

- **baseline per (namespace, model)**, held beside the existing limiter state. A
  first observation establishes the baseline and must NOT activate, or every
  restart wakes every model that has ever been refused.
- **counter resets**: EPP restarting drops the value. A decrease is a reset —
  re-baseline and do not activate.
- **the 10Hz loop** means the delta window is one tick. Compare against the last
  observed value, not against a rate over a window the collector may not have.

**Order of work.** The warning (§3–§6) lands first and is independently useful:
it is read-only, cannot cause a spurious wake, and makes the condition visible
while the fallback is written and tested. The fallback then removes the outage.

## 8. Deliberately not in scope

**Do not** try to fix EPP from WVA. The gate is EPP's configuration, and the
difference between environments is still not understood — identical image digest
(`sha256:873179...af6f5`), identical gate in the loaded config, opposite
behaviour. Reporting it accurately and waking anyway is what WVA can honestly
do.

## 9. Why this is worth building

Every hour spent on this problem went to discovering that the signal was absent.
The autoscaler knew — `pendingRequestsForModel` distinguishes "exported but zero"
from "not exported" on every single tick, and throws that away. Surfacing what
the code already computes turns a silent, week-long misattribution into a line
telling the operator which component to fix.
