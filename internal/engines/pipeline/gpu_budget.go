package pipeline

import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"

// FitsGPUBudget reports whether allocating demand — GPUs keyed by accelerator
// type — is permitted in namespace under the given constraints.
//
// It answers the question the scale-from-zero engine has to ask before waking a
// variant: would this actually get GPUs, or would its pods sit Pending while the
// request that asked for it times out anyway. Waking something that cannot be
// placed is worse than not waking it, because it looks like progress.
//
// The budget semantics are the optimizer's, reused rather than restated so the
// two cannot drift:
//
//   - Per-type budgets are the minimum available across providers, with the
//     Limit < 0 sentinel meaning unlimited (see mergeConstraints).
//   - A namespace that appears in any provider's NamespacePools is a CLOSED
//     allowlist: an accelerator type it does not list is denied outright, and a
//     listed type is capped at the tighter of its cluster and namespace budgets
//     (see mergeNamespaceConstraints).
//   - An accelerator type no provider knows about is denied, matching the
//     optimizer's treatment of absent constraints as zero capacity rather than
//     unlimited.
//
// No constraints at all means "unknown", which returns true. That mirrors the
// engine's own fallback when no provider can supply constraints: proceed rather
// than block scaling silently. Denying instead would keep a model down for the
// first cycle after every restart — exactly when a queued request is waiting on
// it.
func FitsGPUBudget(constraints []*ResourceConstraints, namespace string, demand map[string]int) bool {
	if len(constraints) == 0 {
		return true
	}

	perType := mergeConstraints(constraints)
	nsBudgets, nsScoped := mergeNamespaceConstraints(constraints)[namespace]

	for rawType, need := range demand {
		if need <= 0 {
			continue
		}

		var budget int
		if nsScoped {
			// A namespace present in any provider's NamespacePools is a CLOSED
			// allowlist, and it — not the cluster aggregate — decides what this
			// namespace may use. Resolve against it FIRST: a type the namespace
			// grants as unlimited is legitimately absent from the cluster Pools
			// (aggregateNamespacePools omits the -1 sentinel), so resolving
			// against the cluster first would deny a wake the optimizer allows.
			nsKey, listed := resolveAcceleratorKey(nsBudgets, rawType)
			if !listed {
				return false
			}
			budget = nsBudgets[nsKey]
			// Still bounded by the cluster pool where one is known: a namespace
			// may not be granted more than physically exists.
			if clusterKey, known := resolveAcceleratorKey(perType, rawType); known {
				budget = tighterBudget(budget, perType[clusterKey])
			}
		} else {
			accType, known := resolveAcceleratorKey(perType, rawType)
			if !known {
				return false
			}
			budget = perType[accType]
		}

		// A negative budget survives tighterBudget only when every contributing
		// budget was unlimited.
		if budget < 0 {
			continue
		}
		if need > budget {
			return false
		}
	}

	return true
}

// GPUBudgets reports the per-accelerator-type budgets an allocation in namespace
// is judged against, and whether that namespace is a closed allowlist (in which
// case a type absent from the map is denied outright rather than unconstrained).
// A negative budget is the unlimited sentinel. Nil constraints yield nil, which
// is "unknown" — the permissive case.
//
// It exists so the budgets can be REPORTED with the same resolution FitsGPUBudget
// applies. Nothing else exposes what the check actually compared against: the
// pools carry Limit and Used, the merge takes the minimum across providers, and a
// namespace allowlist overrides the cluster aggregate — so a log line that summed
// the pools itself would be free to disagree with the verdict it purports to
// explain, which is worse than no line at all.
func GPUBudgets(constraints []*ResourceConstraints, namespace string) (map[string]int, bool) {
	if len(constraints) == 0 {
		return nil, false
	}

	perType := mergeConstraints(constraints)
	nsBudgets, nsScoped := mergeNamespaceConstraints(constraints)[namespace]
	if !nsScoped {
		return perType, false
	}

	// Mirrors the namespace branch of FitsGPUBudget: the allowlist decides what the
	// namespace may use, still bounded by the cluster pool where one is known.
	budgets := make(map[string]int, len(nsBudgets))
	for accType, budget := range nsBudgets {
		if clusterKey, known := resolveAcceleratorKey(perType, accType); known {
			budget = tighterBudget(budget, perType[clusterKey])
		}
		budgets[accType] = budget
	}
	return budgets, true
}

// resolveAcceleratorKey maps an accelerator name as a WORKLOAD declares it onto
// the key an accelerator-keyed map actually uses, reporting whether the map
// covers it.
//
// The two sides are written in different vocabularies and neither can be changed
// unilaterally. Physical pool keys come from node product labels normalized to
// short names ("A100"); quota keys are whatever the operator typed; and a
// workload's nodeSelector or inference.optimization/acceleratorName label may
// carry either the full product name ("NVIDIA-A100-PCIE-80GB") or the short one.
//
// Matching the declared name first and only then its normalization is what keeps
// this correct in both directions. Normalizing unconditionally is not safe:
// NormalizeAcceleratorName falls back to "the segment after the first hyphen" for
// names with no vendor prefix it knows, so an already-short "Gaudi-2" becomes "2"
// and matches nothing. Trying the declared name first means such a name is found
// directly, and only names that genuinely need de-vendoring are normalized.
//
// Shared by every accelerator-keyed lookup — physical limits, quotas, and the
// demand check — so they cannot drift in how they reconcile a name.
func resolveAcceleratorKey(known map[string]int, declared string) (string, bool) {
	if _, ok := known[declared]; ok {
		return declared, true
	}
	if normalized := accelerator.NormalizeAcceleratorName(declared); normalized != declared {
		if _, ok := known[normalized]; ok {
			return normalized, true
		}
	}
	return "", false
}
