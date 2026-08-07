# Design & Plan: Analyzer Architecture Refactor — Metadata Discovery + a Pure `(D, P)` Contract

**Status:** Draft (for review)
**Area:** engines/analyzers
**Related:** `docs/proposals/analyzer-metric-interface.md` (the `(demand, target)` contract & external analyzers), issue #1455 (external analyzers), #1444 (proposal, docs-only)

---

## 1. Motivation

WVA's analyzers carry three coupled problems:

1. **The saturation analyzer is privileged as the metadata keeper.** `saturationEntry`
   (`internal/engines/pipeline/analyzer_helpers.go:87`) is the sole source of per-variant identity
   (`Cost`, `AcceleratorName`, `Role`, replica counts) the optimizer reads. Every other analyzer must
   either piggyback on it or be special-cased. The de-privilege TODO is already written in the code
   (`analyzer_helpers.go:90`: *"remove the sat_v2 special role once all analyzers populate variant
   metadata"*), and `throughput` demonstrates the wart — it leaves `Cost`/`AcceleratorName` empty and
   relies on saturation (`internal/engines/analyzers/throughput/analyzer.go:372-382`).

2. **The analyzer contract is bespoke and wide.** `Analyze` returns `*AnalyzerResult` with ~8
   engine-owned or observability fields (`RequiredCapacity`, `SpareCapacity`,
   `TotalAnticipatedSupply`, `TotalSupply`, `Utilization`, `TotalCapacity`, …) that analyzers must
   leave zero or populate by convention. The two numbers that actually matter — demand `D`
   (`AnalyzerResult.TotalDemand`) and per-replica target `P` (`VariantCapacity.PerReplicaCapacity`) —
   are split across two structs and muddied by everything else.

3. **Extending WVA requires writing Go.** A new signal (an SLO probe, a queue-depth source, a
   business metric) cannot be added without compiling a new analyzer. #1455 wants config-driven
   PromQL analyzers, but they can only be *clean* if there is a clean contract to implement.

### Key finding that shapes this design

The saturation analyzer **does not compute** the identity metadata — it **copies** it. `AcceleratorName`,
`Cost`, `Role`, and replica counts arrive **pre-computed on `domain.AnalyzerInput`** (via
`VariantStates` and `ReplicaMetrics`) and are copied onto each `VariantCapacity`
(`saturation_v2/analyzer.go:352-360, 441-453`). The authoritative sources already exist:

| Field | Source | Site |
|---|---|---|
| `VariantName`, `ModelID`, `Cost`, `MinReplicas`, `MaxReplicas` | managed `ScaledObject`/`HPA` annotations (`llm-d.ai/managed`, `llm-d.ai/model-id`, `llm-d.ai/variant-cost`) | `annotationSourcedVariants` `utils/variant.go:198`; `annotations.VariantAutoscalingFromScaledObject`/`FromHPA` `variant_fromannotations.go:40,89` |
| `Role` | leader pod-template label `llm-d.ai/role` | `getRoleFromScaleTarget` `engine.go:1153` |
| `AcceleratorName` | pod-template `nodeSelector`/`nodeAffinity` GPU product keys | `accelerator.GetAcceleratorNameFromScaleTarget` `accelerator.go:90` |
| `CurrentReplicas`, `ReadyReplicas`, `PendingReplicas`, `GPUsPerReplica` | scale-target status/spec | `BuildVariantStates` `engine.go:1063-1151` via `ScaleTargetAccessor` |

**So this refactor is boundary-drawing, not greenfield.** We consolidate the sources above into one
authoritative record, hand it to analyzers *and* the optimizer, and stop laundering identity through
the analyzer's output. This substantially de-risks it.

---

## 2. Goals / Non-Goals

**Goals**

- Introduce a **variant-metadata discovery step** that produces one authoritative record per managed
  variant, sourced from the existing machinery, consumed by both analyzers and the optimizer.
- Reduce the analyzer contract to a **pure `(D, P)` producer**: demand `D` (model- and role-scoped)
  and per-replica target `P` (per variant/role). No identity, no engine-owned fields.
- **De-privilege saturation** — delete `saturationEntry`; the optimizer reads identity from discovery.
- Emit `wva_analyzer_demand` / `wva_analyzer_target` for every analyzer (the proposal's Phase 1
  observability), driven from the `(D, P)` outputs.
- Land the **external-analyzer wrapper (#1455)** as a Go analyzer that implements the pure contract
  from a PromQL definition, with **no metadata to supply**.

**Non-Goals**

- **V1 (`internal/saturationv1/`) and the queueing-model analyzer are out of scope** — both are being
  removed on a separate track. This design targets the **post-removal** engine
  (`saturation_v2` + `throughput` + the external wrapper). Their removal is listed as a precondition
  (§6) only to flag the one shared dependency (`DefaultVariantCost`) that must be relocated.
- No change to the **optimizer's coordination math** (sum-over-variants / min-over-roles in
  utilization space). We change *where identity comes from* and *the shape analyzers emit*, not how
  replicas are chosen. `applyUniversalThreshold` + `roleBottleneckReplicas` stay the `⌈D/P⌉` engine.
- No hard KEDA dependency. WVA stays KEDA-*shaped*.

---

## 3. Target architecture

```
┌─────────────────────┐     VariantMetadata[]      ┌──────────────────────────────┐
│  Discovery step      │ ─────────────────────────▶ │  Optimizer                    │
│  (annotationSourced  │   (identity: Cost, Accel,  │  reads identity from discovery │
│   + BuildVariant     │    Role, replicas, GPUs)   │  (no saturationEntry)          │
│   States + role/acc) │ ──┐                        └──────────────▲───────────────┘
└─────────────────────┘   │                                        │ (D, P) per variant/role
                          │ VariantMetadata[]                      │
                          ▼                                        │
                 ┌──────────────────┐   AnalyzerResult{D, P}  ┌────┴─────────────┐
                 │ Analyzers        │ ───────────────────────▶ │ wva_analyzer_*   │
                 │ (pure D/P):      │                          │ metrics          │
                 │  saturation_v2   │                          └──────────────────┘
                 │  throughput      │
                 │  external wrapper│
                 └──────────────────┘
```

### 3.1 The discovery output — `VariantMetadata`

One record per managed variant, produced once per cycle. It is essentially today's
`domain.VariantReplicaState` **plus** `Cost` and `AcceleratorName` (currently smuggled through
`domain.ReplicaMetrics`). Sketch (final field set settled in Phase 1):

```go
// VariantMetadata is the authoritative per-variant identity + state for one cycle.
// Produced by the discovery step; consumed by analyzers (as input) and the optimizer.
type VariantMetadata struct {
    VariantName     string
    ModelID         string
    Namespace       string
    Role            string  // "prefill" | "decode" | "both" (llm-d.ai/role)
    Cost            float64 // llm-d.ai/variant-cost
    AcceleratorName string  // nodeSelector/affinity GPU product
    GPUsPerReplica  int
    CurrentReplicas int
    ReadyReplicas   int
    PendingReplicas int
    MinReplicas     int
    MaxReplicas     int
}
```

The discovery step is a consolidation of `annotationSourcedVariants` (identity/cost/model/min-max) +
`BuildVariantStates` (role/accelerator/replicas/GPUs). It does **not** add new parsing — it moves the
existing calls behind one boundary and returns `[]VariantMetadata` keyed by `VariantName`.
`EngineParams` (vLLM/SGLang arg parsing for the k2 capacity *estimate*) stays with `saturation_v2` —
it is a capacity input, not identity.

### 3.2 The pure analyzer contract

Analyzers emit **only** demand and per-replica target. Sketch:

```go
type Analyzer interface {
    Name() string
    Analyze(ctx context.Context, input AnalyzerInput) (*AnalyzerResult, error)
}

// AnalyzerResult is a pure (D, P) result. No identity, no engine-owned fields.
type AnalyzerResult struct {
    TotalDemand float64            // D, model scope
    RoleDemand  map[string]float64 // D per role (prefill/decode); nil ⇒ non-disaggregated
    Targets     []VariantTarget    // P per (variant, role)
    Reason      string             // provenance/health hint (observability only)
}

type VariantTarget struct {
    VariantName        string
    Role               string
    PerReplicaCapacity float64 // P
}
```

- **Identity leaves the result.** `Cost`, `AcceleratorName`, `ReplicaCount`, `PendingReplicas`,
  `TotalCapacity`, `Utilization` are removed from the analyzer output — the optimizer gets them from
  discovery, keyed by `VariantName`.
- **Engine-owned fields leave the result.** `RequiredCapacity`, `SpareCapacity`,
  `TotalAnticipatedSupply`, `TotalSupply` move to the engine's post-step
  (`NamedAnalyzerResult`, `optimizer_interfaces.go`) — they are computed by
  `applyUniversalThreshold` from `D` and supply (`Σ replicas·P`, replicas from discovery), never by
  the analyzer.
- **Roles unify.** `RoleDemand` replaces the parallel `RoleCapacities map[string]RoleCapacity`;
  non-disaggregated is `RoleDemand == nil` (one synthetic `both`), exactly the shape `initRoleState`
  (`analyzer_helpers.go:127`) already normalizes to.

### 3.3 Optimizer reads discovery, not saturation

The optimizer keys its per-model pass on the **discovery set** (authoritative variant list + Cost +
Accelerator + Role + replica state) and asks each analyzer only *"what is `P` and demand for variant
`v` / role `r`?"*. Concretely, the reads currently satisfied by `saturationEntry` switch to discovery:

| Today (reads off saturation) | After |
|---|---|
| `saturationEntry(s)` `analyzer_helpers.go:87` | deleted |
| `vc.Cost` in cost-efficiency sort `cost_aware_optimizer.go:238,150,175` | `meta[v].Cost` |
| `vc.AcceleratorName` in `accFromVCs` `analyzer_helpers.go:416`; emitted `:283` | `meta[v].AcceleratorName` |
| `vc.Role` in `variantsForRole`/`rolesOf` `analyzer_helpers.go:226,18` | `meta[v].Role` |
| current replicas (already from `VariantReplicaState`, not vc) | `meta[v].CurrentReplicas` |

`roleBottleneckReplicas` (`analyzer_helpers.go:182`, `max_i ⌈state[i][role]/P_i[v]⌉`) is unchanged — it
already reads `P` per analyzer via `prcForVariant`; it just reads `P` from `VariantTarget` now.

### 3.4 `wva_analyzer_*` metrics

Emit, for every analyzer WVA runs (internal and external), driven from the `(D, P)` outputs:

```
wva_analyzer_demand{analyzer, namespace, model, role?}          # per model instance
wva_analyzer_target{analyzer, namespace, model, scaledobject}   # per ScaledObject (variant)
```

Additive, alongside `RecordSaturationMetrics` (`internal/metrics/metrics.go:869`). Absence is
meaningful (a missing series is not a zero). This both delivers observability and *enforces* the
two-number discipline at the emission boundary.

### 3.5 External-analyzer wrapper (#1455)

A built-in Go analyzer implementing the pure contract from a PromQL definition (per
`docs/proposals/analyzer-metric-interface.md`): templated `{{model}}`/`{{ns}}` (escaped via
`EscapePromQLValue`) demand & target queries, pod→ScaledObject reduction (avg default), ordered target
fallbacks, `orZero`, three-state absent/missing/present. It registers its queries at runtime via the
new `QueryList.Upsert`/`Remove` (already landed, commit `83a04802`). Because identity now comes from
discovery, **the wrapper supplies no metadata** — only `D` and `P` per variant/role. Catalog lives in a
cluster `wva-analyzers` ConfigMap; a policy entry's `name` resolves **built-in registry first
(internal), then catalog (external)**.

---

## 4. Data-model changes (summary)

> **Status:** this table is the **target end-state**. As built, only the top three rows landed —
> `VariantMetadata` (new, with `Engine`), `VariantReplicaState` gaining `Cost`/`AcceleratorName`/`Engine`,
> and the analyzer no longer *emitting* `Cost`/`AcceleratorName`. The rest is **deferred** (see §5
> Phase 3): `AnalyzerResult`/`VariantCapacity` are unchanged, `ReplicaMetrics` still *has* the `Cost`/
> `AcceleratorName` fields (the analyzer just stopped reading them), and `saturationEntry` still exists.

| Type | Change |
|---|---|
| **`VariantMetadata`** (new) | authoritative per-variant identity+state; discovery output |
| `domain.VariantReplicaState` | superseded by / folded into `VariantMetadata` (gains `Cost`, `AcceleratorName`) |
| `domain.AnalyzerResult` | trimmed to `{TotalDemand, RoleDemand, Targets, Reason}` |
| `domain.VariantCapacity` | replaced by `VariantTarget{VariantName, Role, PerReplicaCapacity}`; identity/engine fields removed |
| `domain.RoleCapacity` / `RoleCapacities` | replaced by `RoleDemand map[string]float64` (demand) + engine-side per-role RC/SC on `NamedAnalyzerResult` |
| `domain.ReplicaMetrics` | **loses `Cost` and `AcceleratorName`** — both are variant-level facts, resolved once per variant (`replica_metrics.go:983-1003`) and laundered onto every pod; discovery owns them. `ReplicaMetrics` becomes purely per-pod *signal* (KV usage, tokens, rates) + attribution keys (`PodName`/`VariantName`/`ModelID`/`Namespace`). Capacity-store keying already resolves accelerator directly (`engine_v2.go:48`), not from `ReplicaMetrics`. |
| `NamedAnalyzerResult` (`optimizer_interfaces.go`) | gains the engine-owned `RequiredCapacity`/`SpareCapacity`/anticipated-supply (already has `Remaining`/`Spare`) |
| `saturationEntry` | **deleted** |

---

## 5. Phasing (each phase compiles, tests green, independently landable)

**Phase 0 — Precondition (separate track): remove V1 + queueing-model.** See §6. This design targets
the post-removal engine. If not yet removed, Phases 1–3 still work but must carry the extra switch
arms; removal first is cleaner.

**Phase 1 — Discovery type + producer (additive; no consumer switch). ✅ DONE.**
Introduce `VariantMetadata` and a `DiscoverVariants(ctx) ([]VariantMetadata, error)` that consolidates
`annotationSourcedVariants` + `BuildVariantStates` + role/accelerator resolvers. Produce it each cycle
*alongside* the current path; log/assert it matches the identity the analyzer copies today. **No
behavior change.** De-risks by proving the consolidated source equals the laundered one.

**Phase 2 — Thread discovery metadata into the optimizer; make it the source of truth. ✅ DONE.**
Promote `VariantMetadata` to `domain`; thread the discovery output into
`ModelScalingRequest.Variants`. The engine runs discovery once per model, projects it to
`VariantReplicaState` for the analyzers, and *overlays* the authoritative `Cost`/`Accelerator`/`Role`
onto the analyzers' `VariantCapacity` output before the optimizer runs — so the optimizer reads
identity that came from discovery, not the analyzer's copies. **Behavior-preserving** (values equal
today); no-op on paths that don't run discovery. `saturationEntry` deletion is deferred to Phase 3
(it is still the variant-list/P source until analyzers emit pure `(D, P)`).

