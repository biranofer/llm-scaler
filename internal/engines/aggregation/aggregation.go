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

// Package aggregation provides pure helper functions for aggregating
// per-variant capacity data into model-level and per-role totals. The helpers
// are split along the analyzer/engine contract:
//
// Analyzers own demand. They use SumTotalDemand and DemandByRole (plus
// IsDisaggregated to decide whether a per-role breakdown applies) to populate
// AnalyzerResult.TotalDemand and AnalyzerResult.RoleDemand.
//
// The engine's capacity-build step owns supply. It uses SumTotalSupply,
// SumTotalAnticipatedSupply, and AggregateByRole to derive
// AnalyzerResult.Total*/Utilization and to assemble
// AnalyzerResult.RoleCapacities from the analyzer's RoleDemand.
//
// Deriving both halves from the same VariantCapacities is what makes the
// linearity invariant required by the optimizer's per-variant scaling math hold
// by construction:
//
//	r.TotalSupply            == Σ_v vc.ReplicaCount × vc.PerReplicaCapacity
//	r.TotalAnticipatedSupply == Σ_v (vc.ReplicaCount + vc.PendingReplicas) × vc.PerReplicaCapacity
//	r.TotalDemand            == Σ_v vc.TotalDemand
//	r.RoleCapacities[role].* == same sums filtered by vc.Role == role
package aggregation

import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"

// ScopeTotals holds the three model-level (or per-role) aggregates that the
// engine's universal threshold post-step reads to compute RC and SC.
type ScopeTotals struct {
	TotalSupply            float64
	TotalAnticipatedSupply float64
	TotalDemand            float64
}

// SumTotalSupply returns Σ_v vc.ReplicaCount × vc.PerReplicaCapacity.
func SumTotalSupply(vcs []domain.VariantCapacity) float64 {
	var total float64
	for _, vc := range vcs {
		total += float64(vc.ReplicaCount) * vc.PerReplicaCapacity
	}
	return total
}

// SumTotalAnticipatedSupply returns
// Σ_v (vc.ReplicaCount + vc.PendingReplicas) × vc.PerReplicaCapacity.
// Pending replicas count toward anticipated supply so an in-flight scale-up
// reduces the computed RC and prevents double-scaling.
func SumTotalAnticipatedSupply(vcs []domain.VariantCapacity) float64 {
	var total float64
	for _, vc := range vcs {
		total += float64(vc.ReplicaCount+vc.PendingReplicas) * vc.PerReplicaCapacity
	}
	return total
}

// DemandByRole groups vcs by role and sums each group's TotalDemand. It is the
// demand-only projection of AggregateByRole: analyzers own demand attribution
// and emit it as AnalyzerResult.RoleDemand, while per-role supply is recomputed
// by the engine's capacity-build step. The returned map's keys are also the set
// of roles present in vcs, which analyzers use to decide how to distribute
// demand. Role canonicalization therefore matches AggregateByRole by
// construction: an empty role is treated as domain.RoleBoth.
func DemandByRole(vcs []domain.VariantCapacity) map[string]float64 {
	totals := AggregateByRole(vcs)
	result := make(map[string]float64, len(totals))
	for role, t := range totals {
		result[role] = t.TotalDemand
	}
	return result
}

// IsDisaggregated reports whether vcs describes a P/D-disaggregated model, i.e.
// whether any variant serves a role other than domain.RoleBoth. An empty role is
// canonicalized to domain.RoleBoth, so a fleet of only empty/"both" variants is
// not disaggregated and its analyzer emits model-level demand with no per-role
// breakdown.
func IsDisaggregated(vcs []domain.VariantCapacity) bool {
	for _, vc := range vcs {
		if vc.Role != "" && vc.Role != domain.RoleBoth {
			return true
		}
	}
	return false
}

// SumTotalDemand returns Σ_v vc.TotalDemand.
func SumTotalDemand(vcs []domain.VariantCapacity) float64 {
	var total float64
	for _, vc := range vcs {
		total += vc.TotalDemand
	}
	return total
}

// AggregateByRole groups vcs by role and computes ScopeTotals for each group.
// An empty or blank role string is canonicalized to domain.RoleBoth,
// consistent with saturation_v2's role normalization.
func AggregateByRole(vcs []domain.VariantCapacity) map[string]ScopeTotals {
	result := make(map[string]ScopeTotals)
	for _, vc := range vcs {
		role := vc.Role
		if role == "" {
			role = domain.RoleBoth
		}
		t := result[role]
		t.TotalSupply += float64(vc.ReplicaCount) * vc.PerReplicaCapacity
		t.TotalAnticipatedSupply += float64(vc.ReplicaCount+vc.PendingReplicas) * vc.PerReplicaCapacity
		t.TotalDemand += vc.TotalDemand
		result[role] = t
	}
	return result
}
