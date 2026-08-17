# Say when a model will not park, and when it cannot wake

**Status:** §4–§8 implemented, including §7 once WVA emitted the join key it needed;
§9 to file upstream; §10 deferred. Exercised on kind: e2e smoke 21/21 and the
scale-from-zero specs 6/6, all nine alert rules loading `health=ok`, and every
dashboard panel query valid against a live Prometheus.

That cluster run found a defect nothing offline could. WVA series reach Prometheus
with the model's namespace as **`exported_namespace`** — the ServiceMonitor job
attaches its own `namespace`, which is the CONTROLLER's, and Prometheus renames
the scraped one. Every alert here grouped by and displayed `namespace`, so an
operator paged about a model in `llm-d-sim` would have been sent to
`workload-variant-autoscaler-system`. The dashboards already knew this and used a
`$namespace_label` variable; the alerts did not. The offline check in
`test/alerts` cannot catch it — it validates metric NAMES, not label semantics.

## 1. Two silent failures

Both end with a model in the wrong state and nothing anywhere saying why.

**It will never park.** Scale-to-zero is enabled, but one variant has
`minReplicas > 0`, so something always serves and the model never reaches zero.
The operator believes idle models cost nothing. They do not. There is no symptom
to notice: the model is up and serving, and every metric says it should be.

**It cannot wake.** Every variant permits zero and the model parks, but the wake
signal never arrives, so it stays parked. The autoscaler looks healthy — running,
reconciling, emitting metrics. The model is simply gone.

WVA reports neither. `applyScaleToZeroEnforcement` already refuses to park a model
for **three** distinct reasons — a non-vLLM engine, a variant floor, and the
activation-retention hold — and every one of them exits through a
`logger.V(DEBUG)` line. At the default verbosity all three are invisible.

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

## 3. Scope, and why not a status condition

WVA does not deploy EPP, so the root cause is not ours to fix (§9). What is ours
is to **notice, and say so**.

The idiomatic Kubernetes answer to *"I cannot reach the desired state, and here is
why"* is a `metav1.Condition` on the reconciled object — machine-readable, visible
in `kubectl describe`, usable with `kubectl wait`. It is not available here, and
the reason is structural rather than an oversight:

**WVA owns no API object.** It synthesizes `VariantAutoscaling` values from
ScaledObjects, the VA CRD is being retired, and a ScaledObject's status belongs to
KEDA — writing conditions into another controller's status is a fight over
ownership, not a design. So the reporting surfaces available are metric, Event,
and log, and everything below is shaped by that constraint.

**Absence is not zero.** An idle queue reports `0`; a queue that does not exist
reports nothing. Every wrong conclusion reached while diagnosing this came from
conflating them. `pendingRequestsForModel` already distinguishes the two on every
tick and discards it — most of what follows is surfacing what is already computed.

## 4. One metric, many reasons

The repo already has `wva_variant_at_max_replicas`: a bespoke gauge for one
specific "why isn't this where it wants to be" condition. A second such gauge for
flow control, and a third for the variant floor, is the start of a pattern that
ends with a metric per discovery — each with its own name, its own alert, and its
own four-file checklist. `applyScaleToZeroEnforcement` alone would justify three.

Instead, one series with a **reason label** — the conditions pattern expressed in
Prometheus, and the shape `wva_decisions_limited_total{limited_by}` already uses:

```
wva_model_scaling_blocked{namespace, model, reason} 1
```

| `reason` | meaning |
|---|---|
| `variant-floor` | a variant has `minReplicas > 0`, so the model cannot reach zero |
| `policy-forbids-zero` | every variant permits zero, but the model's policy disables scale-to-zero |
| `engine-unsupported` | a non-vLLM engine, whose request counter the enforcer cannot read |
| `no-wake-signal` | EPP is not exporting a flow-control queue, so nothing can wake this model |

One metric, one place to look, and a closed enum so cardinality stays bounded.
Several reasons can hold at once, which emits several series — correct, and more
informative than a single winner. A new condition later costs a constant, not a
metric.

Presence is the signal: a series exists only while its reason holds. That makes
clearing them part of the contract, and implementation turned up two ways to get
it wrong that are worth stating as part of the design.

