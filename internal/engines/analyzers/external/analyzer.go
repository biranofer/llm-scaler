// Package external implements a built-in analyzer that turns a config-driven
// PromQL definition into a domain.Analyzer, so WVA can be extended with new
// scaling signals without writing Go (issue #1455).
//
// An external analyzer mirrors KEDA scaler vocabulary: a demand query (the total
// signal D) and a per-replica target (threshold P), such that desired replicas =
// ceil(D / P). It runs its own query each cycle via the shared metrics source and
// reduces the result to D; P is the definition's constant threshold. Per-variant
// identity (cost, accelerator) is filled downstream by the capacity builder from
// the discovery step, so the wrapper emits only the measured (D, P) signal — the
// same contract the internal analyzers now follow.
package external

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
)

// queryNamePrefix namespaces external-analyzer queries in the shared QueryList so
// they cannot collide with the built-in template names.
const queryNamePrefix = "external:"

// reasonConstantTarget labels a per-replica capacity that came from a constant
// threshold rather than a measured query.
const reasonConstantTarget = "E1-const"

// Definition describes a config-driven external analyzer. The field names mirror
// KEDA's Prometheus scaler so an operator reads it with no translation.
type Definition struct {
	// Label is the analyzer's unique name, used for policy selection and metrics.
	Label string
	// DemandQuery is a PromQL expression for the total demand D. It may reference
	// {{.namespace}} and {{.modelID}}; the metrics source escapes both before
	// substitution.
	DemandQuery string
	// Threshold is the per-replica target P — the amount of demand a single
	// replica can serve. desired = ceil(D / threshold). Must be > 0.
	Threshold float64
}

// Analyzer is the built-in wrapper that runs a Definition as a domain.Analyzer.
type Analyzer struct {
	def       Definition
	source    source.MetricsSource
	queryName string
}

// New constructs an external analyzer from def and registers its demand query
// into src's shared QueryList. Registration uses Upsert, so re-initialization
// (e.g. a catalog reload) is safe.
func New(def Definition, src source.MetricsSource) (*Analyzer, error) {
	if def.Label == "" {
		return nil, errors.New("external analyzer: label is required")
	}
	if def.DemandQuery == "" {
		return nil, fmt.Errorf("external analyzer %q: demandQuery is required", def.Label)
	}
	if def.Threshold <= 0 {
		return nil, fmt.Errorf("external analyzer %q: threshold must be > 0, got %v", def.Label, def.Threshold)
	}
	if src == nil {
		return nil, fmt.Errorf("external analyzer %q: metrics source is required", def.Label)
	}

	queryName := queryNamePrefix + def.Label + ":demand"
	if err := src.QueryList().Upsert(source.QueryTemplate{
		Name:        queryName,
		Type:        source.QueryTypePromQL,
		Template:    def.DemandQuery,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: fmt.Sprintf("demand (D) for external analyzer %q", def.Label),
	}); err != nil {
		return nil, fmt.Errorf("external analyzer %q: registering demand query: %w", def.Label, err)
	}

	return &Analyzer{def: def, source: src, queryName: queryName}, nil
}

// Name returns the analyzer's label.
func (a *Analyzer) Name() string { return a.def.Label }

// Analyze runs the demand query and produces a (D, P) result: D is the summed
// query value (model-level total demand), P is the constant per-replica threshold
// applied to every variant. The engine's universal-threshold post-step computes
// RequiredCapacity/SpareCapacity from these; the optimizer combines this
// analyzer's P with the other analyzers' via the cross-analyzer bottleneck.
func (a *Analyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	demand, err := a.queryDemand(ctx, input.ModelID, input.Namespace)
	if err != nil {
		return nil, err
	}

	vcs := make([]domain.VariantCapacity, 0, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}
		// Cost/AcceleratorName are intentionally unset: the capacity builder fills
		// per-variant identity from discovery. The wrapper emits only P.
		vcs = append(vcs, domain.VariantCapacity{
			VariantName:        vs.VariantName,
			Role:               vs.Role,
			ReplicaCount:       readyCount,
			PendingReplicas:    vs.PendingReplicas,
			PerReplicaCapacity: a.def.Threshold,
			TotalCapacity:      float64(readyCount) * a.def.Threshold,
			Reason:             reasonConstantTarget,
		})
	}

	return &domain.AnalyzerResult{
		AnalyzerName:           a.def.Label,
		ModelID:                input.ModelID,
		Namespace:              input.Namespace,
		AnalyzedAt:             time.Now(),
		VariantCapacities:      vcs,
		TotalSupply:            aggregation.SumTotalSupply(vcs),
		TotalAnticipatedSupply: aggregation.SumTotalAnticipatedSupply(vcs),
		TotalDemand:            demand,
		// RequiredCapacity/SpareCapacity are written by the engine post-step.
		// RoleCapacities is nil: this MVP treats demand as model-level
		// (non-disaggregated). Per-role demand is a follow-up increment.
	}, nil
}

// queryDemand runs the demand query for (modelID, namespace) and returns the sum
// of all returned series — the model-level total demand. A missing or failed
// query this cycle yields zero observed demand (absent-vs-missing semantics are
// a follow-up).
func (a *Analyzer) queryDemand(ctx context.Context, modelID, namespace string) (float64, error) {
	results, err := a.source.Refresh(ctx, source.RefreshSpec{
		Queries: []string{a.queryName},
		Params: map[string]string{
			source.ParamModelID:   modelID,
			source.ParamNamespace: namespace,
		},
	})
	if err != nil {
		return 0, fmt.Errorf("external analyzer %q: demand query: %w", a.def.Label, err)
	}

	res := results[a.queryName]
	if res == nil || res.HasError() {
		return 0, nil
	}
	var demand float64
	for _, v := range res.Values {
		demand += v.Value
	}
	return demand, nil
}
