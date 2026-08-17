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

1. **the model can actually reach zero**, which needs BOTH:
   - `config.ResolveScaleToZeroEnabled(policy)` is true. Scale-to-zero is a
     **model/tier** property, not a per-variant one: it is resolved from the
     scaling entry (with the `WVA_SCALE_TO_ZERO` fallback), and the allocation
     enforcer is the only thing that consults it.
   - **every** variant of the model has `minReplicas == 0`. A model is at zero
     only when nothing serves it, so a single variant with a floor of 1 keeps the
     model up and no wake is ever needed. This is the condition that makes it a
     model-level question rather than a per-variant one, and getting it wrong in
     either direction is a bug: ORing the two would warn about models that can
     never park, and checking one variant would warn about models that another
     variant is still serving.

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

## 3a. "This model will not park, and here is why"

Parking needs the policy AND every variant's floor to permit it (§3.1). When one
half permits and the other does not, the result is a **valid configuration that
does not do what it looks like it does** — and today WVA says nothing at all. The
only related logging is a `V(DEBUG)` line in the allocation enforcer naming which
branch it took: no metric, no Event, no message.

Neither case is an error, and neither should be rejected or alerted on. They are
reported because the operator's *expectation* is wrong, and nothing else will
tell them:

| what the operator sees | what actually happens |
|---|---|
| scale-to-zero enabled on the model | a variant with `minReplicas > 0` keeps serving, so the model never reaches zero and the setting is inert — idle GPUs, billed indefinitely |
| `minReplicas: 0` on every variant | the model's policy disables scale-to-zero, so WVA keeps the scaler active and the bounds are inert — the model simply never parks |

The second matters most with a **single variant**, where `minReplicas: 0` reads
exactly like a deliberate request to park and is quietly overridden by a policy
the operator may not know applies to them.

So: one condition — *can this model actually reach zero?* — reported with the
reason it cannot. One message, `Normal` rather than `Warning`, because nothing is
broken:

```
Model %q will not scale to zero: %s.
  reason "scale-to-zero is disabled for this model, though every variant permits it"
  reason "variant %q has minReplicas=%d, so the model cannot reach zero even
          though scale-to-zero is enabled"
```

**Evaluate this where the policy is resolved, not in the wake loop.** It is pure
configuration — no metrics, no cluster state — so it belongs in `internal/config`
at resolution time and fires once per config change. That removes the per-model
state map, the hourly re-emission, the eviction it would otherwise need, and the
10Hz rate limiting described in §6, none of which this check requires. §4's
metric and Event still apply; §6's machinery does not.

## 4. How to report it

Three surfaces, because each reaches a different reader.

**Metric** — for alerting, and the only surface that survives a restart:

```
wva_epp_flow_control_available{namespace, pool} 1|0
```

Keyed by **pool, not model**. Flow control is an EPP setting, so one EPP serving
five models yields one fact, not five; keying by model would inflate cardinality
and imply a per-model cause that does not exist. Positive polarity, matching the
existing `wva_saturation_metrics_up` rather than a `..._missing` negative.

A gauge, not a counter: the condition is a state and the operator needs to know
it is true *now*.

```
WVAScaleFromZeroCannotWake:
  expr: wva_epp_flow_control_available == 0
  for: 10m
```

`for: 10m` because it is briefly true and harmless at startup, before EPP has
been scraped at all.

Adding this metric touches **four** files, and missing the last one breaks CI:
`internal/constants/metrics.go`, `internal/metrics/metrics.go`,
`config/components/prometheus-alerts/prometheusrule.yaml`, and `wvaMetricNames`
in `test/e2e/prometheus_alerts_test.go`. That last one is not optional — the
alerts spec fails on any alert referencing a metric absent from that list, which
is exactly how `wva_node_access_denied` shipped broken and stayed hidden behind a
runtime skip.

**Event on the ScaledObject** — for `kubectl describe`, where an operator
debugging a stuck workload actually looks. Use `variant.EventTarget()`, which
returns a `*kedav1alpha1.ScaledObject`: events on the synthesized
`VariantAutoscaling` are silently dropped with `no kind is registered for the
type variant.VariantAutoscaling in scheme`, which is a live bug seen in the
pokprod logs today — a wake succeeded and left no Event behind.

**Log** — at `logger.Info` (V(0)), *only on transition*. Not "WARN": logr has
`Info` and `Error` and no Warn, and `internal/logging` defines only `DEBUG = 4`
and `TRACE = 5`. Operator-visible means V(0). The wake loop runs at 10Hz, so an
unconditional line would emit ~36,000 an hour and bury itself.

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

## 7. Hardening

### 7.1 Fix the cause first: hash the EPP config into its pod template

The failure that motivated all of this was **a stale EPP pod**. EPP reads
`--config-file` once at startup, so a ConfigMap that gains
`featureGates: [flowControl]` never reaches a pod already running. On kind the
ConfigMap enabled it and the process had parsed no gates at all; restarting the
Deployment produced `featureGates:["flowControl"]`, a flow-controller logger, and
14 metric families where there had been none.

Neither EPP Deployment on either cluster carries a config-hash annotation, so
this recurs on **every** ConfigMap edit. The canonical Kubernetes fix is one
annotation:

```yaml
spec:
  template:
    metadata:
      annotations:
        checksum/config: <sha of the EPP ConfigMap>
```

A config change then rolls the pods and the process can never be older than its
configuration. This belongs upstream in the llm-d EPP chart; our tooling can
apply it locally in the meantime, the way `fma_fixups.sh` already does for the
FMA defects.

**This is the primary remedy.** It removes the failure class at source rather
than compensating for it, and unlike a fallback it leaves no one running quietly
on a degraded signal.

### 7.2 A fallback wake signal — deferred, with the guard it would need

Considered and **not recommended now**:

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

**Why it is deferred.** The premise was that flow control may be absent in some
environments. That turned out false: it was one pod running a two-day-old config,
which §7.1 fixes at source. And `ServiceUnavailable` is not a clean proxy for
"capacity is needed" — it also fires for a mistyped model name, a misconfigured
pool, an endpoint failing readiness, or EPP overload. Waking a fleet because a
client typo'd a model ID is a real failure mode with a GPU bill attached. Queue
depth carries no such ambiguity: it means requests are waiting for capacity and
nothing else.

**If it is ever built**, it needs one guard beyond the above: require rejections
rising across several *consecutive* ticks, not a single delta, so a burst of
bad-model-name requests cannot wake anything.

**Order of work.** §7.1 first — it is a few lines of YAML and removes the cause.
Then the warning (§3–§6), which is read-only and cannot cause a spurious wake, so
a degraded EPP becomes visible in minutes instead of days. The fallback only if a
genuine no-flow-control environment appears.

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
