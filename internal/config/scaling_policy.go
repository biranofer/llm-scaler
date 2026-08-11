package config

import "strings"

// Named scaling policies: reusable tiers, selected per variant by the
// `scalingPolicy` key in its scaler's trigger metadata.
//
// A policy is a SCALING TIER — "interactive", "standard", "batch" — carrying
// thresholds, analyzer selection and scale-to-zero settings. It is deliberately
// IDENTITY-FREE: no model, no namespace. That is the whole point. Per-(model,
// namespace) entries bind settings to identity, so "loosen the thresholds for
// everything interactive" is an edit per model; a tier makes it one edit, and the
// set of models in the tier is declared by the models themselves.
//
// Policies live in the same ConfigMap as the default entry and per-model
// overrides, distinguished by their key:
//
//	default                    the cluster default entry
//	{modelID}#{namespace}      a per-model override  (contains "#")
//	anything else              a named policy tier
//
// The "#" is what makes this unambiguous, and it is already the override key's
// shape — a policy name is a tier name, which has no reason to contain one.
//
// See docs/proposals/wva-keda-external-scaler.md §7.5.

// PolicyEntryKey reports whether a ConfigMap entry key names a policy tier,
// rather than the default entry or a per-model override.
func PolicyEntryKey(key string) bool {
	return key != GlobalDefaultsKey && !strings.Contains(key, modelOverrideKeySeparator)
}

// ModelOverrideKey builds the per-model override entry key for a model in a
// namespace. One place, so the readers and the writers cannot drift.
func ModelOverrideKey(modelID, namespace string) string {
	return modelID + modelOverrideKeySeparator + namespace
}

// modelOverrideKeySeparator joins modelID and namespace in an override entry key.
const modelOverrideKeySeparator = "#"

// ScalingPolicies returns the named policy tiers declared in a resolved config
// map, keyed by tier name.
func NamedPolicies(configMap map[string]ScalingPolicy) map[string]ScalingPolicy {
	policies := make(map[string]ScalingPolicy)
	for key, entry := range configMap {
		if PolicyEntryKey(key) {
			policies[key] = entry
		}
	}
	return policies
}

// ResolveScalingPolicy resolves config for a model.
// Starts from the "default" entry (or zero-value), then merges the model-specific
// override "{modelID}#{namespace}" on top (if present). This allows per-model
// overrides to specify only the fields they want to change.
// After merging, ApplyDefaults fills remaining zero-valued fields, then
// ApplyV2ThresholdDefaults calibrates the V2 thresholds on the final config and an
// inconsistent (inverted) threshold pair is reset to the defaults.
func ResolveScalingPolicy(
	configMap map[string]ScalingPolicy,
	modelID, namespace string,
) ScalingPolicy {
	return ResolveScalingPolicyForTier(configMap, modelID, namespace, "")
}

// ResolveScalingPolicyForTier resolves the effective entry, layering the
// named policy tier the variant selected between the cluster default and any
// per-model override:
//
//	default entry            fleet-wide fallback
//	  â–¼ overridden by
//	named policy tier        the variant's scalingPolicy, or defaultPolicy
//	  â–¼ overridden by
//	{modelID}#{namespace}    per-model override, most specific
//
// Most specific wins, per docs/proposals/wva-keda-external-scaler.md Â§7.5. The
// per-model override stays the innermost layer so existing configs keep working
// while tiers are adopted: a fleet moves to policies model by model, not all at
// once.
//
// An unknown policy name resolves to the default entry â€” the same outcome as
// naming none, which is the right behaviour but a bad silence, so the caller
// reports it (see policyResolution).
func ResolveScalingPolicyForTier(
	configMap map[string]ScalingPolicy,
	modelID, namespace, policy string,
) ScalingPolicy {
	// Start with default config as base
	base := ScalingPolicy{}
	if defaultCfg, ok := configMap[GlobalDefaultsKey]; ok {
		base = defaultCfg
	}
	// Overlay the named policy tier, falling back to the cluster's defaultPolicy.
	// Read from the DEFAULT entry only: a fleet-wide fallback chosen by a policy or
	// a per-model entry would be that entry choosing for everyone but itself.
	if policy == "" {
		policy = base.DefaultPolicy
	}
	if policy != "" {
		if tier, ok := configMap[policy]; ok && PolicyEntryKey(policy) {
			base.Merge(tier)
		}
	}
	// Overlay model-specific override if present (non-zero fields win)
	if override, ok := configMap[ModelOverrideKey(modelID, namespace)]; ok {
		base.Merge(override)
	}
	base.ApplyDefaults()
	// Calibrate V2 thresholds on the final merged config. Analyzer selection is
	// global, so this entry may run on the V2 path even when written V1-style;
	// applied here (post-merge) rather than in ApplyDefaults so a V1-style override
	// cannot clobber a tuned global threshold during Merge.
	base.ApplyV2ThresholdDefaults()
	// Per-entry V2 thresholds are range-validated at load, but a merge can still
	// produce an inconsistent pair across entries (e.g. an override raises
	// scaleDownBoundary above the base scaleUpThreshold). Rather than feed the
	// optimizer an inverted pair, fall back to the defaults for both.
	if base.ScaleUpThreshold <= base.ScaleDownBoundary {
		base.ScaleUpThreshold = DefaultScaleUpThreshold
		base.ScaleDownBoundary = DefaultScaleDownBoundary
	}
	return base
}
