package config

import "testing"

// Named policies are reusable TIERS, selected per variant by trigger metadata.
// The point is that they carry no model identity: one tier serves many models, so
// retuning a tier is one edit rather than an edit per model. These pin the
// layering from docs/proposals/wva-keda-external-scaler.md §7.5 —
//
//	default entry → named policy tier → {modelID}#{namespace} override
//
// most specific winning at each step.
func policyConfig() map[string]ScalingPolicy {
	return map[string]ScalingPolicy{
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
	cfg := ResolveScalingPolicyForTier(policyConfig(), "m", "ns", "interactive")

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
	a := ResolveScalingPolicyForTier(cfgMap, "model-a", "ns-1", "batch")
	b := ResolveScalingPolicyForTier(cfgMap, "model-b", "ns-2", "batch")

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
	cfgMap[ModelOverrideKey("m", "ns")] = ScalingPolicy{
		ScaleUpThreshold: 0.50, ScaleDownBoundary: 0.35,
	}

	cfg := ResolveScalingPolicyForTier(cfgMap, "m", "ns", "interactive")
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

	cfg := ResolveScalingPolicyForTier(cfgMap, "m", "ns", "")
	if cfg.ScaleUpThreshold != 0.95 {
		t.Errorf("scaleUpThreshold = %v, want the defaultPolicy tier's 0.95", cfg.ScaleUpThreshold)
	}

	// An explicitly named tier still wins over the fleet-wide fallback.
	cfg = ResolveScalingPolicyForTier(cfgMap, "m", "ns", "interactive")
	if cfg.ScaleUpThreshold != 0.60 {
		t.Errorf("scaleUpThreshold = %v, want the named tier's 0.60", cfg.ScaleUpThreshold)
	}
}

// A misspelled tier must fall back rather than fail: refusing to scale a workload
// because its policy name has a typo would turn a config error into an outage.
// The engine reports it separately (reportUnknownPolicy) so it is not silent.
func TestUnknownPolicyFallsBackToTheDefaultEntry(t *testing.T) {
	cfg := ResolveScalingPolicyForTier(policyConfig(), "m", "ns", "interctive")
	if cfg.ScaleUpThreshold != 0.85 {
		t.Errorf("scaleUpThreshold = %v, want the default entry's 0.85", cfg.ScaleUpThreshold)
	}
}

// A per-model override key must never be reachable as a policy tier, or a model's
// private settings could be adopted by any other model naming that key.
func TestAnOverrideKeyIsNotAPolicyTier(t *testing.T) {
	cfgMap := policyConfig()
	overrideKey := ModelOverrideKey("secret-model", "secret-ns")
	cfgMap[overrideKey] = ScalingPolicy{ScaleUpThreshold: 0.10}

	cfg := ResolveScalingPolicyForTier(cfgMap, "other", "other-ns", overrideKey)
	if cfg.ScaleUpThreshold != 0.85 {
		t.Errorf("scaleUpThreshold = %v; a %q key must not resolve as a tier", cfg.ScaleUpThreshold, "modelID#namespace")
	}
}
