# KEDA-driven discovery

Status: in progress
Branch: `feat/wva-external-scaler`

## What changes

WVA discovers the workloads it manages by listing the cluster and filtering on an
annotation. This inverts that: **the KEDA external-scaler call is the discovery
event.** WVA stops looking for variants and lets them announce themselves.

Today:

```
timer ──► List HPAs (cluster-wide)  ──┐
     ──► List ScaledObjects        ──┴─► filter llm-d.ai/managed
                                       ──► synthesize VariantAutoscaling from
                                           llm-d.ai/model-id, llm-d.ai/variant-cost
                                       ──► collector ──► optimizer ──► decision.Set

KEDA ──► GetMetrics ──────────────────► read decision store
```

After:

```
KEDA ──► IsActive / StreamIsActive / GetMetricSpec / GetMetrics
           │  ScaledObjectRef{namespace, name, scalerMetadata}
           ├─► registry.Observe(...)   ◄── THIS is discovery
           └─► read decision store     ◄── the answer

timer ──► registry.Snapshot() ──► collector ──► optimizer ──► decision.Set
```

Three consequences:

- **No listing for discovery.** A ScaledObject WVA has never been called about
  does not exist as far as WVA is concerned. RBAC narrows to `get` by name.
- **No WVA annotations.** Being called *is* being managed, so `llm-d.ai/managed`
  has nothing left to say. Identity comes from the trigger's `metadata`, which
  KEDA already delivers verbatim as `scalerMetadata` on every call.
