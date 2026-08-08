/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalefromzero

import (
	"context"
	"errors"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/log"

	accel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// variantByName finds a group member by variant name.
func (g modelGroup) variantByName(name string) (wvav1alpha1.VariantAutoscaling, bool) {
	for _, va := range g.variants {
		if va.Name == name {
			return va, true
		}
	}
	return wvav1alpha1.VariantAutoscaling{}, false
}

// buildCandidates turns a model's inactive variants into selection candidates
// and resolves the EPP pool they share.
//
// A variant whose pool cannot be resolved is skipped rather than failing the
// model: that is the normal bootstrap state, and one unresolvable variant must
// not stop its siblings from being woken. The pool returned is the first that
// resolves; a model's variants serve the same model behind the same EPP, so they
// share a queue.
func (e *Engine) buildCandidates(
	ctx context.Context,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	group modelGroup,
) ([]Candidate, *poolutil.EndpointPool, error) {
	logger := log.FromContext(ctx)

	var (
		candidates []Candidate
		pool       *poolutil.EndpointPool
	)

	for _, va := range group.variants {
		scaleTarget, err := e.resolveScaleTarget(ctx, scaleTargets, va)
		if err != nil {
			return nil, nil, err
		}
		podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
		if podTemplateSpec == nil {
			return nil, nil, errors.New("pod template spec is missing for target workload object")
		}
		labels := podTemplateSpec.Labels
		if labels == nil {
			return nil, nil, errors.New("labels are missing for target workload object")
		}

		variantPool, err := e.Datastore.PoolGetFromLabels(va.Namespace, labels)
		if err != nil {
			// Only skip on "not found" errors - return other errors to surface real datastore failures
			if errors.Is(err, datastore.ErrPoolNotSynced) {
				logger.V(logging.DEBUG).Info("Skipping variant, target EPP not found in datastore",
					"variant", va.Name,
					"namespace", va.Namespace,
					"modelID", va.Spec.ModelID)
				continue
			}
			logger.Error(err, "Unexpected error finding target EPP",
				"variant", va.Name,
				"namespace", va.Namespace,
				"modelID", va.Spec.ModelID)
			return nil, nil, err
		}
		if pool == nil {
			pool = variantPool
		}

		candidates = append(candidates, Candidate{
			VariantName:     va.Name,
			ScaleTargetName: va.GetScaleTargetName(),
			Role:            discovery.RoleFromScaleTarget(scaleTarget),
			Accelerator:     candidateAccelerator(&va, scaleTarget),
			GPUsPerReplica:  scaleTarget.GetTotalGPUsPerReplica(),
			Cost:            resolveVariantCost(ctx, va),
		})
	}

	if pool == nil {
		return nil, nil, nil
	}
	return candidates, pool, nil
}

// candidateAccelerator returns the variant's accelerator exactly as declared.
//
// It is deliberately NOT normalized here. Pool keys are normalized short names
// while a workload may declare either a full product label
// ("NVIDIA-A100-PCIE-80GB") or an already-short one ("A100"), so the two sides
// have to be reconciled — but doing it by normalizing the candidate is wrong in
// both directions. NormalizeAcceleratorName guesses for names it has no vendor
// prefix for ("Gaudi-2" -> "2", via its `return parts[1]` fallback), so
// normalizing a short name that happens to contain a hyphen produces a value
// matching no pool at all. Reconciliation instead happens at lookup time against
// the pool keys that actually exist — see FitsGPUBudget.
func candidateAccelerator(va *wvav1alpha1.VariantAutoscaling, scaleTarget scaletarget.ScaleTargetAccessor) string {
	return accel.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
}

// resolveVariantCost reads the variant's declared cost, falling back to the project
// default when it is absent or unparseable.
//
// VariantCost is optional (`variantCost,omitempty`), and domain.DefaultVariantCost
// is what every other consumer assumes for an unset one — see costFromVA in
// internal/engines/discovery. Ranking such a variant behind every well-formed one
// instead would invert that default: a fleet that never sets variantCost would
// have no meaningful cost order at all, and a cheap-but-unset variant would lose
// to a declared-expensive one.
func resolveVariantCost(ctx context.Context, va wvav1alpha1.VariantAutoscaling) float64 {
	if va.Spec.VariantCost == "" {
		return domain.DefaultVariantCost
	}
	cost, err := strconv.ParseFloat(va.Spec.VariantCost, 64)
	if err != nil {
		log.FromContext(ctx).V(logging.DEBUG).Info("Variant has an unparseable cost; using the default",
			"variant", va.Name, "variantCost", va.Spec.VariantCost, "default", domain.DefaultVariantCost)
		return domain.DefaultVariantCost
	}
	return cost
}

// gpuConstraints returns the GPU/quota constraints to place a wake within, or
// nil when they cannot be determined.
//
// Nil means "unknown", which FitsGPUBudget treats as permissive. That is
// deliberate and matches the saturation engine's own fallback when no provider
// can supply constraints: proceed rather than block scaling silently. The first
// tick after a restart has no usage snapshot yet, and that is exactly when a
// queued request is waiting to be served.
//
// Three things make this nil, and each means the wake is published without a
// capacity check:
//
//   - No limiter was injected. In production SetGPULimiter is always called, so
//     this only happens when NewLimiterFromConfig itself failed at startup.
//     Declaring no limiters: list does NOT land here: EffectiveLimiterMode then
//     returns the inventory mode, which builds a DefaultLimiter — and that IS a
//     ConstraintProvider, so the physical check still runs.
//
//   - No usage snapshot has been published. The saturation engine is the sole
//     producer; until it completes a cycle over a non-empty population there is
//     no denominator. Note this is independent of enableLimiter — the snapshot is
//     published before the optimizer guard precisely so that a default
//     (CostAware) deployment still feeds this path.
//
//   - A provider could not compute constraints (see below): a partial view would
//     deny types the surviving providers happen not to mention.
//
// The check is therefore best-effort by construction: it prevents waking onto an
// accelerator that is known to be full, and stays out of the way when placement
// cannot be reasoned about.
func (e *Engine) gpuConstraints(ctx context.Context, namespace string) []*pipeline.ResourceConstraints {
	logger := log.FromContext(ctx)

	providers := pipeline.ConstraintProvidersFrom(e.gpuLimiter)
	if len(providers) == 0 {
		return nil
	}

	usage, ok := decision.LatestGPUUsage()
	if !ok {
		logger.V(logging.DEBUG).Info("No GPU usage snapshot published yet; waking without a capacity check",
			"namespace", namespace)
		return nil
	}

	var constraints []*pipeline.ResourceConstraints
	for _, cp := range providers {
		c, err := cp.ComputeConstraints(ctx, usage.ByType, usage.ByNamespace)
		if err != nil {
			// A partial view is worse than none. FitsGPUBudget denies any
			// accelerator type its constraints do not mention, so proceeding with
			// the survivors would turn "this provider could not be reached" into
			// "this variant cannot be placed" — refusing a wake with requests
			// already queued, on the strength of a failed lookup. Fall back to
			// unknown, matching the saturation engine, which drops to its
			// unlimited optimizer rather than let scale-up be silently blocked.
			logger.Info("Could not compute GPU constraints; waking without a capacity check",
				"provider", cp.Name(), "namespace", namespace, "error", err.Error())
			return nil
		}
		constraints = append(constraints, c)
	}
	return constraints
}

// requirePrefill reports whether the model refuses a decode-only wake.
//
// Resolution goes through SaturationScalingConfig.Merge — the same overlay every
// other per-entry setting uses — rather than a hand-rolled precedence check, so
// there is one implementation of "model-specific entry wins over default" and it
// cannot drift from the rest of the config. Only the merge is run, not
// ApplyDefaults/ApplyV2ThresholdDefaults, which calibrate scaling thresholds that
// have no bearing on this field.
func (e *Engine) requirePrefill(modelID, namespace string) bool {
	if e.config == nil {
		return false
	}
	entries := e.config.SaturationConfigForNamespace(namespace)
	resolved := entries["default"] // zero value when absent, which reads as false
	if override, ok := entries[modelID+"#"+namespace]; ok {
		resolved.Merge(override)
	}
	return resolved.RequirePrefillOnScaleFromZero()
}