**Reasons have owners.** Two engines write this metric — the steady-state
enforcer decides the policy reasons, the scale-from-zero loop decides the wake
reason — and they run at different rates. A "delete every series for this model,
then set mine" implementation has them taking turns erasing each other's answers,
at 10Hz, producing a metric that flaps for reasons having nothing to do with the
cluster. So each producer declares the reasons it owns and clears only those, and
setting a reason it does not own is a no-op rather than an orphan nothing can
ever clear.

**A departed model has no producer left.** Its last reason keeps asserting that a
workload which no longer exists will never park, and alerts forever. Nothing else
notices, because the code that would clear it never runs for that model again. So
the engine tracks which models it has published for and prunes them against the
active set — the same shape `pruneAnalyzerSeries` already uses, and for the same
underlying reason: a Prometheus GaugeVec cannot enumerate its own children. The
empty-fleet cycle evicts everything, on the path that has already proved the
model list succeeded rather than on a transient one.

## 5. First: say when a model will never park

Parking needs **both** halves to permit it:

- `config.ResolveScaleToZeroEnabled(policy)` is true. Scale-to-zero is a
  **model/tier** property, resolved from the scaling entry with a
  `WVA_SCALE_TO_ZERO` fallback.
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

Neither is an error and neither should be rejected. Alongside the metric, one
`Normal`-severity Event and one `logger.Info` on transition, because nothing is
broken:

```
Model %q will not scale to zero: %s.
  reason "scale-to-zero is disabled for this model, though every variant permits it"
  reason "variant %q has minReplicas=%d, so the model cannot reach zero even
          though scale-to-zero is enabled"
```

**Where it is evaluated.** Not in `internal/config`: variant floors come from
`ScaledObject.spec.minReplicaCount` through the registry, so they are cluster
state and `internal/config` never sees them. The one place both halves are already
in hand is `applyScaleToZeroEnforcement` — which resolves the policy, calls
`hasMinReplicasAboveZero`, and is deliberately the single funnel every optimize
path goes through, with a test locking that down. The reason strings are computed
by a pure function over `(scaleToZeroEnabled, []VariantReplicaState)`; only the
emission lives in the engine.

**Why this is first.** It depends on nothing outside this repo: no EPP, no flow
control, no wake path, no metric join, and no way for it to cause a spurious wake.
And it catches a failure **nothing else can see** — a model that will never park
on a cluster where EPP is perfectly healthy has no symptom to alert on. It is up
and serving, exactly as every metric says it should be.

## 6. Second: say when the wake signal is absent

`reason="no-wake-signal"`, keyed by **model**. The contract is one model to one
InferencePool, so pool-keying saves nothing today, and the documented direction is
*per-role pools for a single model* — one model to many pools — under which
pool-keying would emit several series for one fact. Model is also the key every
other WVA metric uses, and the one the reader needs: the question is "can this
model wake?". If a model ever spans pools, add `pool` as a label rather than
promoting it to the identity.

**Reported for serving models too, not only parked ones.** The first version
emitted this from the wake path, which only ever considers models already at zero
— so a model learned it could not be woken at the moment it needed waking. That
is reporting after the trap has closed, and the trap is worth naming: a model
behind a flow-control-less EPP is parked by the enforcer on the **vLLM request
counter**, which has nothing to do with flow control. WVA will strand it, then
discover it cannot get it back.

So the check runs on its own 30-second clock over every model WVA knows about,
parked or serving. Family presence is a property of the EPP, not of a model, and
it changes when EPP restarts — not ten times a second. It also runs **after** the
wake loop, so every pool with something parked has just been scraped and its
verdict is already cached; the sweep pays only for pools nothing is parked on.
Running it first cost a second scrape per model per tick, which is the exact cost
the per-model grouping was introduced to remove.

Keeping it off the per-model path matters for a second reason. That function
returns early on half a dozen bootstrap conditions — no pool in the datastore, no
resolvable candidates, no EPP metrics source, a scrape failure with no fallback —
and an emission below those returns silently stops updating, leaving whatever it
last said standing. A sweep on its own clock cannot be skipped by any of them.

**A failure to look is not an observation of absence.** An unreadable pool, an
unresolvable one, a failed scrape: all report nothing. A false `no-wake-signal`
sends an operator to restart a healthy EPP.