- **The HPA path goes.** It has no equivalent of "KEDA calls us", so it cannot be
  driven this way. It returns later as an external-metrics API server (see
  [HPA-only clusters](#hpa-only-clusters)).

## What the call drives, and what it does not

The call drives **membership, not cadence.**

KEDA calls once per ScaledObject. The optimizer is inherently cross-variant — one
GPU budget, fair share across models, a limiter that has to see the whole fleet
at once — so answering each call by optimizing that one variant in isolation
would throw away the only view in which those decisions are meaningful. The
engines therefore keep their own cadence (saturation every `GLOBAL_OPT_INTERVAL`,
scale-from-zero every 100ms) and iterate the registry snapshot where they used to
iterate a List result.

So the call makes a variant *exist*; the timer decides what it *gets*.

## Identity: trigger metadata replaces annotations

KEDA passes the trigger's `metadata` map to every RPC as `scalerMetadata`. That is
the whole configuration surface.

| was | becomes | required |
|---|---|---|
| `llm-d.ai/managed: "true"` | — registration replaces it | — |
| `llm-d.ai/model-id` | `modelID` | yes |
| `llm-d.ai/variant-cost` | `variantCost` | no (defaults) |
| — | `variantName` | no (defaults to `scaleTargetRef.name`) |

```yaml
triggers:
- type: external-push
  name: wva-external-scaler
  metadata:
    scalerAddress: wva-external-scaler.<ns>.svc.cluster.local:9090
    modelID: default/default
    variantCost: "10.0"
```

Reserved for later increments, parsed but not yet consumed: `inferencePool`,
`engine`, `scalingPolicy`, `wvaOwnership` (see the external-scaler proposal §7).

`minReplicaCount` / `maxReplicaCount` / `advanced.horizontalPodAutoscalerConfig`
are NOT metadata — they stay on the ScaledObject, where KEDA needs them anyway,
and WVA reads them with a targeted `Get` of the object it was just called about.

## Liveness

An entry is live while KEDA is still interested in it. Two independent signals,
because at zero replicas only one of them exists:

- **A recent call.** Any RPC refreshes `LastSeen`. A `type: external` trigger
  polls `IsActive` every `pollingInterval`, so this alone keeps a parked workload
  live.
- **An open stream.** A `type: external-push` trigger calls `StreamIsActive` once
  and holds it. At zero replicas nothing else is called at all — the HPA does not
  query metrics for a workload it is not scaling — so the open stream must count
  as liveness on its own, for as long as it is open. It is released when the
  stream closes, after which the TTL takes over.

TTL defaults to 5 minutes: comfortably above KEDA's 30s default
`pollingInterval` and the 15s optimize interval, so a slow poll never evicts a
live workload, while an abandoned entry does not linger for the process lifetime.

## Why not watch ScaledObjects?

The obvious alternative is to subscribe: watch ScaledObjects and update the
registry the moment one changes. It is rejected, because **a watch is a list.**
A controller-runtime informer does an initial cluster-wide LIST and then holds a
WATCH, keeping every ScaledObject in the cluster in memory — including the ones
WVA does not manage — and needs `list;watch` cluster-wide in RBAC. That is
precisely the thing this design removes. (Note this is also why WVA's per-entry
read uses `GetAPIReader`: a *cached* Get is served by that same informer, so
reading through `GetClient()` would reinstate the watch without anyone writing
the word "watch".)

The gap a watch would close is narrower than it looks, because **KEDA already
notifies us.** When a ScaledObject's generation changes, KEDA rebuilds its scaler
cache: it re-issues `GetMetricSpec` and closes and re-opens `StreamIsActive`. So
a change to the trigger — which is where WVA's own configuration lives — is
pushed to WVA for free, on the object's own edit.

What KEDA does not forward is the rest of the object: `scaleTargetRef`,
`minReplicaCount`, `maxReplicaCount`. So:

- A call carrying metadata that differs from what is stored is treated as
  evidence the OBJECT changed, and **invalidates the entry's target read** — the
  next enrichment pass re-reads immediately rather than serving a stale envelope
  for the rest of its window. The last known target keeps serving until it lands,
  so an edit never drops a variant out of the fleet.
- An edit that touches *only* min/max and leaves the trigger alone still waits
  out the freshness window (≤30s). That is the accepted cost. These are envelope
  bounds that change when someone edits a manifest, and KEDA's HPA — not WVA —
  is what enforces them in the meantime.

If that window ever proves too slow, the cheaper fix than a watch is to shorten
`DefaultTargetMaxAge`, or to move the affected field into trigger metadata where
KEDA will push it.

### Restart

The registry is in-memory and starts empty. After a WVA restart, KEDA re-opens its
streams and resumes polling, so the registry refills within one `pollingInterval`.
Until it does, the engines see no variants and publish no decisions — which is
exactly the state WVA is already in before its first optimization cycle:
`GetMetrics` returns 0 and HPA holds the target at `minReplicaCount`. No new
failure mode, and no persistence needed.

## What still lists

"No listing" is scoped to variant discovery. These remain, and are not discovery:

- **Node inventory** (`internal/discovery`) — cluster GPU capacity. Nothing in the
  KEDA call describes the cluster.
- **Pod lookup in the locator** — pod→variant attribution for metrics. vLLM
  metrics carry no variant label, so the owner-walk from pod is the only linkage
  (see the metric-label surface notes).
- **Namespace list for ConfigMap bootstrap** — configuration, not variants.

## HPA-only clusters

Killed now, restored later as a separate path: WVA serves
`external.metrics.k8s.io` so an HPA's `External` metric source reads
`wva-desired-replicas` the same way the KEDA trigger does. The registry then has a
second feeder — the metrics API request — with the same shape as the gRPC call:
the request names an object, and naming it registers it. That symmetry is the
reason to build the registry as a feeder-agnostic component now rather than
folding it into the gRPC handler.

Not in this plan. Next step after it.

## Increments

1. **Registry, fed by the scaler.** New `internal/registry`. Every RPC upserts;
   `StreamIsActive` holds. Additive — nothing reads it yet, so it can land and be
   observed before anything depends on it.
2. **Engines read the registry.** `utils.{Active,Inactive}VariantAutoscaling`
   build from a snapshot plus one targeted `Get` per entry, instead of listing
   ScaledObjects. The synthesized `VariantAutoscaling` stays as the internal
   carrier; only where it comes from changes.
3. **Remove the HPA path.** `HPAReconciler`, the HPA indexer,
   `VariantAutoscalingFromHPA`, the coordinator's HPA discovery, the RBAC.
4. **Retire the annotations.** Metadata becomes the only identity source; delete
   `llm-d.ai/managed`, `model-id`, `variant-cost`, the ScaledObject reconciler and
   the annotation predicate. Samples, fixtures and e2e follow.

Each increment keeps the unit gate and the kind e2e green on its own.
