package steadystate

import (
	"context"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// blockedModelRef is the identity needed to delete a model's
// wva_model_scaling_blocked series once the model is gone, plus the reasons last
// published for it so a change can be logged without repeating on every cycle.
type blockedModelRef struct {
	namespace string
	modelID   string
	// reasons is the last published set, joined, purely for comparison.
	reasons string
}

// recordBlockedModel notes that this engine has published reasons for a model,
// so pruneBlockedModels can find the series again after the model disappears,
// and reports whether the set CHANGED since the previous cycle.
//
// The bookkeeping doubles as the transition state deliberately. A separate map
// would need its own eviction, on a process that runs for weeks and would
// otherwise retain an entry for every model it has ever seen; sharing this one
// means prune and evict already cover it.
func (e *Engine) recordBlockedModel(namespace, modelID string, reasons []string) (changed bool) {
	if e.lastBlockedModels == nil {
		e.lastBlockedModels = make(map[string]blockedModelRef)
	}
	key := utils.GetNamespacedKey(namespace, modelID)
	joined := strings.Join(reasons, ",")

	prev, seen := e.lastBlockedModels[key]
	// A model observed for the first time is a change only if something is
	// actually blocking it — otherwise every restart would log a line per healthy
	// model to say nothing is wrong.
	changed = joined != prev.reasons || (!seen && joined != "")

	e.lastBlockedModels[key] = blockedModelRef{
		namespace: namespace,
		modelID:   modelID,
		reasons:   joined,
	}
	return changed
}

// logBlockedTransition reports a change in why a model cannot reach zero.
//
// Info at V(0), not a warning: logr has Info and Error and no Warn, and none of
// these reasons is an error — a model that will never park is serving perfectly
// well, which is exactly why nothing else reports it. On transition only,
// because the alternative on a per-interval loop is the same line forever.
func logBlockedTransition(ctx context.Context, namespace, modelID string, reasons []string) {
	logger := ctrl.LoggerFrom(ctx)
	if len(reasons) == 0 {
		logger.Info("Model can now reach zero: nothing is blocking it",
			"modelID", modelID, "namespace", namespace)
		return
	}
	logger.Info("Model will not scale to zero",
		"modelID", modelID,
		"namespace", namespace,
		"reasons", strings.Join(reasons, ","),
		"detail", blockedDetail(reasons))
}

// blockedDetail spells out what each reason means for this model, since the
// reason slugs are chosen for a metric label rather than for a reader.
func blockedDetail(reasons []string) string {
	details := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		switch reason {
		case constants.ScalingBlockedVariantFloor:
			details = append(details, "scale-to-zero is enabled but a variant declares minReplicas > 0, "+
				"so the model never reaches zero and the setting is inert")
		case constants.ScalingBlockedPolicyForbidsZero:
			details = append(details, "every variant permits zero but this model's scaling policy disables "+
				"scale-to-zero, so the replica bounds are inert")
		case constants.ScalingBlockedEngineUnsupported:
			details = append(details, "the model runs more than one inference engine, so no single "+
				"request counter measures its idleness; vLLM and SGLang are each supported alone")
		default:
			details = append(details, reason)
		}
	}
	return strings.Join(details, "; ")
}

// pruneBlockedModels drops every reason series for a model that is no longer
// active, and forgets its bookkeeping.
//
// An empty activeKeys is a no-op, mirroring pruneAnalyzerSeries: a cycle that
// enumerates no models is usually transient — a collector hiccup, config not
// loaded yet — and must not be read as "every model went away". The genuinely
// empty fleet is handled by evictAllBlockedModels, on the path that has already
// proved the list succeeded.
func (e *Engine) pruneBlockedModels(activeKeys map[string]bool) {
	if len(activeKeys) == 0 || e.lastBlockedModels == nil {
		return
	}
	for modelKey, ref := range e.lastBlockedModels {
		if !activeKeys[modelKey] {
			metrics.ClearModelScalingBlocked(ref.namespace, ref.modelID)
			delete(e.lastBlockedModels, modelKey)
		}
	}
}

// evictAllBlockedModels removes every reason series this engine has published.
// Used when a cycle finds no active models at all, where the per-model prune has
// nothing to compare against. Idempotent — it empties its own bookkeeping.
func (e *Engine) evictAllBlockedModels() {
	for modelKey, ref := range e.lastBlockedModels {
		metrics.ClearModelScalingBlocked(ref.namespace, ref.modelID)
		delete(e.lastBlockedModels, modelKey)
	}
}

// scaleToZeroBlockReasons reports every reason a model cannot reach zero despite
// a configuration that reads as though it should.
//
// It reports CONTRADICTIONS, not settings. Scale-to-zero disabled alongside a
// variant floor is a coherent choice and yields nothing; so does scale-to-zero
// enabled with every variant permitting it. What is worth an operator's
// attention is one half permitting zero while the other forbids it, because the
// half they set is the half they will believe:
//
//   - scale-to-zero enabled, some variant floored — the model never reaches zero
//     and the setting is inert. This one has no other symptom at all: the model
//     is up and serving, exactly as every other metric says it should be, while
//     the accelerators are billed indefinitely.
//   - every variant floored at zero, policy disabling scale-to-zero — the bounds
//     are inert. Most misleading with a single variant, where minReplicas: 0
//     reads exactly like a deliberate request to park.
//
// engineSupported folds in the enforcer's own limitation rather than the
// operator's: a non-vLLM engine cannot be measured for idleness, so WVA declines
// to park it. It is reported only when the configuration would otherwise park,
// since otherwise it is a limitation on something nobody asked for.
//
// Pure, so it is worth testing directly: this is the whole of the judgement, and
// the caller does nothing but emit the result.
func scaleToZeroBlockReasons(scaleToZeroEnabled, engineSupported bool, states []domain.VariantReplicaState) []string {
	var reasons []string

	if !scaleToZeroEnabled {
		// Only a contradiction if every variant is asking to park. A floor here
		// agrees with the policy, and agreement is not worth reporting.
		if !hasMinReplicasAboveZero(states) && len(states) > 0 {
			reasons = append(reasons, constants.ScalingBlockedPolicyForbidsZero)
		}
		return reasons
	}

	if hasMinReplicasAboveZero(states) {
		reasons = append(reasons, constants.ScalingBlockedVariantFloor)
	}
	if !engineSupported {
		reasons = append(reasons, constants.ScalingBlockedEngineUnsupported)
	}
	return reasons
}
