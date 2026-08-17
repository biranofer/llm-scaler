# Say when a model will not park, and when it cannot wake

**Status:** proposal

## 1. Two silent failures

Both end with a model in the wrong state and nothing anywhere saying why.

**It will never park.** Scale-to-zero is enabled, but one variant has
`minReplicas > 0`, so something always serves and the model never reaches zero.
The operator believes idle models cost nothing. They do not. There is no symptom
to notice: the model is up and serving, and every metric says it should be.

**It cannot wake.** Every variant permits zero and the model parks, but the wake
signal never arrives, so it stays parked. The autoscaler looks healthy — running,
reconciling, emitting metrics. The model is simply gone.

WVA reports neither. The only related logging is a `V(DEBUG)` line in the
allocation enforcer naming which branch it took.

## 2. That this is not hypothetical

Measured on kind, 152 samples across a full spec run: no
`llm_d_epp_flow_control_queue_size` series ever appeared — nor `pool_saturation`,
nor the enqueue counter — while
`llm_d_epp_request_error_total{error_code="ServiceUnavailable"}` climbed by 40.
Requests arrived, were refused, and no wake was possible.

The cause was **a stale EPP pod**. EPP reads `--config-file` once at startup, so a
ConfigMap that gains `featureGates: [flowControl]` never reaches a pod already
running. That pod was ~2 days old and had parsed no feature gates at all;
restarting it produced `featureGates:["flowControl"]`, a flow-controller logger,
and 14 metric families where there had been none — and the scale-from-zero specs
went from failing to `10 Passed | 0 Failed`.

It cost a week of red e2e runs and three investigations that each blamed the
wrong component, because every artifact matched: same image digest, same
ConfigMap, same feature gate, same auth. The difference was *when the process
last read its config*, which appears in none of them.

The same code on pokprod wakes correctly: queue depth 40, `Published
scale-from-zero activation`, `0/0 -> 1/1` in under 10s.

## 3. Scope

WVA does not deploy EPP, so the root cause is not ours to fix (§8). What is ours
is to **notice, and say so**. The four items below are ordered by cost and by how
much they depend on anything outside this repo.

**Absence is not zero.** An idle queue reports `0`; a queue that does not exist
reports nothing. Every wrong conclusion reached while diagnosing this came from
conflating them. `pendingRequestsForModel` already distinguishes the two on every
tick and discards it — most of what follows is surfacing what is already computed.

## 4. First: say when a model will never park

Parking needs **both** halves to permit it:

- `config.ResolveScaleToZeroEnabled(policy)` is true. Scale-to-zero is a
  **model/tier** property, resolved from the scaling entry (with the
  `WVA_SCALE_TO_ZERO` fallback); the allocation enforcer is the only consumer.
- **every** variant of the model has `minReplicas == 0`. A model is at zero only
  when nothing serves it, so one variant with a floor of 1 keeps it up.

When one half permits and the other does not, the result is a **valid
configuration that does not do what it looks like it does**:

| what the operator sees | what actually happens |
|---|---|
| scale-to-zero enabled on the model | a variant with `minReplicas > 0` keeps serving, so the model never reaches zero and the setting is inert — idle GPUs, billed indefinitely |
| `minReplicas: 0` on every variant | the model's policy disables scale-to-zero, so WVA keeps the scaler active and the bounds are inert |

The second matters most with a **single variant**, where `minReplicas: 0` reads
exactly like a deliberate request to park.

Neither is an error and neither should be rejected or alerted on. One condition —
*can this model reach zero?* — reported with the reason it cannot, as `Normal`
rather than `Warning`, because nothing is broken:

```
Model %q will not scale to zero: %s.
  reason "scale-to-zero is disabled for this model, though every variant permits it"
  reason "variant %q has minReplicas=%d, so the model cannot reach zero even
          though scale-to-zero is enabled"
```

**Why this is first.** It is the only item with no external dependency: pure
configuration, evaluated in `internal/config` where the policy is already
resolved, once per config change. No EPP, no flow control, no wake path, no
metric join, no hot-path state, and no way for it to cause a spurious wake.

It also catches a failure **nothing else can see**: a model that will never park
on a cluster where EPP is perfectly healthy has no symptom to alert on. It is up
and serving, exactly as every metric says it should be.

## 5. Second: say when the wake signal is absent

```
wva_epp_flow_control_available{namespace, model} 1|0
```

Keyed by **model**. The contract is one model to one InferencePool, so
pool-keying saves nothing today, and the documented direction is *per-role pools
for a single model* — one model to many pools — under which pool-keying would
emit several series for one fact. Model is also the key every other WVA metric
uses, and the one the reader needs: the question is "can this model wake?". If a
model ever spans pools, add `pool` as a label rather than promoting it to the
identity.

