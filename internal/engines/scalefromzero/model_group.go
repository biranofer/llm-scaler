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
	"sort"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// modelGroup is one model's inactive variants. Waking is decided per model
// rather than per variant: a P/D model's decode and prefill have to be chosen
// together against a single GPU budget, and all of a model's variants share one
// EPP queue, so the pending-request check is one query for the group.
type modelGroup struct {
	namespace string
	modelID   string
	variants  []wvav1alpha1.VariantAutoscaling
}

// key identifies the group for coverage lookups.
func (g modelGroup) key() string {
	return modelKey(g.namespace, g.modelID)
}

func modelKey(namespace, modelID string) string {
	return namespace + "|" + modelID
}

// groupInactiveByModel buckets inactive variants by (namespace, model). Groups
// are returned in a stable order so repeated ticks process models the same way.
func groupInactiveByModel(vas []wvav1alpha1.VariantAutoscaling) []modelGroup {
	byKey := make(map[string]*modelGroup)
	for _, va := range vas {
		key := modelKey(va.Namespace, va.Spec.ModelID)
		g, ok := byKey[key]
		if !ok {
			g = &modelGroup{namespace: va.Namespace, modelID: va.Spec.ModelID}
			byKey[key] = g
		}
		g.variants = append(g.variants, va)
	}

	groups := make([]modelGroup, 0, len(byKey))
	for _, g := range byKey {
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].namespace != groups[j].namespace {
			return groups[i].namespace < groups[j].namespace
		}
		return groups[i].modelID < groups[j].modelID
	})
	return groups
}

// refusalChanged reports whether outcome differs from the last refusal recorded
// for this model, recording it either way.
//
// The optimize loop runs every 100ms, so a model that cannot be woken produces a
// verdict ten times a second for as long as its requests are queued. Logging
// only transitions keeps the operator-facing signal — "this model has demand and
// was not woken, because X" — without burying the log.
func (e *Engine) refusalChanged(key string, outcome SelectionOutcome) bool {
	e.refusalMu.Lock()
	defer e.refusalMu.Unlock()
	if e.lastRefusal == nil {
		e.lastRefusal = make(map[string]SelectionOutcome)
	}
	if prev, ok := e.lastRefusal[key]; ok && prev == outcome {
		return false
	}
	e.lastRefusal[key] = outcome
	return true
}

// clearRefusal forgets a model's last refusal, so that if it is refused again
// later the reason is reported afresh rather than suppressed as unchanged.
func (e *Engine) clearRefusal(key string) {
	e.refusalMu.Lock()
	defer e.refusalMu.Unlock()
	delete(e.lastRefusal, key)
}

// coverage records which P/D roles a model is already running.
type coverage struct {
	decode  bool
	prefill bool
}

// activeRoleCoverage maps (namespace, model) to the roles its running variants
// already provide. A "both" variant counts as decode: it serves complete
// requests on its own.
//
// This is what keeps scale-from-zero out of the saturation engine's way. A model
// with a running decode can already serve, so a queue building up on it is a
// saturation problem, not an activation one.
func activeRoleCoverage(
	activeVAs []wvav1alpha1.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) map[string]coverage {
	covered := make(map[string]coverage)
	for _, va := range activeVAs {
		key := modelKey(va.Namespace, va.Spec.ModelID)
		c := covered[key]
		target := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
		// A nil target resolves to RoleBoth, which counts as decode — the safe
		// direction, since over-reporting coverage only declines a wake for a
		// model something else is already running.
		switch discovery.RoleFromScaleTarget(target) {
		case domain.RolePrefill:
			c.prefill = true
		default:
			c.decode = true
		}
		covered[key] = c
	}
	return covered
}
