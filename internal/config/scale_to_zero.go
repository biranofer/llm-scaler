package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Scale-to-zero configuration constants
const (
	// DefaultScaleToZeroRetentionPeriod is the default time to wait after the last request
	// before scaling down to zero replicas. This default applies when scale-to-zero is enabled
	// but no explicit retention period is specified.
	DefaultScaleToZeroRetentionPeriod = 10 * time.Minute

	// GlobalDefaultsKey is the "default" entry key shared by the scaling-policy
	// ConfigMap: the entry every model inherits before its own
	// "{modelID}#{namespace}" override is merged over it.
	GlobalDefaultsKey = "default"
)

// ResolveScaleToZeroEnabled reports whether scale-to-zero is enabled for the
// resolved scaling entry.
//
// The entry is the ONLY per-model surface: it arrives already resolved
// namespace-local → global and merged with its "{modelID}#{namespace}" override,
// so tiering and per-model precedence are settled before this is called. A
// separate wva-model-scale-to-zero-config ConfigMap used to carry the same
// setting, which meant three places to look and a precedence rule to remember.
//
// An absent inline value falls back to WVA_SCALE_TO_ZERO, which stays as the
// DEPLOYMENT-level switch: it answers "may this cluster scale anything to zero at
// all", not "should this model".
func ResolveScaleToZeroEnabled(sat *ScalingPolicy) bool {
	if sat != nil && sat.ScaleToZero != nil && sat.ScaleToZero.Enabled != nil {
		return *sat.ScaleToZero.Enabled
	}
	return strings.EqualFold(os.Getenv("WVA_SCALE_TO_ZERO"), "true")
}

// ResolveScaleToZeroRetention returns how long a model must be idle before it is
// scaled to zero, and how long a freshly woken one is held.
//
// An unparseable value is reported and falls back to the system default rather
// than failing the cycle: a typo in a duration must not stop scaling decisions,
// and silently treating it as zero would scale a model down the instant it went
// idle.
func ResolveScaleToZeroRetention(sat *ScalingPolicy) time.Duration {
	if sat == nil || sat.ScaleToZero == nil || sat.ScaleToZero.RetentionPeriod == "" {
		return DefaultScaleToZeroRetentionPeriod
	}
	duration, err := ValidateRetentionPeriod(sat.ScaleToZero.RetentionPeriod)
	if err != nil {
		ctrl.Log.Info("Invalid scaleToZero.retentionPeriod, using the system default",
			"retentionPeriod", sat.ScaleToZero.RetentionPeriod,
			"default", DefaultScaleToZeroRetentionPeriod,
			"error", err)
		return DefaultScaleToZeroRetentionPeriod
	}
	return duration
}

// ValidateRetentionPeriod validates a retention period string.
// Returns the parsed duration and an error if validation fails.
func ValidateRetentionPeriod(retentionPeriod string) (time.Duration, error) {
	if retentionPeriod == "" {
		return 0, errors.New("retention period cannot be empty")
	}

	duration, err := time.ParseDuration(retentionPeriod)
	if err != nil {
		return 0, fmt.Errorf("invalid duration format: %w", err)
	}

	if duration <= 0 {
		return 0, fmt.Errorf("retention period must be positive, got %v", duration)
	}

	// Warn if retention period is unusually long (> 24 hours)
	if duration > 24*time.Hour {
		ctrl.Log.Info("Retention period is unusually long",
			"retentionPeriod", retentionPeriod,
			"duration", duration,
			"recommendation", "Consider using a shorter period for better resource utilization")
	}

	return duration, nil
}
