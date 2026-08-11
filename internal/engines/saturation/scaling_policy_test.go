package saturation

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// Named policies are reusable TIERS, selected per variant by trigger metadata.
// The point is that they carry no model identity: one tier serves many models, so
// retuning a tier is one edit rather than an edit per model. These pin the
// layering from docs/proposals/wva-keda-external-scaler.md §7.5 —
//
//	default entry → named policy tier → {modelID}#{namespace} override
//
// most specific winning at each step.
func policyConfig() map[string]config.ScalingPolicy {
	return map[string]config.ScalingPolicy{
		"default": {
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			KvCacheThreshold:  0.80,
			Priority:          1.0,
		},
		// Identity-free tiers. Note neither names a model or a namespace.
		// A tier states a consistent BAND, not a lone threshold: lowering
		// scaleUpThreshold below the inherited scaleDownBoundary is an inverted
		// pair, and resolveScalingPolicy resets both to defaults rather than
		// feed the optimizer one.
		"interactive": {
			ScaleUpThreshold:  0.60, // scale out sooner: latency matters more than cost
			ScaleDownBoundary: 0.45,
			Priority:          2.0,
		},
		"batch": {
			ScaleUpThreshold: 0.95, // pack hard: throughput matters more than latency
			Priority:         0.5,
		},
	}
}

func TestPolicyTierOverridesTheDefaultEntry(t *testing.T) {
	cfg := resolveScalingPolicyForTier(policyConfig(), "m", "ns", "interactive")

	if cfg.ScaleUpThreshold != 0.60 {
		t.Errorf("scaleUpThreshold = %v, want the tier's 0.60", cfg.ScaleUpThreshold)
	}
	if cfg.Priority != 2.0 {
		t.Errorf("priority = %v, want the tier's 2.0", cfg.Priority)
	}
	// Fields the tier does not mention still come from the default entry — a tier
	// states what differs, not a whole configuration.
	if cfg.KvCacheThreshold != 0.80 {
		t.Errorf("kvCacheThreshold = %v, want the default entry's 0.80", cfg.KvCacheThreshold)
	}
}

// One tier, many models: the same policy resolves identically for unrelated
// models, which is the property per-(model, namespace) keying cannot provide.
func TestOneTierServesManyModels(t *testing.T) {
	cfgMap := policyConfig()
	a := resolveScalingPolicyForTier(cfgMap, "model-a", "ns-1", "batch")
	b := resolveScalingPolicyForTier(cfgMap, "model-b", "ns-2", "batch")

	if a.ScaleUpThreshold != b.ScaleUpThreshold || a.Priority != b.Priority {
		t.Errorf("the same tier resolved differently for two models: %v vs %v", a, b)
	}
	if a.ScaleUpThreshold != 0.95 {
		t.Errorf("scaleUpThreshold = %v, want the batch tier's 0.95", a.ScaleUpThreshold)
	}
}

// The per-model override stays the innermost layer, so a fleet can adopt tiers
// model by model instead of all at once.
func TestPerModelOverrideBeatsTheTier(t *testing.T) {
	cfgMap := policyConfig()
	cfgMap[config.ModelOverrideKey("m", "ns")] = config.ScalingPolicy{
		ScaleUpThreshold: 0.50, ScaleDownBoundary: 0.35,
	}

	cfg := resolveScalingPolicyForTier(cfgMap, "m", "ns", "interactive")
	if cfg.ScaleUpThreshold != 0.50 {
		t.Errorf("scaleUpThreshold = %v, want the per-model override's 0.50", cfg.ScaleUpThreshold)
	}
	if cfg.Priority != 2.0 {
		t.Errorf("priority = %v; the override is silent on it, so the tier's 2.0 must stand", cfg.Priority)
	}
}

