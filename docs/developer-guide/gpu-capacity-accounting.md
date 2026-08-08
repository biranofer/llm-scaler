# GPU Capacity Accounting

How WVA decides whether GPUs are available, what that number actually means
today, and the ways it can still over-state free capacity.

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
Limit = sum of node Allocatable for that GPU type, across nodes  (node discovery)
Used  = GPUs WVA believes its own managed variants hold       (caller-supplied)
Available() = max(0, Limit - Used)
```

`Used` is not measured. It is passed in by the saturation engine, which sums
`CurrentReplicas × GPUsPerReplica` over the variants it manages
(`gpuUsageByType`), and is keyed by the variant's resolved accelerator type.

## Key reconciliation: `Used` vs `Limit` (fixed)

The two sides of a pool are written in different vocabularies, and getting this
wrong made the budgets inert rather than merely inaccurate.

- **Limits** are keyed by *normalized short names*: `TypeInventory.Refresh` runs
  each discovered node product label through `NormalizeAcceleratorName`, so
  `NVIDIA-A100-PCIE-80GB` becomes `A100`.
- **Usage** arrives keyed by whatever the workload declares. `gpuUsageByType`
  keys by `VariantMetadata.AcceleratorName`, i.e. the nodeSelector/label value
  verbatim — for the common deployment shape, the raw product label.

Unreconciled, a product-label usage key never matched a limit key,
`GetResourcePools` reported `Used = 0` for it, and every pool claimed its full
complement was free however much was running. Both consumers therefore saw an
empty cluster.

`TypeInventory.SetUsed` now reconciles incoming keys onto the discovered limit
keys, trying the declared name first and only then its normalization. Order
matters: `NormalizeAcceleratorName` falls back to "the segment after the first
hyphen" for names with no vendor prefix it recognises, so an already-short
`Gaudi-2` would become `2` and match nothing if normalized unconditionally. The
demand side does the same at lookup time (`pipeline.resolvePoolKey`), so both
spellings work on both sides.

Pinned by `TestUsageIsReconciledOntoPoolKeys` and `TestPoolKeysAreShortNames`.

> **This changed allocation behaviour.** Before it, `Used` was effectively always
> 0 in nodeSelector deployments, so the GPU-aware optimizer believed the cluster
> was empty and allocated against the full installed capacity. It now sees real
> usage and will allocate less. That is the correction, but it is a behavioural
> change for any existing deployment running `enableLimiter: true`.

## Gap 1: usage on an unresolved accelerator is still unattributable

A variant with neither a `nodeSelector`/`nodeAffinity` GPU key nor the
`inference.optimization/acceleratorName` label resolves to
`constants.DefaultAcceleratorName` — the internal `"unknown"` placeholder. That
name matches no discovered type, so reconciliation drops it: its GPUs are charged
to no pool.

The per-type budgets therefore still over-state free capacity by however many
GPUs unresolved variants hold. With 8 H100s, 2 held by a resolved variant and 4
by unresolved ones, the pool reports `Available() == 6` while only 2 are
genuinely free.

What changed is that the inconsistency is gone: dropped usage is excluded from
`TotalUsed` as well, so the aggregate and the sum of the pools now agree.
Previously `SetUsed` summed every key while the pools iterated only discovered
types, leaving the two views contradicting each other in opposite directions.

Pinned by `internal/engines/pipeline/unresolved_accelerator_usage_test.go`.

**Why it is not fixed further.** Usage that cannot be attributed to a type cannot
be charged to a pool, and charging it to every candidate type would deny
legitimate scale-up on a guess. The durable fix is making the accelerator
resolvable — set a `nodeSelector`/`nodeAffinity` GPU key on the workload, or the
`inference.optimization/acceleratorName` label. WVA emits an
`AcceleratorNotResolved` warning event per affected variant.

## Gap 2 (future work): `Limit` is installed GPUs, not available GPUs

`Limit` sums each node's **Allocatable** for the GPU resource and `Used` counts
only what WVA itself manages. Allocatable is the node's total for that resource;
it does not subtract what running pods have requested, and it does not go to zero
when a node is cordoned or `NotReady`. Everything else competing
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
4. Exclude unschedulable nodes (cordoned / `NotReady`), which Allocatable alone
   does not do, so drained capacity is not counted as available.
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

## Testing the placement check end-to-end

The scale-from-zero placement check has unit coverage on every seam, but its
**deny** branch is not yet exercised end-to-end. Three conditions must hold
simultaneously for a denial to be reachable at all:

1. the variant's accelerator resolves — a `nodeSelector` on a GPU product label,
   or the `inference.optimization/acceleratorName` label. Without it the
   candidate contributes no demand and `FitsGPUBudget` returns true having
   evaluated nothing;
2. a GPU-usage snapshot exists — which requires at least one **active**
   WVA-managed variant, since the saturation engine publishes only measured
   cycles; and
3. that usage is attributed to the same pool key the limits use (see the key
   reconciliation above).

Condition 2 interacts awkwardly with the trigger. Scale-from-zero fires on EPP
flow-control queueing, and requests only queue when the pool has **no ready
endpoints** — so the running variant needed for condition 2 must not sit in the
pool under test, or it serves the requests itself. The one-model-one-pool
contract makes that natural: the occupier serves its own model, so it belongs to
its own pool (`fixtures.WithPoolGuide`).

`test/e2e/scale_from_zero_capacity_test.go` implements the scenario but is
**pending** (`XDescribe`) — it does not yet reproduce. The occupier runs, holds
its GPUs, and is discovered with a resolved accelerator, but the engine reaches
no verdict for the parked variant at all, which points at the EPP not queueing
for that model rather than at the capacity check. Until it passes, treat the deny
branch as **unit-tested only**.
