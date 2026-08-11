package config

import (
	"os"
	"strconv"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// DefaultConfigMapName is the default name of the ConfigMap containing autoscaler configuration
	DefaultConfigMapName = "wva-manager-config"
	// DefaultScalingPolicyConfigMapName is the default name of the ConfigMap
	// carrying the scaling policy: the cluster default entry, named policy tiers,
	// analyzer definitions, limiters and scale-to-zero.
	DefaultScalingPolicyConfigMapName = "wva-scaling-policy-config"

	// LegacyScalingPolicyConfigMapName is the name this ConfigMap had while it held
	// only saturation thresholds. Still read, so an existing deployment does not
	// silently lose its whole scaling configuration on upgrade — see
	// ScalingPolicyConfigMapName.
	LegacyScalingPolicyConfigMapName = "wva-saturation-scaling-config"
	// DefaultNamespace is the default namespace for the controller
	DefaultNamespace = "workload-variant-autoscaler-system"

	// WellKnownPolicyNamespace is the namespace every WVA on a cluster reads
	// cluster policy from when it exists, WHATEVER any individual deployment is
	// configured to do.
	//
	// It is a fixed name on purpose. The configurable form above is carried on the
	// controller's Deployment, and a Deployment binds nobody who can edit it — a
	// tenant running their own namespace-scoped WVA owns that Deployment and could
	// point it at a namespace of their own making, or unset it and fall back to
	// their own ConfigMap. A name that cannot be configured cannot be redirected.
	//
	// So a cluster admin gets one action that binds every WVA on the cluster,
	// including tenant-owned ones, without editing a single tenant Deployment:
	// create this namespace, put the limiters in it, and grant each controller
	// read. Precedence is deliberately "well-known wins" rather than "configured
	// wins" — the whole value is that a tenant cannot opt out.
	//
	// This raises the floor; it is not a sandbox. A tenant who can replace the
	// controller's IMAGE runs whatever code they like, and no in-process rule
	// survives that. Enforcement against an untrusted tenant is a Kubernetes
	// ResourceQuota on their namespace; this makes the GPU budget authoritative
	// for every WVA that is actually running WVA.
	WellKnownPolicyNamespace = "wva-policy"
)

// ConfigValue retrieves a value from a ConfigMap with a default fallback
func ConfigValue(data map[string]string, key, def string) string {
	if v, ok := data[key]; ok {
		return v
	}
	return def
}

// ParseDurationFromConfig parses a duration string from ConfigMap with default fallback
// Returns the parsed duration or the default value if parsing fails or key is missing
func ParseDurationFromConfig(data map[string]string, key string, defaultValue time.Duration) time.Duration {
	if valStr := ConfigValue(data, key, ""); valStr != "" {
		if val, err := time.ParseDuration(valStr); err == nil {
			return val
		}
		ctrl.Log.Info("Invalid duration value in ConfigMap, using default", "value", valStr, "key", key, "default", defaultValue)
	}
	return defaultValue
}

// ParseIntFromConfig parses an integer from ConfigMap with default fallback and minimum value validation
// Returns the parsed integer or the default value if parsing fails, is less than minValue, or key is missing
func ParseIntFromConfig(data map[string]string, key string, defaultValue int, minValue int) int {
	if valStr := ConfigValue(data, key, ""); valStr != "" {
		if val, err := strconv.Atoi(valStr); err == nil && val >= minValue {
			return val
		}
		ctrl.Log.Info("Invalid integer value in ConfigMap, using default", "value", valStr, "key", key, "minValue", minValue, "default", defaultValue)
	}
	return defaultValue
}

// ParseBoolFromConfig parses a boolean from ConfigMap with default fallback
// Accepts "true", "1", or "yes" as true values (case-sensitive)
// Returns the parsed boolean or the default value if key is missing or value is not recognized
func ParseBoolFromConfig(data map[string]string, key string, defaultValue bool) bool {
	if valStr := ConfigValue(data, key, ""); valStr != "" {
		// Accept "true", "1", "yes" as true
		return valStr == "true" || valStr == "1" || valStr == "yes"
	}
	return defaultValue
}

// SystemNamespace returns the controller's system namespace where WVA is deployed.
// This is the namespace containing global ConfigMaps and the controller deployment.
//
// Returns: POD_NAMESPACE environment variable if set, otherwise "workload-variant-autoscaler-system"
func SystemNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return DefaultNamespace
}

// PolicySource records how the policy namespace was chosen, so an admin can tell
// a bound they imposed from one a tenant chose for themselves.
type PolicySource string

const (
	// PolicySourceWellKnown means WellKnownPolicyNamespace exists and won. The
	// deployment could not have opted out of this.
	PolicySourceWellKnown PolicySource = "well-known-namespace"
	// PolicySourceLocal means policy comes from the controller's own namespace —
	// the default, and correct when that namespace belongs to an admin.
	PolicySourceLocal PolicySource = "controller-namespace"
)

// ConfigMapName returns the main ConfigMap name from environment variable or default (DefaultConfigMapName).
// Set CONFIG_MAP_NAME in the deployment manifest to use a non-default name.
func ConfigMapName() string {
	if name := os.Getenv("CONFIG_MAP_NAME"); name != "" {
		return name
	}
	return DefaultConfigMapName
}

// ScalingPolicyConfigMapName returns the scaling-policy ConfigMap name, from the
// environment or the default.
//
// SCALING_POLICY_CONFIG_MAP_NAME overrides it; SATURATION_CONFIG_MAP_NAME is the
// name that override had while the ConfigMap held only saturation thresholds, and
// is still honored so an existing deployment keeps working.
func ScalingPolicyConfigMapName() string {
	if name := os.Getenv("SCALING_POLICY_CONFIG_MAP_NAME"); name != "" {
		return name
	}
	if name := os.Getenv("SATURATION_CONFIG_MAP_NAME"); name != "" {
		return name
	}
	return DefaultScalingPolicyConfigMapName
}

// ScalingPolicyConfigMapNames returns the ConfigMap names to read scaling policy
// from, current name first.
//
// The rename is not a clean cutover: the ConfigMap outgrew "saturation" — it now
// carries policy tiers, analyzer definitions, limiters and scale-to-zero, none of
// which are saturation — but an operator who has not yet renamed theirs must not
// lose every scaling setting they have on upgrade. So both are read, the current
// name wins, and using the old one is reported once at startup.
func ScalingPolicyConfigMapNames() []string {
	current := ScalingPolicyConfigMapName()
	if current == LegacyScalingPolicyConfigMapName {
		return []string{current}
	}
	return []string{current, LegacyScalingPolicyConfigMapName}
}