func TestDefaultPolicyAppliesWhenTheVariantNamesNone(t *testing.T) {
	cfgMap := policyConfig()
	base := cfgMap["default"]
	base.DefaultPolicy = "batch"
	cfgMap["default"] = base

	cfg := resolveScalingPolicyForTier(cfgMap, "m", "ns", "")
	if cfg.ScaleUpThreshold != 0.95 {
		t.Errorf("scaleUpThreshold = %v, want the defaultPolicy tier's 0.95", cfg.ScaleUpThreshold)
	}

	// An explicitly named tier still wins over the fleet-wide fallback.
	cfg = resolveScalingPolicyForTier(cfgMap, "m", "ns", "interactive")
	if cfg.ScaleUpThreshold != 0.60 {
		t.Errorf("scaleUpThreshold = %v, want the named tier's 0.60", cfg.ScaleUpThreshold)
	}
}

// A misspelled tier must fall back rather than fail: refusing to scale a workload
// because its policy name has a typo would turn a config error into an outage.
// The engine reports it separately (reportUnknownPolicy) so it is not silent.
func TestUnknownPolicyFallsBackToTheDefaultEntry(t *testing.T) {
	cfg := resolveScalingPolicyForTier(policyConfig(), "m", "ns", "interctive")
	if cfg.ScaleUpThreshold != 0.85 {
		t.Errorf("scaleUpThreshold = %v, want the default entry's 0.85", cfg.ScaleUpThreshold)
	}
}

// A per-model override key must never be reachable as a policy tier, or a model's
// private settings could be adopted by any other model naming that key.
func TestAnOverrideKeyIsNotAPolicyTier(t *testing.T) {
	cfgMap := policyConfig()
	overrideKey := config.ModelOverrideKey("secret-model", "secret-ns")
	cfgMap[overrideKey] = config.ScalingPolicy{ScaleUpThreshold: 0.10}

	cfg := resolveScalingPolicyForTier(cfgMap, "other", "other-ns", overrideKey)
	if cfg.ScaleUpThreshold != 0.85 {
		t.Errorf("scaleUpThreshold = %v; a %q key must not resolve as a tier", cfg.ScaleUpThreshold, "modelID#namespace")
	}
}

// WVA scales a MODEL: the optimizer distributes its replicas across its variants
// against one set of thresholds. Variants naming different tiers therefore have no
// correct merge, so the choice must at least be deterministic — an allocation that
// flips tier as map order changes is worse than either tier.
func TestModelPolicyIsDeterministicWhenVariantsDisagree(t *testing.T) {
	vas := []wvav1alpha1.VariantAutoscaling{
		{Spec: wvav1alpha1.VariantAutoscalingSpec{ScalingPolicy: "interactive"}},
		{Spec: wvav1alpha1.VariantAutoscalingSpec{ScalingPolicy: "batch"}},
	}
	chosen, conflicting := modelPolicy(vas)
	if chosen != "batch" {
		t.Errorf("chosen = %q, want the lexicographically smallest (%q) for determinism", chosen, "batch")
	}
	if len(conflicting) != 2 {
		t.Errorf("conflicting = %v, want both tiers reported", conflicting)
	}

	// Reversing the input must not change the outcome.
	reversed, _ := modelPolicy([]wvav1alpha1.VariantAutoscaling{vas[1], vas[0]})
	if reversed != chosen {
		t.Errorf("input order changed the resolved policy: %q vs %q", reversed, chosen)
	}
}

func TestModelPolicyAgreementIsNotAConflict(t *testing.T) {
	vas := []wvav1alpha1.VariantAutoscaling{
		{Spec: wvav1alpha1.VariantAutoscalingSpec{ScalingPolicy: "interactive"}},
		{Spec: wvav1alpha1.VariantAutoscalingSpec{ScalingPolicy: "interactive"}},
		{Spec: wvav1alpha1.VariantAutoscalingSpec{}}, // names none: inherits, not a disagreement
	}
	chosen, conflicting := modelPolicy(vas)
	if chosen != "interactive" {
		t.Errorf("chosen = %q, want interactive", chosen)
	}
	if len(conflicting) > 1 {
		t.Errorf("conflicting = %v, want no conflict when the named tiers agree", conflicting)
	}
}
