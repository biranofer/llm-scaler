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
Used  = GPUs in use, on the basis this provider asked for      (caller-supplied)
Available() = max(0, Limit - Used)
```

## `Used` depends on the question the provider is answering

There are two measures of "GPUs in use", they are not interchangeable, and each
provider declares which one it needs via `pipeline.UsageBasis`. The caller
(`gpuUsageViews` in the saturation engine, `Engine.gpuUsageViews` in
scale-from-zero) hands each provider the matching view.

| basis | counts | produced by | consumed by |
|---|---|---|---|
| `PhysicalUsage` | every GPU held on the cluster's GPU nodes, whoever holds it | `internal/gpuusage`, on its own 15s ticker | physical inventory (`TypeInventory`) |
| `ManagedUsage` | only what WVA's own variants hold | the saturation engine's population sum (`gpuUsageByType`) | quota inventory (`QuotaInventory`) |

The split exists because the two answer different questions:

- a **physical inventory** asks *"will the scheduler find a free device?"*. Every
  GPU-requesting pod counts, whoever owns it — a training job in another
  namespace is as real an obstacle as one of WVA's own replicas. This view is
  attributed by the **node** a pod runs on, so nothing is unattributable and it
  does not depend on WVA having discovered the workload;
- a **quota** asks *"how much of the operator's declared allowance has WVA
  consumed?"*. A quota governs WVA-managed variants and nothing else. Charged the
  physical figure, a namespace with a 4-GPU WVA quota and an unrelated 4-GPU
  training job reads as fully spent while WVA has placed nothing, and every
  scale-up is refused. The managed view sums `CurrentReplicas × GPUsPerReplica`
  over the variants WVA manages, keyed by each variant's resolved accelerator.

Physical is the default: anything that does not declare a basis gets it. That is
the safe direction — a physical figure over-states consumption for a quota, which
errs toward refusing, rather than under-stating what the hardware holds, which
would place a variant onto a device that is already taken.

**A missing view is not zero.** Absent means "unknown", which both engines treat
as permissive; an empty map is a confident claim that nothing is in use, and a
provider handed one reports its entire capacity free. Callers check
`GPUUsageViews.MissingBasis` before computing any constraint, and fall back to
the unlimited optimizer (saturation) or an unchecked wake (scale-from-zero) when
a needed view has not been observed. Only the bases some provider actually asks
for are gathered, so a quota-only deployment is never held up waiting for a
physical observation it does not consult, and vice versa.

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

## Gap 2 (mostly closed): `Limit` is installed GPUs, not available GPUs

`Limit` sums each node's **Allocatable** for the GPU resource. Allocatable is the
node's total; it does not subtract what running pods have requested, and it does
not go to zero when a node is cordoned or `NotReady`.

The `Used` side of that gap is **closed for physical providers**.
`internal/gpuusage` observes the cluster directly — walking the pods that occupy
GPU nodes and attributing each to the node it is scheduled to — so a physical
pool now nets out:

- workloads in other namespaces, or not managed by WVA at all;
- system/DaemonSet pods holding GPUs;
- pods scheduled but not yet running (the scheduler has already committed those
  devices; treating them as free would let two wakes land on the same one).

Gap 1 does not apply to that view either: attribution is by node, so there is no
unresolved bucket to leak through. Budgets are correspondingly **tighter** than
they were before this landed, which is the correction — they were over-stated.

Two pieces remain open:

1. **Unschedulable nodes still contribute their GPUs to `Limit`.** Allocatable
   alone does not exclude cordoned or `NotReady` nodes, so drained capacity reads
   as available.
2. **Quota providers still use the population sum**, and deliberately so — see
   the usage-basis section above. Gap 1 therefore still applies to them: a variant
   with an unresolved accelerator is charged to no pool, so a quota reports more
   of its allowance free than it has. `wva_unattributed_gpus` reports the amount.

## Testing the placement check end-to-end

The scale-from-zero placement check has unit coverage on every seam, and its
**deny** branch is exercised end-to-end by
`test/e2e/scale_from_zero_capacity_test.go`. Three conditions must hold
simultaneously for a denial to be reachable at all:

1. the variant's accelerator resolves — a `nodeSelector` on a GPU product label,
   or the `inference.optimization/acceleratorName` label. Without it the
   candidate contributes no demand and `FitsGPUBudget` returns true having
   evaluated nothing;
2. a GPU-usage snapshot exists for the basis the configured provider needs. For
   the default physical provider this no longer requires an active WVA variant —
   `internal/gpuusage` observes on its own ticker from process start, which is
   what makes the check meaningful for a fleet parked at zero. A quota provider
   does still need a completed saturation cycle, which publishes the managed view
   (including as an explicit zero when nothing is active); and
3. that usage is attributed to the same pool key the limits use (see the key
   reconciliation above).

Condition 2 interacts awkwardly with the trigger. Scale-from-zero fires on EPP
flow-control queueing, and requests only queue when the pool has **no ready
endpoints** — so the running variant needed for condition 2 must not sit in the
pool under test, or it serves the requests itself. The one-model-one-pool
contract makes that natural: the occupier serves its own model, so it belongs to
its own pool (`fixtures.WithPoolGuide`).

The suite went green once `internal/gpuusage` became an independent producer.
Before that it could not pass: the parked fleet meant the saturation engine
returned without publishing, no snapshot existed when the wake was decided, and
"unknown" is permissive by design — so the wake the suite requires to be refused
was allowed, with nothing in the logs saying the check had been skipped.

## Assumption: one model, one InferencePool

The scale-from-zero wake path assumes a model's variants all sit behind a single
EPP. `buildCandidates` resolves one pool for the model group, and the group's
pending-request verdict is read from that pool's flow-control queue.

That matches the project's contract, and it is what keeps the wake cheap: one
scrape per model per tick, rather than one per variant.

**Per-role pools are a possible future shape and are not implemented.** If a
model's decode sat behind one EPP and its prefill behind another, the current
code would read both variants' demand from whichever pool resolved first — so a
decode variant could be judged by the prefill queue, and its activation logged
against an EPP that was never consulted. That is silently wrong rather than
imprecise, which is why the assumption is stated here and checked in code.

Supporting per-role pools would mean:

1. resolving the pool **per candidate** rather than per model group;
2. reading demand from each distinct pool in turn, attributing the verdict to the
   pool it came from; and
3. keeping selection across the whole model, so a P/D pair is still chosen as one
   set against one joint GPU budget even when its halves are behind different
   pools.

Until then, a model that resolves to more than one pool is logged
("Model resolves to more than one InferencePool"), and only the first is used.