**The cheaper alternative, weighed.** "Does this metric family exist?" is natively
answerable by Prometheus as `absent(llm_d_epp_flow_control_queue_size)`, as a
recording rule, with no WVA code at all — and the e2e capability gate already uses
exactly that query. It is rejected here for one reason: **WVA knows the
model → pool → EPP mapping and Prometheus does not.** A recording rule would need
the same fragile `label_replace` join that §7 pays for, and would answer "some EPP
somewhere is not queueing" rather than "this model cannot wake". WVA resolves the
mapping internally, where it is authoritative.

What this buys is naming the cause quickly. It is a diagnosis accelerator, not a
detector — §7 is what tells you something is wrong.

## 7. Third: alert on the symptom, not the cause

§6 detects one cause. The failure an operator cares about is "my model is parked
and will not wake", which has many: stale EPP, missing gate, RBAC denial, KEDA
stuck, an engine bug. A cause-specific detector only catches the cause someone
already thought of — and the one that bit us was invisible for a week precisely
because nobody had.

Both halves look like they already exist as metrics:

```
wva_current_replicas == 0
rate(llm_d_epp_request_error_total{error_code="ServiceUnavailable"}[5m]) > 0
```

A model at zero while requests are being refused is broken **whatever the
reason**, and this appeared to need no engine code at all.

**As written, it had no join key.** `wva_current_replicas` carries `variant_name`,
`namespace` and `accelerator_type` — and **no `model_name`**, because it is a
per-variant series. EPP's counter, from a real scrape, is
`llm_d_epp_request_error_total{error_code, fairness_id, model_name, priority,
target_model_name}` — `model_name` and **no namespace**. `label_replace` can
rewrite a label; it cannot derive a model from a variant name, and nothing in
Prometheus knows that mapping. An earlier draft called this a "friction" to be
paid with `label_replace`. That was wrong: not expensive, impossible.

**So WVA emits the key.** `wva_model_replicas{namespace, model_name}` — replicas
serving a model, summed across its variants. WVA is the one component that knows
which variants serve which model, which is exactly why the join is unwritable
without it. It is a value rather than a condition, so it does not reopen the §4
sprawl argument, and its labels are deliberately minimal: adding `variant_name` or
`accelerator_type` would put the series back on the per-variant side of the join
it exists to bridge.

```
(sum by (namespace, model_name) (wva_model_replicas) == 0)
  and on (model_name)
(sum by (model_name) (rate(llm_d_epp_request_error_total{error_code="ServiceUnavailable"}[5m])) > 0)
```

Two properties worth stating plainly. **Zero must be published**, not omitted — a
model at zero is the entire subject of the alert, so a caller that skips the zero
case disables it. But **unknown is not zero**: a model whose variants have not
been discovered yet publishes nothing, because emitting 0 would claim it is
parked. And a departed model's series is **deleted** rather than set to 0, which
would keep the alert live for a workload that no longer exists.

The join is on `model_name` alone, since EPP carries no namespace — so two
namespaces serving the same modelID cannot be told apart in the join, and the
namespace shown comes from WVA's side. That is a real limitation of EPP's label
surface, not something PromQL can fix.

The rejected alternative was to let WVA emit the symptom as another reason: the
scale-from-zero loop already scrapes EPP and could read the error counter on the
same scrape, skipping the join. But that is §10's signal wearing a different hat,
and inherits every ambiguity §10 defers it for — a mistyped model name refuses
requests exactly like a parked model does. Behind an alert a human reads, that
ambiguity is tolerable; behind a wake decision that spends GPUs, it is not.

The second cost was real and is now paid: the alerts spec rejects any metric it
does not know, so `llm_d_epp_request_error_total` needed an explicit external
allowance — kept as a short named list rather than a blanket exemption, because
if EPP renames it our alert silently matches nothing and fires never.

## 8. Reporting surfaces and their conventions

**Metric** — the only surface that survives a restart, and the one alerts read.
Adding one touches **four** files and missing the last breaks CI:
`internal/constants/metrics.go`, `internal/metrics/metrics.go`,
`config/components/prometheus-alerts/prometheusrule.yaml`, and `wvaMetricNames`
in `test/e2e/prometheus_alerts_test.go`. §4 is partly an argument for paying this
once rather than per condition.

