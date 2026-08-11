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
