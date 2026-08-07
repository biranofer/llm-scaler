package pipeline

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// satEntryFixture is the test-side builder for one analyzer entry in a
// ModelScalingRequest.
//
// It carries both halves of what the engine hands the optimizer in a single
// flat literal: the analyzer's own (D, P) signal, and — below the line — the
// aggregates the engine's capacity-build step derives from it. Production code
// cannot conflate the two, which is the point of the split: domain.AnalyzerResult
// has no supply and no scaling-signal fields, so an analyzer cannot write a
// supply that contradicts its own (D, P).
//
// Optimizer tests still need to state both, because they exercise what the
// optimizer *does with* a given (RequiredCapacity, SpareCapacity,
// RoleCapacities) — not how those were derived from (D, P). Deriving them here
// would couple every optimizer test to the threshold formula and make the
// intended scenario unreadable. Tests for the derivation itself live in
// internal/engines/saturation (engine_v2_capacity_build_test.go,
// engine_v2_threshold_test.go) and use the real types.
//
// named() performs the split, mirroring the engine's buildNamedResult.
type satEntryFixture struct {
	// --- Analyzer-owned: the pure (D, P) signal. ---
	AnalyzerName      string
	ModelID           string
	Namespace         string
	AnalyzedAt        time.Time
	VariantCapacities []domain.VariantCapacity
	TotalDemand       float64
	RoleDemand        map[string]float64

	// --- Engine-owned: derived by buildCapacities in production. ---
	TotalSupply            float64
	TotalAnticipatedSupply float64
	Utilization            float64
	RequiredCapacity       float64
	SpareCapacity          float64
	RoleCapacities         map[string]domain.RoleCapacity
}

// withScore sets the fair-share weight the engine would have resolved from
// AnalyzerScoreConfig. Returns a copy so it chains off named().
func (n NamedAnalyzerResult) withScore(s float64) NamedAnalyzerResult {
	n.Score = s
	return n
}

// result returns the analyzer-owned half, exactly as an analyzer would emit it.
func (f *satEntryFixture) result() *domain.AnalyzerResult {
	if f == nil {
		return nil
	}
	return &domain.AnalyzerResult{
		AnalyzerName:      f.AnalyzerName,
		ModelID:           f.ModelID,
		Namespace:         f.Namespace,
		AnalyzedAt:        f.AnalyzedAt,
		VariantCapacities: f.VariantCapacities,
		TotalDemand:       f.TotalDemand,
		RoleDemand:        f.RoleDemand,
	}
}

// named builds the optimizer-facing entry the way the engine does: the
// analyzer's result under Result, the engine-owned aggregates alongside it, and
// the mutable Remaining/Spare counters seeded from RC/SC.
//
// name defaults to the saturation analyzer when empty, since that is the entry
// the optimizer's per-variant metadata helpers look for.
func (f *satEntryFixture) named(name string) NamedAnalyzerResult {
	if name == "" {
		name = domain.SaturationAnalyzerName
	}
	return NamedAnalyzerResult{
		Name:                   name,
		Result:                 f.result(),
		TotalSupply:            f.TotalSupply,
		TotalAnticipatedSupply: f.TotalAnticipatedSupply,
		Utilization:            f.Utilization,
		RequiredCapacity:       f.RequiredCapacity,
		SpareCapacity:          f.SpareCapacity,
		RoleCapacities:         f.RoleCapacities,
		Remaining:              f.RequiredCapacity,
		Spare:                  f.SpareCapacity,
		Live:                   true,
	}
}
