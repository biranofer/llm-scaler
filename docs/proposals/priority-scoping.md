# Proposal: Scoping Priority to a Contention Domain

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-08-17
**Last Updated:** 2026-08-17

---

## Problem Statement

`priority` is a single float on a scaling-policy entry, and it is compared against
priorities written by other people for other workloads. Nothing says whose numbers
are comparable with whose, so the question "what does `priority: 5` mean?" has no
answer that survives a second tenant.

Two concrete consequences:

**A tenant can weight themselves.** Any namespace with a tracked workload may carry
its own `wva-scaling-policy-config` (`shouldWatchNamespaceLocalConfigMap`), and a
namespace-local map **replaces** the global one rather than merging with it
(`resolveScalingPolicyConfigMap`). A tenant therefore writes their own `default`
entry and their own `priority`. In the greedy pass that number is compared directly
against every other namespace's.

**There is no way to weight a namespace.** The knob that a platform owner actually
wants — "the research namespace gets a third of what production gets when they
contend" — does not exist at all. The only cluster-level lever is a quota, which is
a hard bound, not a share of contended capacity.

### What is already scoped, and what is not

This matters because it makes the change much smaller than "priority is global"
suggests. The two consumers of `priority` differ:

| consumer | contention domain today | cross-namespace? |
| --- | --- | --- |
| `computeRescaleTargets` (water-fill) | `(acceleratorType, namespace-quota \| cluster)` — `applyRescale` groups by exactly this | **No**, when the namespace has its own quota |
| `fairShareValue` (greedy ordering) | all models on the cluster, flat | **Yes**, always |

So the rescale pass has already answered the question — a namespace with a quota is
a closed domain, and priorities inside it only compete with each other. What it
lacks is any weight *between* domains: two namespaces drawing on the same cluster
pool have no declared relative standing.

The greedy pass has not answered it at all: `fsv = priority × Σ Score × demand`
orders every model on the cluster in one flat list.

## Goals

- Give `priority` a stated contention domain, so a number is only ever compared with
  numbers written by the same owner.
- Let a platform owner weight namespaces against each other.
- Make tenant self-declaration structurally pointless rather than policed.
- Change no behaviour for existing single-tenant configs unless opted in.

## Non-Goals

- Replacing quotas. Priority distributes contended capacity; a quota bounds it.
  Limiters remain constraints only.
- Preemption. Nothing here evicts running replicas; priority influences how
  headroom is allocated.
- A new CRD. This extends the existing ConfigMaps, as the ScalingPolicy work did.

## The four candidate scopes

The options as posed, evaluated against the structure above.

### 1. Priority per namespace, at cluster level — **adopt**

This is the missing knob, and its owner is unambiguous: the same party that already
writes cluster quotas. It belongs in the cluster policy ConfigMap (`wva-policy`
when separate), beside `limiters`, because that is already the boundary a tenant
cannot write across.

It is meaningful only when namespaces contend for a shared pool. Under
`WVA_SCOPE=namespace` there is one namespace and the weight is inert.

### 2. Priority per model, at namespace level — **adopt, as a relative weight**

Tenants know which of *their* models matters most; nobody else does. Keeping this
knob is right. The change is what the number means: **relative within the
namespace**, not absolute.

That single change closes the self-weighting hole without policing anything. If
model weights are normalised by their namespace's own sum, a tenant who writes
`priority: 1000` for every model gets exactly what they had at `1.0` — the ratios
are unchanged and the namespace's total standing is fixed by (1). Self-declaration
becomes arithmetic that cancels.

### 3. Priority per variant — **reject**

Variants of one model are *substitutes*: they serve the same demand, and choosing
between them is the optimizer's job, done on cost and capacity. A per-variant
priority would compete with that objective — an operator could weight the expensive
variant up and silently defeat cost-aware optimization, with no diagnostic
distinguishing "priority said so" from "the optimizer chose badly".

The per-variant knob already exists and is the right shape: `variantCost` in trigger
metadata, which the workload owner sets because they own the workload, and which
feeds ranking rather than entitlement.

### 4. Priority per named policy, at namespace level — **adopt as the mechanism, not a fourth scope**

This is not an alternative to (2); it is how (2) should be *delivered*. A tier
carries a weight, models select the tier by name, and retuning a class of workloads
is one edit rather than one per model. It resolves into the same per-model weight
and is normalised identically.

Tiers are already wired end to end (`scalingPolicy` trigger metadata → variant spec
→ `ResolveScalingPolicyForTier`), so this needs no new plumbing — only a statement
that a tier's `priority` is a namespace-relative weight like any other.

## Design

### Effective weight

For model `m` in namespace `n`:

