package steadystate

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

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
