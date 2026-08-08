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

		cost, err := strconv.ParseFloat(va.Spec.VariantCost, 64)
		if err != nil {
			// An unparseable cost must not hide the variant: rank it last rather
			// than dropping a variant that may be the only one able to serve.
			logger.V(logging.DEBUG).Info("Variant has an unparseable cost; ranking it last",
				"variant", va.Name, "variantCost", va.Spec.VariantCost)
			cost = maxCost
		}

		candidates = append(candidates, Candidate{
			VariantName:     va.Name,
			ScaleTargetName: va.GetScaleTargetName(),
			Role:            discovery.RoleFromScaleTarget(scaleTarget),
			Accelerator:     accel.GetAcceleratorNameFromScaleTarget(&va, scaleTarget),
			GPUsPerReplica:  scaleTarget.GetTotalGPUsPerReplica(),
			Cost:            cost,
		})
	}

	if pool == nil {
		return nil, nil, nil
	}
	return candidates, pool, nil
}

// maxCost ranks a variant with an unparseable cost behind every well-formed one.
const maxCost = 1e18

// gpuConstraints returns the GPU/quota constraints to place a wake within, or
// nil when they cannot be determined.
//
// Nil means "unknown", which FitsGPUBudget treats as permissive. That is
// deliberate and matches the saturation engine's own fallback when no provider
// can supply constraints: proceed rather than block scaling silently. The first
// tick after a restart has no usage snapshot yet, and that is exactly when a
// queued request is waiting to be served.
func (e *Engine) gpuConstraints(ctx context.Context, namespace string) []*pipeline.ResourceConstraints {
	logger := log.FromContext(ctx)

	providers := e.constraintProviders()
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
			logger.V(logging.DEBUG).Error(err, "Failed to compute GPU constraints, skipping provider",
				"provider", cp.Name())
			continue
		}
		constraints = append(constraints, c)
	}
	return constraints
}

// constraintProviders unwraps the configured limiter into the providers that can
// supply constraints, mirroring the saturation engine's gpuConstraintProviders.
func (e *Engine) constraintProviders() []pipeline.ConstraintProvider {
	if e.gpuLimiter == nil {
		return nil
	}
	switch lim := e.gpuLimiter.(type) {
	case *pipeline.CompositeLimiter:
		var providers []pipeline.ConstraintProvider
		for _, c := range lim.Constituents() {
			if cp, ok := c.(pipeline.ConstraintProvider); ok {
				providers = append(providers, cp)
			}
		}
		return providers
	case pipeline.ConstraintProvider:
		return []pipeline.ConstraintProvider{lim}
	}
	return nil
}

// requirePrefill reports whether the model refuses a decode-only wake.
//
// Precedence matches the rest of the saturation config: a model-specific entry
// wins over the "default" entry. Only this one field is resolved rather than
// running the full merge, which also calibrates thresholds that have no bearing
// here.
func (e *Engine) requirePrefill(modelID, namespace string) bool {
	if e.config == nil {
		return false
	}
	entries := e.config.SaturationConfigForNamespace(namespace)
	if override, ok := entries[modelID+"#"+namespace]; ok &&
		override.ScaleFromZero != nil && override.ScaleFromZero.RequirePrefill != nil {
		return *override.ScaleFromZero.RequirePrefill
	}
	if def, ok := entries["default"]; ok {
		return def.RequirePrefillOnScaleFromZero()
	}
	return false
}