```
                          p(m)
  W(m) = Wns(n) × ─────────────────────
                    Σ p(m') for m' ∈ n
```

- `Wns(n)` — namespace weight, cluster policy only, default `1.0`.
- `p(m)` — the model's resolved `priority` (default → tier → per-model override,
  unchanged), default `1.0`.
- The denominator sums over models **of that namespace present in the current
  contention group**, not every model that exists — a namespace does not lose
  standing because it has idle models elsewhere.

Properties worth stating because they are the point:

- Scaling every `p` in a namespace by any constant `k` leaves `W` unchanged.
- `Σ W(m)` over a namespace equals `Wns(n)`, so a namespace's total standing is
  exactly what the platform owner granted, whatever its tenant writes.
- With one namespace, or with all `Wns` equal and one model each, `W` reduces to
  today's ratios.

### Where each number lives

| number | file | namespace | writable by |
| --- | --- | --- | --- |
| `Wns(n)` | cluster policy ConfigMap | `wva-policy` (or the WVA system namespace) | platform owner |
| `p(m)` via tier or override | scaling-policy ConfigMap | global **or** namespace-local | platform owner or tenant |

`Wns` **must not** be read from a namespace-local map. That is the whole
enforcement point, and it is a single guard rather than a policy engine: the
existing `isPolicySource` branch in `ConfigMapReconciler.Reconcile` already
distinguishes the cluster-policy namespace from a tenant one.

Proposed surface, following the existing limiter entry shape:

```yaml
# cluster policy ConfigMap — same file as `limiters`
namespaceWeights:
  default: 1.0          # reserved key: any namespace not listed
  production: 5.0
  research: 1.0
  batch: 0.2
```

The reserved `default` key matches `QuotaLimiterReservedNamespaceKey`, so the two
cluster-level maps read the same way.

### Applying it

- **Rescale** — replace `w := m.Priority * m.Demand` with `W(m) × demand`. For a
  namespace-scoped group every model shares one `Wns`, so it cancels and behaviour
  is **unchanged**; the weight only bites in a cluster-scoped group holding models
  from several namespaces, which is precisely where it should.
- **Greedy** — `fairShareValue` takes `W(m)` in place of the raw priority. This is
  the pass that changes, and the one that was flat.

### Opt-in

Normalisation changes established behaviour: today a lone model at `priority: 10`
in one namespace outranks a lone model at `1.0` in another; under this design they
are equal unless `namespaceWeights` says otherwise. That is the intended semantics
but it must not arrive silently in an upgrade.

Gate on the presence of `namespaceWeights`. Absent, resolution is exactly as today
— a flat cluster-wide comparison — and the guard costs one map lookup. Present, the
cluster has opted into scoped priority as a whole, which is the right granularity:
a half-migrated cluster comparing normalised weights against raw ones would be
worse than either.

## Observability

Scoped priority is only trustworthy if an operator can see the arithmetic. The
effective weight is derived from three inputs across two ConfigMaps and one of them
may be absent, so "why did this model lose?" must be answerable without a rebuild.

- Report `Wns`, the raw `p`, the namespace sum and the resulting `W` on the existing
  effective-policy report (`policy_report.go`), which already logs the resolved band
  per model.
- A namespace named in `namespaceWeights` that matches no tracked namespace is
  almost always a typo and silently grants nothing. It should be reported the way an
  unknown policy tier already is.

## Migration

1. Land `namespaceWeights` parsing and `W(m)`, gated on presence. No config change,
   no behaviour change.
2. Document that a tier's or override's `priority` is namespace-relative once
   weights exist.
3. Platform owners add `namespaceWeights` when they want it.

No tenant config becomes invalid; a tenant's existing numbers keep their meaning
*relative to that tenant's other models*, which is the only meaning they ever
reliably had.

## Open Questions

- **Namespaces with no workload in a group.** The denominator sums over models in
  the group, so a namespace with nothing running contributes nothing and its weight
  is idle capacity for others. That is the desirable behaviour for autoscaling, but
  it means a namespace cannot reserve headroom by weight alone — reservation is a
  quota's job. Worth stating explicitly in the docs rather than leaving to inference.
- **Interaction with `enableRescale`'s scope coupling.** `RescaleFlags` is already
  resolved per namespace from that namespace's own config. If `namespaceWeights`
  exists, should a namespace still be able to disable rescale for its own quota
  group? Probably yes — it governs only its own closed domain — but it should be a
  decision, not an accident.
- **Should `Wns` accept `0`?** Zero would mean "never gets contended capacity",
  which is expressible but is really a quota of zero. Rejecting it keeps one
  concept in one place.
