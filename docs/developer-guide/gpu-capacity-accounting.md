# GPU Capacity Accounting

How WVA decides whether GPUs are available, what that number actually means
today, and the two ways it currently over-states free capacity.

Two consumers read these budgets:

- the **GPU-aware optimizer** (`GreedyByScoreOptimizer`), active when the
  saturation config sets `enableLimiter: true`; and
- the **scale-from-zero placement check**, which refuses to wake a variant onto
  an accelerator with no room (see
  [`scaleFromZero`](saturation-scaling-config.md#scalefromzero)).

Both over-allocate when the budget over-states availability: pods land in
`Pending`, and for scale-from-zero the queued request that triggered the wake
times out anyway — a wake that looks like progress and delivers none.

## How a budget is computed

`DefaultLimiter.ComputeConstraints` builds a `ResourcePool` per accelerator type:

```
Limit = total GPUs of that type installed across all nodes   (node discovery)
Used  = GPUs WVA believes its own managed variants hold       (caller-supplied)
Available() = max(0, Limit - Used)
```

`Used` is not measured. It is passed in by the saturation engine, which sums
`CurrentReplicas × GPUsPerReplica` over the variants it manages
(`gpuUsageByType`), and is keyed by the variant's resolved accelerator type.

## Gap 1: usage on an unresolved accelerator is invisible

A variant with neither a `nodeSelector`/`nodeAffinity` GPU key nor the
`inference.optimization/acceleratorName` label resolves to
`constants.DefaultAcceleratorName` — the internal `"unknown"` placeholder — so
its GPUs are recorded under that key.

`TypeInventory.GetResourcePools` iterates the **discovered** types:

```go
for accType, limit := range i.limitByType {   // discovered types only
    used := i.usedByType[accType]             // "unknown" is never a key here
```

so the placeholder entry has nowhere to land and is silently dropped. With 8
H100s, 2 held by a resolved variant and 4 by unresolved ones, the pool reports
`Available() == 6` while only 2 are genuinely free.

There is a second, **latent** half: `SetUsed` sums *every* key into `totalUsed`,
so `TotalUsed` reports 6 while the pools account for 2. Nothing reads it —
`ResourceConstraints.TotalUsed`/`TotalAvail` are populated by `DefaultLimiter`
and never consumed, because the optimizer merges `Pools` and `NamespacePools`
only — but a future consumer would inherit the inconsistency. `QuotaInventory`
deliberately keeps its totals symmetric with its limits; `TypeInventory` has no
such guard, which is why this reads as an oversight rather than a decision.

Both behaviours are pinned by
`internal/engines/pipeline/unresolved_accelerator_usage_test.go`, which fails if
they change, so the semantics cannot drift without updating this document.

**Why it is not fixed in the accounting.** Usage that cannot be attributed to a
type cannot be charged to a pool, and charging it to every candidate type would
deny legitimate scale-up on a guess. The durable fix is making the accelerator
resolvable — set a `nodeSelector`/`nodeAffinity` GPU key on the workload, or the
`inference.optimization/acceleratorName` label. WVA emits an
`AcceleratorNotResolved` warning event per affected variant.

If the accounting is ever changed, the two candidates are:

| option | effect |
| --- | --- |
| exclude placeholder keys at the source (`gpuUsageByType` + `constants.IsAcceleratorResolved`) | makes pools and totals consistent; no behaviour change today, since pools already ignore the key. Totals become less accurate but no longer contradict the pools |
| charge unresolved usage conservatively (e.g. against the largest candidate pool) | closes the over-statement, at the cost of denying legitimate scale-up on a guess |

## Gap 2 (future work): `Limit` is installed GPUs, not available GPUs

`Limit` comes from node **capacity** discovery — every GPU installed on every
node — and `Used` counts only what WVA itself manages. Everything else competing
for those GPUs is invisible:

- workloads in other namespaces, or not managed by WVA at all;
- system/DaemonSet pods holding GPUs;
- variants whose accelerator is unresolved (Gap 1);
- GPUs on nodes that are cordoned, `NotReady`, or otherwise unschedulable.

So a cluster whose GPUs are fully consumed by non-WVA workloads still reports
its full complement as available, and WVA will happily scale into capacity that
does not exist.

**Most of the machinery exists but is not wired end-to-end.**
`limiter_factory.go` builds the inventory with `NewTypeInventoryWithUsage`, which
supplies a `FullDiscovery` whose `DiscoverUsage` lists pods and sums their actual
GPU requests. Reaching it requires `RefreshAll` (limits **and** usage), but
`DefaultLimiter.ComputeConstraints` calls `Refresh` — limits only — and then
overwrites usage with the caller's WVA-only figure via `SetUsed`.

Switching the call is not sufficient on its own. A fix must:

1. **Normalize the discovered usage keys.** `DiscoverUsage` keys its result by the
   raw node product label (`NVIDIA-A100-PCIE-80GB`), while `Refresh` normalizes
   *limit* keys to short names via `NormalizeAcceleratorName`. Calling
   `RefreshAll` today would file usage under keys `GetResourcePools` never reads —
   reproducing Gap 1's silent drop rather than fixing Gap 2.
2. Call `RefreshAll` (or `DiscoverUsage` directly) so `Used` reflects real
   cluster-wide GPU consumption.
3. Decide how discovered usage composes with WVA's own accounting — they overlap,
   since WVA's managed pods are also real pods, so they must not be summed. The
   discovered figure most likely **replaces** the supplied one, with WVA's
   accounting kept only for in-flight replicas not yet visible as pods.
4. Prefer node **allocatable** over capacity, and exclude unschedulable nodes, so
   drained capacity is not counted.
5. Keep the "unknown means do not block" posture — a discovery failure must not
   deny scale-up, matching the existing fallback when no provider can supply
   constraints.

Step 3 is the substantive one: naively switching to discovered usage without
reconciling the overlap would double-count WVA's own replicas and under-state
availability, which fails in the opposite direction.

Until then, treat the physical limiter as an upper bound on what WVA *itself*
has allocated, not as a statement about cluster-wide GPU availability. Operators
who need a hard ceiling should declare an explicit quota limiter (see
[`limiters`](saturation-scaling-config.md#limiters-cluster-default-only-live)),
which is enforced independently of physical discovery.