**Event on the ScaledObject** — for `kubectl describe`, where an operator
debugging a stuck workload looks. Emitting on another controller's object is
conventional (KEDA does it too) and is not the ownership problem that writing its
`status` would be. Use `variant.EventTarget()`: events on the synthesized
`VariantAutoscaling` are silently dropped with `no kind is registered for the type
variant.VariantAutoscaling in scheme`, a live bug seen in pokprod logs today — a
wake succeeded and left no Event behind.

**Not used for these conditions**, though, and the reason is worth recording:
Events attach to an object, and every condition here is a property of a MODEL. A
model has N variants and therefore N ScaledObjects, so "this model will never
park" either duplicates onto all of them or picks one arbitrarily. Neither is
right. The Event surface fits variant-scoped facts; the metric and the log carry
the model-scoped ones. A variant-scoped reason added later can and should use it.

**Log** — `logger.Info` at V(0), on transition only. Not "WARN": logr has `Info`
and `Error` and no Warn, and `internal/logging` defines only `DEBUG = 4` and
`TRACE = 5`. The transition state rides on the prune bookkeeping above rather than
living in a map of its own, so it is evicted by the same code that clears the
series — a separate map would need its own eviction on a process that runs for
weeks. A model seen for the first time with nothing blocking it logs nothing, or
every restart would emit a line per healthy model to report that all is well.

## 9. The root cause, which is not ours

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

## 10. Deferred: a fallback wake signal

`llm_d_epp_request_error_total{error_code="ServiceUnavailable"}` is the one
signal present even when flow control is not: it moved +40 on kind while no queue
series existed. A fallback would have kept scale-from-zero working through those
two days.

Deferred, not dismissed — and §9 is why it cannot simply be dismissed: the root
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

## 11. Sequencing, and what shipped

§5 and §6 share the §4 metric but nothing else, and landed in that order — §5
first, since it has no external dependency at all.

Shipped: the metric with four reasons; the transition log; two alerts
(`WVAModelWillNotScaleToZero` at info, `WVAModelHasNoWakeSignal` at warning); two
operational-dashboard panels.

**There is deliberately no alert on `engine-unsupported`.** It is a standing
property of running a non-vLLM engine, not an event: it would fire the day the
model is deployed and never clear. An alert that is always firing is one
operators learn to route to /dev/null, and it would take the two alerts above
with it. The reason stays on the metric and on the dashboard, which is where a
permanent condition belongs.

Not shipped, with reasons above: the Event surface (§8, model-scoped conditions do
not fit object-scoped events) and the fallback wake signal (§10, deferred on
purpose).

**Alert validation moved out of `test/e2e`.** The "alerts reference only known
metrics" check lived there, and `make test` runs
`go list ./... | grep -v /e2e` — so it ran only against a live cluster, which is
how `wva_node_access_denied` shipped broken and stayed hidden behind a runtime
skip. `test/alerts` now validates the rule as WRITTEN at unit-test speed, and the
e2e spec still validates it as DEPLOYED, both through the same helpers.

Two hand-maintained lists went with it, because both had already drifted:

- the known-metric list is now derived from the constants that define the metrics,
  so adding a metric does not require a second edit elsewhere to use it in an
  alert;
- the expected-alert-names list is now read from the manifest we ship. It asserted
  five names against a manifest that had shipped six since the commit adding both,
  so it could never have passed — unseen because that spec is labelled `smoke`,
  which `test-e2e-full` excludes.

The extractor itself was also wrong: it treated the contents of a label matcher as
metric names, so `reason=~"variant-floor|..."` yielded `variant`, `policy`,
`forbids` and `zero` as four unknown metrics and failed a correct alert. No
shipped alert had used a string-valued matcher before, so the bug was latent —
and §7 would have hit it too.

## 12. Why this is worth building

The autoscaler already knows. It distinguishes "exported but zero" from "not
exported" on every tick and throws it away; it refuses to park a model for three
distinct reasons and logs each at a verbosity nobody runs. Every failure above is
computable from what is already in hand.

The cost of not doing it is measured: a week of red e2e runs, three
investigations that blamed the wrong component, and a benchmark suite that
reported SUCCESS while testing nothing.