**Phase 3 — Trim the contract; analyzers emit pure `(D, P)`; delete `saturationEntry`. ⚠️ PARTIAL — the behavioral split is done; the type-level trim is DEFERRED.**
*Done:*
- *(3.0)* `saturation_v2` stopped laundering per-pod `Cost`/`AcceleratorName` onto its output —
  identity now comes from discovery via the builder overlay.
- *(3.1)* Extracted the dedicated capacity-build step (`buildCapacities` in `engine_v2.go`) that runs
  between every analyzer's `Analyze()` and the optimizer.
- *(3.3a)* The builder assembles per-variant identity, model-level supply, and `RoleCapacities`
  (pairing the analyzer's `RoleDemand` with per-role supply grouped from `VariantCapacities`).
- *(3.3b)* Analyzers now emit **pure `(D, P)`**: `saturation_v2`, `throughput`, and the external
  wrapper no longer set `TotalSupply`, `TotalAnticipatedSupply`, `Utilization`, or `RoleCapacities`.
  The builder derives all four from `VariantCapacities` + `RoleDemand`, so the linearity invariant
  (supply = Σ_v replicas × per-replica P) now holds **by construction** rather than by assertion —
  the analyzer-level specs that policed it were removed as redundant.
  `RequiredCapacity`/`SpareCapacity` were already engine-post-step-owned.

*Deferred (not implemented):* the **type-level** trim — replacing `AnalyzerResult`/`VariantCapacity`
with `VariantTarget`/`RoleDemand` (the engine-owned fields still live on `AnalyzerResult`; analyzers
simply leave them zero), and deleting `saturationEntry` (still at `analyzer_helpers.go:91`, still the
per-variant metadata keeper) + the Phase-2 overlay. §3.2/§3.3/§4 describe that **target end-state**,
not the current code.

**Phase 4 — `wva_analyzer_*` metrics. ✅ DONE.** `wva_analyzer_demand` (per model instance, per role
when disaggregated) and `wva_analyzer_target` (per-replica P per variant) are emitted for every
analyzer that runs each cycle, straight from the `(D, P)` it produced — see `recordAnalyzerMetrics`
in `engine_v2.go`.

§3.4's "absence is meaningful (a missing series is not a zero)" is enforced, not just asserted: the
engine evicts analyzer series it stops publishing. Prometheus gauges cannot enumerate their own
children, so `Engine.lastAnalyzerSeries` records what each model published last cycle and
`evictStaleAnalyzerSeries` deletes what is no longer emitted — a role that disappears when a fleet
stops being disaggregated, a variant that is removed, an analyzer that gets disabled.
`pruneAnalyzerSeries` clears a whole model instance when it stops being reconciled, mirroring
`pruneLastGoodAnalysis` including its empty-active-set guard. Eviction always runs **after** the
cycle's `Set` calls, per the existing "no zero-value window" rule, so a surviving series is never
briefly absent from a concurrent scrape.

**Phase 5 — External-analyzer wrapper (#1455). ✅ DONE.** Delivered as §10 describes (catalog CM +
per-engine bodies + runtime registry + per-cycle reconcile + built-in→catalog name resolution). Unit
+ envtest coverage at every layer. The KEDA external-scaler **kind e2e** has since been run green
(smoke 17/17, full 28/28), and the saturation suite was moved onto the external-scaler transport so
the analyzer decision itself is exercised over gRPC, not only the wrapper.

---

## 6. Removal of V1 + queueing-model (precondition detail) — ✅ DONE

Completed this session: V1 (`internal/saturationv1` + `optimizeV1` + helpers) and the queueing-model
analyzer (`internal/engines/analyzers/queueingmodel` + `engine_queueing_model.go`) are removed, V2 is
the sole analysis path, `DefaultVariantCost` was relocated to `internal/domain`, the dead
`internal/queueing` math package and the queueing-model *config* plumbing (`Config.QMAnalyzer*`,
`domain.QueueingModelScalingConfig`, the reconciler QM handlers) are removed;
`RegisterQueueingModelQueries` is retained (shared with the throughput analyzer). The `engine.go` line
references below are from the pre-removal plan and no longer resolve.

- **`saturationv1.DefaultVariantCost`** is used outside V1 — `engine.go:1505` and
  `collector/replica_metrics.go:998`. Removing `internal/saturationv1/` must **relocate this constant**
  (e.g. to `internal/domain` or `internal/annotations`, next to the `llm-d.ai/variant-cost` default).
- V1 wiring to delete: `engine.go` `v1Analyzer` iface (`:69`), `defaultV1AnalyzerFactory` (`:86`),
  `v1AnalyzerFactory` field/wiring (`:230,283`), `optimizeV1` (`:665`), switch default (`:558`).
- Queueing-model wiring to delete: `engine.go` field (`:181`), construction (`:278`), selection
  (`:508-524`), `optimizeQueueingModel` (`engine_queueing_model.go`), switch arm (`:554`).

---

## 7. Test impact

- **Analyzer suites** (`saturation_v2/*_test.go`, `throughput/*_test.go`) — update result assertions to
  the trimmed `(D, P)` shape.
- **Engine** — `engine_register_test.go`, `engine_v2_threshold_test.go` (`applyUniversalThreshold`),
  `engine_v2_population_test.go`; add discovery-producer tests.
- **Optimizer** — `analyzer_helpers_test.go` (`makeNamed`/`makeNamedPD` builders switch to supplying
  metadata via discovery, not the sat result), `cost_aware_optimizer_test.go`,
  `optimizer_equivalence_test.go`.
- **Config** — catalog parsing + name resolution (Phase 5).
- **e2e** — re-run the KEDA external-scaler kind smoke on the epic branch after Phase 5.

---

## 8. Risks & mitigations

1. **Optimizer-core blast radius (Phase 3).** Mitigation: Phases 1–2 make the source switch
   behavior-preserving *before* the shape change; Phase 3 is then mechanical on a green base. The
   `optimizer_equivalence_test` is the guardrail.
2. **`DefaultVariantCost` relocation collides with the V1-removal track.** Mitigation: relocate the
   constant as the *first* step of whichever track lands first; the other rebases onto it.
3. **Discovery/laundering mismatch** (a field the analyzer massaged, e.g. DP-rank `ReplicaCount`
   conversion at `analyzer.go:400-416`). Mitigation: Phase 1's parallel-produce + assert catches any
   divergence before consumers switch; DP-rank conversion is a *capacity* concern and stays in the
   analyzer, not discovery.
4. **Shadow-pod / role attribution** relies on `llm-d.ai/role` + owner-walk. Mitigation: discovery
   reuses the exact existing resolvers (`getRoleFromScaleTarget`, locator), no new attribution logic.

---

## 9. Open questions

- **Discovery placement:** a new `internal/discovery` package, or a method on the engine consuming
  `internal/utils/variant.go`? (Leaning: thin `internal/engines/discovery` that returns
  `[]VariantMetadata`, so analyzers/optimizer import a type, not the engine.)
- **`VariantReplicaState` vs new `VariantMetadata`:** extend the former in place, or introduce the
  latter and deprecate? (Leaning: introduce `VariantMetadata`, alias/embed during transition.)
- ~~**Per-pod `AcceleratorName` on `ReplicaMetrics`:**~~ **Resolved** — drop `AcceleratorName` *and*
  `Cost` from `ReplicaMetrics`. Both are variant-level facts resolved once per variant
  (`replica_metrics.go:983-1003`) and stamped identically onto every pod; no consumer needs them
  per-pod (capacity keying already uses `GetAcceleratorNameFromScaleTarget` directly at
  `engine_v2.go:48`). Discovery owns them; `ReplicaMetrics` keeps only per-pod signal + attribution
  keys.

---

## 10. #1455 external analyzers — as built

The external-analyzer wrapper is wired through the **ScalingPolicy configuration system**, split
across ConfigMaps exactly as §7.6 of the KEDA-external-scaler proposal describes — not collapsed into
a single policy entry:

- **Catalog** (cluster CM **`wva-analyzers`**) — external analyzer *definitions*: `label →
  {engines: {vllm:{query,threshold}, sglang:{…}} | query,threshold}` (per-engine bodies, mirroring
  the collector's `registerForEngine`; an engine-agnostic body has no `engines:` map). Parsed by
  `config.ParseAnalyzerCatalogConfigMap`, stored on `Config`, refreshed by the ConfigMap reconciler.
- **Policy** (tier CM) — *selects/weights* by name (`analyzers: [{name, enabled, score}]`). A policy
  entry's `name` resolves **built-in registry first, then catalog**; because a plain `{name: ttft-slo}`
  entry has `EffectiveType() == Name`, the existing `effectiveEnabled`/`scoreForAnalyzer`/
  `resolveThresholds` matching needed no change — resolution is just "construct from the catalog when
  the name is not a built-in".
- **Wrapper** — `internal/engines/analyzers/external`: emits pure `(D, P)` (`desired = ceil(D/P)`),
  selects the query body by the variant's `Engine` (a discovery field via `inferenceengine.Detect`),
  and returns a nil result (not-defined → skipped) when no body matches.
- **Runtime add/remove** — the engine holds a lock-guarded, name-keyed external registry
  (`UpsertExternalAnalyzer`/`RemoveExternalAnalyzer`) separate from the frozen built-in snapshot, and
  `reconcileExternalAnalyzers` syncs it with the catalog **each optimize cycle**, so a `wva-analyzers`
  edit takes effect **without a restart**.

## 11. Security — PromQL trust model

The catalog introduces a raw-PromQL config surface. The posture matches KEDA's Prometheus scaler,
with one addition we already have:

- **The query body is trusted config.** Like KEDA's ScaledObject `query`, the catalog PromQL is run
  as-is — WVA does **not** semantically sanitize or whitelist it (infeasible, and KEDA doesn't). The
  trust boundary is **RBAC on the `wva-analyzers` ConfigMap**: editing it is a privileged operation.
- **Interpolated identity is escaped.** Unlike KEDA, WVA interpolates `{{.modelID}}`/`{{.namespace}}`
  (from a less-trusted ScaledObject annotation) into the query — and `PrometheusSource.executeQuery`
  runs both through `EscapePromQLValue` before substitution (`prometheus_source.go:121-123`), so a
  crafted `modelID` cannot break out of a label matcher. This is the genuine injection defense.
- **Bounded cost + fail-safe.** Each query has a 10s timeout (`prometheus_source.go:35,140`); a
  failed/empty/malformed query yields **0 demand → no scaling action**, so a bad definition cannot
  drive runaway scaling — it simply contributes nothing. Heavy expressions should be precomputed as
  Prometheus recording rules.
- **Load-time validation is deliberately minimal** — `external.New` rejects an empty query or a
  non-positive threshold (bad def skipped, logged); we do **not** pull in the full
  `github.com/prometheus/prometheus` PromQL parser just to syntax-check, since the timeout + fail-safe
  already contain a bad query's blast radius.
