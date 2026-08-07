// Package external implements a built-in analyzer that turns a config-driven
// PromQL definition into a domain.Analyzer, so WVA can be extended with new
// scaling signals without writing Go (issue #1455).
//
// An external analyzer mirrors KEDA scaler vocabulary: a demand query (the total
// signal D) and a per-replica target (threshold P), such that desired replicas =
// ceil(D / P). Like the collector's registerForEngine, the query body is
// per-engine: a Definition carries one Body per inference engine (vllm, sglang),
// plus an optional engine-agnostic body; Analyze selects the body matching the
// model's engine. It runs the selected query each cycle via the shared metrics
// source and reduces the result to D; P is the body's constant threshold.
//
// Per-variant identity (cost, accelerator) is filled downstream by the capacity
// builder from the discovery step, so the wrapper emits only the measured (D, P)
// signal — the same contract the internal analyzers follow.
package external

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// queryNamePrefix namespaces external-analyzer queries in the shared QueryList so
// they cannot collide with the built-in template names.
const queryNamePrefix = "external:"

// reasonConstantTarget labels a per-replica capacity that came from a constant
// threshold rather than a measured query.
const reasonConstantTarget = "E1-const"

// engineAgnostic is the Bodies key for a body that applies to any engine. It is
// used when no engine-specific body matches the model's engine.
const engineAgnostic = ""

// Body is one engine's query and per-replica target for an external analyzer.
// The field names mirror KEDA's Prometheus scaler.
type Body struct {
	// Query is a PromQL expression for the total demand D. It may reference
	// {{.namespace}} and {{.modelID}}; the metrics source escapes both before
	// substitution.
	Query string
	// Threshold is the per-replica target P — the demand a single replica can
	// serve. desired = ceil(D / threshold). Must be > 0.
	Threshold float64
}

// Definition describes a config-driven external analyzer with per-engine query
// bodies. A body under the engineAgnostic ("") key applies to any engine.
type Definition struct {
	// Label is the analyzer's unique name, used for policy selection and metrics.
	Label string
	// Bodies maps an inference engine ("vllm", "sglang") to its query body; the
	// empty-string key is the engine-agnostic body. At least one is required.
	Bodies map[string]Body
}

// Analyzer is the built-in wrapper that runs a Definition as a domain.Analyzer.
type Analyzer struct {
	def    Definition
	source source.MetricsSource
	// queryNames maps an engine key to the query name registered for its body.
	queryNames map[string]string
}

// New constructs an external analyzer from def and registers each body's query
// into src's shared QueryList. Registration uses Upsert, so re-initialization
// (e.g. a catalog reload) is safe.
func New(def Definition, src source.MetricsSource) (*Analyzer, error) {
	if def.Label == "" {
		return nil, errors.New("external analyzer: label is required")
	}
	if len(def.Bodies) == 0 {
		return nil, fmt.Errorf("external analyzer %q: at least one query body is required", def.Label)
	}
	if src == nil {
		return nil, fmt.Errorf("external analyzer %q: metrics source is required", def.Label)
	}

	queryNames := make(map[string]string, len(def.Bodies))
	for engineKey, body := range def.Bodies {
		if body.Query == "" {
			return nil, fmt.Errorf("external analyzer %q (%s): query is required", def.Label, engineLabel(engineKey))
		}
		if body.Threshold <= 0 {
			return nil, fmt.Errorf("external analyzer %q (%s): threshold must be > 0, got %v", def.Label, engineLabel(engineKey), body.Threshold)
		}
		name := queryName(def.Label, engineKey)
		if err := src.QueryList().Upsert(source.QueryTemplate{
			Name:        name,
			Type:        source.QueryTypePromQL,
			Template:    body.Query,
			Params:      []string{source.ParamNamespace, source.ParamModelID},
			Description: fmt.Sprintf("demand (D) for external analyzer %q (%s)", def.Label, engineLabel(engineKey)),
		}); err != nil {
			return nil, fmt.Errorf("external analyzer %q (%s): registering demand query: %w", def.Label, engineLabel(engineKey), err)
		}
		queryNames[engineKey] = name
	}

	return &Analyzer{def: def, source: src, queryNames: queryNames}, nil
}

// Name returns the analyzer's label.
func (a *Analyzer) Name() string { return a.def.Label }

// Analyze selects the query body for the model's engine, runs it, and produces a
// (D, P) result: D is the summed query value, P is the body's constant threshold.
// When no body matches the model's engine the analyzer does not apply to this
// model and returns a nil result (the engine skips it — the not-defined state,
// distinct from a present-but-zero result).
func (a *Analyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	body, qName, ok := a.selectBody(modelEngine(input.VariantStates))
	if !ok {
		return nil, nil
	}

	demand, err := a.queryDemand(ctx, qName, input.ModelID, input.Namespace)
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
			PerReplicaCapacity: body.Threshold,
			Reason:             reasonConstantTarget,
		})
	}

	return &domain.AnalyzerResult{
		AnalyzerName:      a.def.Label,
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        time.Now(),
		VariantCapacities: vcs,
		TotalDemand:       demand,
		// Supply, utilization, and RoleCapacities are assembled downstream by the
		// engine's capacity-build step; the wrapper emits only the (D, P) signal.
		// RequiredCapacity/SpareCapacity are written by the engine post-step.
		// RoleDemand is nil: this MVP treats demand as model-level.
	}, nil
}

// selectBody returns the body (and its query name) for engine, falling back to
// the engine-agnostic body. ok is false when neither matches.
func (a *Analyzer) selectBody(engine string) (Body, string, bool) {
	if b, ok := a.def.Bodies[engine]; ok {
		return b, a.queryNames[engine], true
	}
	if b, ok := a.def.Bodies[engineAgnostic]; ok {
		return b, a.queryNames[engineAgnostic], true
	}
	return Body{}, "", false
}

// queryDemand runs the given query name for (modelID, namespace) and returns the
// sum of all returned series — the model-level total demand. A missing or failed
// query this cycle yields zero observed demand.
func (a *Analyzer) queryDemand(ctx context.Context, qName, modelID, namespace string) (float64, error) {
	results, err := a.source.Refresh(ctx, source.RefreshSpec{
		Queries: []string{qName},
		Params: map[string]string{
			source.ParamModelID:   modelID,
			source.ParamNamespace: namespace,
		},
	})
	if err != nil {
		return 0, fmt.Errorf("external analyzer %q: demand query: %w", a.def.Label, err)
	}

	res := results[qName]
	if res == nil || res.HasError() {
		return 0, nil
	}
	var demand float64
	for _, v := range res.Values {
		demand += v.Value
	}
	return demand, nil
}

// modelEngine returns the model's inference engine from its variant states.
// MVP: it uses the first variant that declares an engine (models are assumed
// engine-homogeneous); mixed-engine models are a follow-up.
func modelEngine(states []domain.VariantReplicaState) string {
	for _, vs := range states {
		if vs.Engine != "" {
			return vs.Engine
		}
	}
	return ""
}

// queryName builds the registered query name for an analyzer label and engine
// key. The engine-agnostic body has no engine suffix.
func queryName(label, engineKey string) string {
	if engineKey == engineAgnostic {
		return queryNamePrefix + label + ":demand"
	}
	return queryNamePrefix + label + ":demand:" + engineKey
}

// engineLabel renders an engine key for error messages.
func engineLabel(engineKey string) string {
	if engineKey == engineAgnostic {
		return "engine-agnostic"
	}
	return "engine=" + engineKey
}
