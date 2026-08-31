# What counts as a serving replica

WVA decides how much capacity a variant has by counting the Pods that reported
engine metrics. That count is used as though it meant "Pods that are taking
traffic right now". It does not, and the gap has produced three separate
symptoms that were each investigated as unrelated bugs.

This note states what the count means today, what it should mean, and what has
to change to close the difference. It covers ordinary replicas and warm pool
Pods together, because the same count is used for both and the answer differs
between them.

## What the count means today

A Pod contributes to a variant's measured supply when both of these hold:

1. a Prometheus query returned a series carrying its `pod`/`pod_name` label, and
2. `locator.Locate` resolved that Pod name to a managed scaler.

Neither condition says the Pod exists, is Ready, or is serving anything.
`internal/collector/replica_metrics.go` and `internal/collector/locator/` check
no Pod phase, no readiness condition and no `DeletionTimestamp`; and the
locator's pod→target resolution is deliberately cached "indefinitely", on the
sound reasoning that ownerReferences are immutable — which also means a Pod that
has been **deleted** still resolves for as long as the cache holds it.

So the count today includes:

- **Pods that are not yet Ready.** vLLM serves `/metrics` as soon as its HTTP
  server is up, which is before it passes readiness. The engine is scraping and
  the EPP is not yet routing to it.
- **Pods that are terminating.** A Pod with a `DeletionTimestamp` is still in the
  API and still scraped until it goes.
- **Pods that are already gone**, for as long as Prometheus retains their series
  (~5 minutes) — because the locator cache still resolves the name.

## The three symptoms

All measured on pokprod or in the e2e suite, 2026-08-30/31.

- **`ready 1, serving 4` with no bridge lent.** The scale target had one replica;
  three others had existed minutes earlier. The warm pool's return rule was
  reading a fleet four times the size of the one that existed.
- **`ready 3, serving 4` during a 1→4 scale-up.** Metrics led the Deployment's
  `Status.ReadyReplicas`. Benign in itself, but indistinguishable from the bug
  above without knowing which Pods were counted — which is why the e2e's
  `serving <= ready` assertion is only sound where the spec pins the fleet.
- **`scale_down_conceded_supply_test.go` fails about 1 run in 3**, and fails
  FAST (45 s against 370 s for a pass) with "WVA scaled below the target while an
  unowned replica was reporting: its capacity was counted as spare". A fast
  failure means the fleet was already below target when the spec began, which
  points at supply carried over from the previous run rather than at the
  behaviour under test.

The first and third are the same defect seen from two directions: capacity is
attributed to Pods that are not serving.

## What it should mean

The count has one consumer today — the warm pool's return rule, which asks: *have
the ordinary replicas arrived, so the Pod I lent can go home?* For that question
a Pod counts only if handing traffic back to it would actually be served. That is
three conditions, not one:

- it **exists and is not terminating** (`DeletionTimestamp == nil`),
- it is **Ready**, so the Service or EPP will route to it,
- it is **reporting**, so the engine has registered and is scraping.

Today only the third is checked. `ServingStore`'s own doc argues that reporting
is a better signal than the kubelet probe, and it is — but as an ADDITION to the
probe, not a replacement for it. A terminating Pod reports. A not-yet-Ready Pod
reports. A deleted Pod's series report for another five minutes.

### Warm pool Pods are a separate case

A pool Pod is not an ordinary replica and must not be counted as one. Two rules
already exist and should be stated together with the above:

- a Pod **lent** to a variant contributes that variant's DEMAND but never its
  SUPPLY (`publishServing` skips `FromWarmPool`; the analyzer splits
  `ownReplicas` from `warmPoolReplicas`);
- a pool Pod that is **awake but not lent** belongs to the pool's own scale
  target and must not reach any model's count at all.

Both hold today. What does not hold is the liveness and readiness of the Pods on
either side of that split.

## What has to change

1. **Filter the collector's rows by Pod state.** Drop rows whose Pod is
   terminating or absent. This is the cheapest fix and removes the "already
   gone" and "terminating" classes outright.
2. **Carry readiness onto the row**, so `publishServing` can require Ready AND
   reporting. `domain.ReplicaMetrics` has no readiness field today; adding one
   keeps the decision where the evidence is rather than making the warm pool
   re-derive it.
3. **Bound the locator cache by Pod existence.** Caching pod→target for ever is
   correct while the Pod lives and wrong the moment it does not. An entry
   invalidated on Pod deletion keeps the property the cache was built for and
   stops resurrecting dead Pods.
4. **Re-check the conceded-supply spec once 1–3 land.** If its intermittency is
   cross-run contamination through retained series, these remove the cause; if it
   still fails, the failure is real and belongs to the clamp.

Ordering matters: 1 and 3 are narrow and independently testable, 2 changes a
shared struct, and 4 is the measurement that says whether any of it worked.
