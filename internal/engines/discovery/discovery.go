// Package discovery resolves the authoritative per-variant metadata for one
// optimization cycle: variant identity (name, model, namespace, role,
// accelerator, cost) together with replica and GPU state, sourced from the
// managed scale targets and their synthesized VariantAutoscaling records.
//
// It is intended to be the single source of this metadata for both the analyzers
// and the optimizer, replacing the previous scheme in which the saturation
// analyzer laundered identity onto its per-variant capacity output (and cost and
// accelerator name were stamped onto every pod's ReplicaMetrics). Analyzers can
// then reduce to pure (demand, per-replica-capacity) producers.
package discovery

import (
	"context"
	"strconv"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// RoleLabel is the pod-template label carrying a variant's P/D disaggregation
// role, and the sole signal WVA uses to distinguish roles. Any other value, or
// its absence, means the non-disaggregated role "both".
const RoleLabel = "llm-d.ai/role"

// Discover resolves the domain.VariantMetadata for each VariantAutoscaling. It prefers a
// scale target from the provided map (populated earlier in the cycle) and falls
// back to a live fetch; a VA whose scale target cannot be resolved is skipped
// (logged at DEBUG), matching the previous BuildVariantStates behavior.
func Discover(
	ctx context.Context,
	vas []llmdvariant.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
) []domain.VariantMetadata {
	logger := ctrl.LoggerFrom(ctx)
	metas := make([]domain.VariantMetadata, 0, len(vas))

	for i := range vas {
		va := &vas[i]

		scaleTarget, ok := resolveScaleTarget(ctx, va, scaleTargets, k8sClient)
		if !ok {
			continue
		}

		currentReplicas := int(scaleTarget.GetStatusReplicas())
		if currentReplicas == 0 && scaleTarget.GetReplicas() != nil {
			currentReplicas = int(*scaleTarget.GetReplicas())
		}

		readyReplicas := int(scaleTarget.GetStatusReadyReplicas())
		pendingReplicas := currentReplicas - readyReplicas
		if pendingReplicas < 0 {
			// readyReplicas exceeding currentReplicas is an unexpected state;
			// surface it and clamp so downstream sizing is not skewed.
			logger.Info("Unexpected state: readyReplicas exceeds currentReplicas, clamping pendingReplicas to 0",
				"variant", va.Name, "currentReplicas", currentReplicas, "readyReplicas", readyReplicas)
			pendingReplicas = 0
		}

		desiredReplicas := 0
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
			desiredReplicas = int(*va.Status.DesiredOptimizedAlloc.NumReplicas)
		}

		var minReplicas *int
		if va.Spec.MinReplicas != nil {
			v := int(*va.Spec.MinReplicas)
			minReplicas = &v
		}
		var maxReplicas *int
		if va.Spec.MaxReplicas > 0 {
			v := int(va.Spec.MaxReplicas)
			maxReplicas = &v
		}

		metas = append(metas, domain.VariantMetadata{
			VariantName:     va.Name,
			ModelID:         va.Spec.ModelID,
			Namespace:       va.Namespace,
			Role:            RoleFromScaleTarget(scaleTarget),
			Cost:            costFromVA(ctx, va),
			AcceleratorName: accelerator.GetAcceleratorNameFromScaleTarget(va, scaleTarget),
			Engine:          inferenceengine.Detect(scaleTarget).String(),
			GPUsPerReplica:  scaleTarget.GetTotalGPUsPerReplica(),
			CurrentReplicas: currentReplicas,
			DesiredReplicas: desiredReplicas,
			ReadyReplicas:   readyReplicas,
			PendingReplicas: pendingReplicas,
			MinReplicas:     minReplicas,
			MaxReplicas:     maxReplicas,
		})
	}

	return metas
}

// resolveScaleTarget returns the scale target for va, preferring the pre-populated
// map and falling back to a live fetch. Returns ok=false (VA skipped) when no
// scale target can be resolved.
func resolveScaleTarget(
	ctx context.Context,
	va *llmdvariant.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
) (scaletarget.ScaleTargetAccessor, bool) {
	if scaleTargets != nil {
		if st, found := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]; found {
			return st, true
		}
	}

	st, err := scaletarget.FetchScaleTarget(ctx, k8sClient, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
	if err != nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not get scale target for VA, skipping",
			"variant", va.Name, "error", err)
		return nil, false
	}
	return st, true
}

// costFromVA parses the variant's per-replica cost from its spec, falling back to
// DefaultVariantCost when unset or unparseable. Mirrors the previous engine-side
// cost resolution.
func costFromVA(ctx context.Context, va *llmdvariant.VariantAutoscaling) float64 {
	cost := domain.DefaultVariantCost
	if va.Spec.VariantCost != "" {
		if parsed, err := strconv.ParseFloat(va.Spec.VariantCost, 64); err == nil {
			cost = parsed
		} else {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Failed to parse variant cost, using default",
				"variant", va.Name, "variantCost", va.Spec.VariantCost, "default", cost, "error", err)
		}
	}
	return cost
}

// RoleFromScaleTarget resolves a scale target's P/D role from the llm-d role
// label, returning "prefill", "decode", or "both".
//
// The label is the source of truth. Engine arguments are deliberately NOT
// consulted: the flag differs per engine (SGLang has --disaggregation-mode, vLLM
// configures disaggregation through --kv-transfer-config), so parsing them would
// be an engine-by-engine guess at something llm-d already states declaratively.
// "both" is the default when no role is declared, which is correct for a
// non-disaggregated workload — it serves complete requests on its own.
func RoleFromScaleTarget(scaleTarget scaletarget.ScaleTargetAccessor) string {
	if scaleTarget == nil {
		return domain.RoleBoth
	}

	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil || podTemplateSpec.Labels == nil {
		return domain.RoleBoth
	}
	switch podTemplateSpec.Labels[RoleLabel] {
	case domain.RolePrefill:
		return domain.RolePrefill
	case domain.RoleDecode:
		return domain.RoleDecode
	default:
		return domain.RoleBoth
	}
}