Positive polarity, matching `wva_saturation_metrics_up` rather than a
`..._missing` negative.

**This is nearly free.** `pendingRequestsForModel` already computes the boolean;
setting a gauge from it is a few lines. And a gauge needs **no rate limiting** —
Prometheus gauges are idempotent, so setting one at 10Hz costs nothing. The
transition tracking in §7 is for logs and events only.

What it buys is naming the cause quickly. It is a diagnosis accelerator, not a
detector — §6 is what tells you something is wrong.

## 6. Third: alert on the symptom, not the cause

§5 detects one cause. The failure an operator cares about is "my model is parked
and will not wake", which has many: stale EPP, missing gate, RBAC denial, KEDA
stuck, an engine bug. A cause-specific detector only catches the cause someone
already thought of — and the one that bit us was invisible for a week precisely
because nobody had.

Both halves already exist as metrics:

```
wva_current_replicas == 0
rate(llm_d_epp_request_error_total{error_code="ServiceUnavailable"}[5m]) > 0
```

A model at zero while requests are being refused is broken **whatever the
reason**, and this needs no engine code at all.

**Two real costs, not to be waved away.** The families do not share labels — WVA
uses `variant_name`/`exported_namespace`, EPP uses `model_name`/
`target_model_name`, and EPP's pool metrics carry no namespace — so the join
needs `label_replace` and depends on modelID and variant naming lining up. And
the alerts spec validates that expressions reference only metrics in
`wvaMetricNames`, so an alert naming an EPP metric fails until that list allows
it. That is the same trap that let `wva_node_access_denied` ship broken.

## 7. Reporting surfaces and their conventions

**Metric** — the only surface that survives a restart, and the one alerts read.
Adding one touches **four** files and missing the last breaks CI:
`internal/constants/metrics.go`, `internal/metrics/metrics.go`,
`config/components/prometheus-alerts/prometheusrule.yaml`, and `wvaMetricNames`
in `test/e2e/prometheus_alerts_test.go`.

**Event on the ScaledObject** — for `kubectl describe`, where an operator
debugging a stuck workload looks. Use `variant.EventTarget()`: events on the
synthesized `VariantAutoscaling` are silently dropped with `no kind is registered
for the type variant.VariantAutoscaling in scheme`, a live bug seen in pokprod
logs today — a wake succeeded and left no Event behind. Rely on client-go's
recorder for spam filtering; it already aggregates similar events per object.

**Log** — `logger.Info` at V(0), on transition only. Not "WARN": logr has `Info`
and `Error` and no Warn, and `internal/logging` defines only `DEBUG = 4` and
`TRACE = 5`. Any transition state held per model must be evicted in step with the
registry's `DefaultTTL` (5 minutes), or it retains an entry for every model ever
seen.

## 8. The root cause, which is not ours

The stale-config failure is EPP's: it reads `--config-file` once and nothing
restarts it when the ConfigMap changes. The canonical Kubernetes fix is one
annotation on the pod template:

```yaml
annotations:
  checksum/config: <sha of the EPP ConfigMap>
```

Neither EPP Deployment on either cluster carries one, so this recurs on **every**
ConfigMap edit. **File it against the llm-d EPP chart.** It is named here so the
reader knows where the real fix lives — but WVA cannot land it, and must not
assume someone else will.

## 9. Deferred: a fallback wake signal

`llm_d_epp_request_error_total{error_code="ServiceUnavailable"}` is the one
signal present even when flow control is not: it moved +40 on kind while no queue
series existed. A fallback would have kept scale-from-zero working through those
two days.

Deferred, not dismissed — and §8 is why it cannot simply be dismissed: the root
cause is outside our control, so the trigger may persist.

Against it: `ServiceUnavailable` is not a clean proxy for "capacity is needed". It
also fires for a mistyped model name, a misconfigured pool, an endpoint failing
readiness, or EPP overload. Waking a fleet because a client typo'd a model ID has
a GPU bill attached. Queue depth carries no such ambiguity.

If built, it needs three guards: consulted **only** when the queue family is
absent (not when the queue reads zero); activated on an **increase**, with a
per-model baseline that does not fire on first observation and treats a decrease
as an EPP restart; and requiring rejections rising across **several consecutive
ticks**, so a burst of bad requests cannot wake anything.

## 10. Why this is worth building

The autoscaler already knows. It distinguishes "exported but zero" from "not
exported" on every tick and throws it away, and it resolves a scale-to-zero
policy it never reconciles against the replica bounds. Both failures above are
computable from what is already in hand.

The cost of not doing it is measured: a week of red e2e runs, three
investigations that blamed the wrong component, and a benchmark suite that
reported SUCCESS while testing nothing.
