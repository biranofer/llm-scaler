package config

import (
	"maps"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Config is the unified configuration structure for the WVA controller.
// All fields are private and accessed via thread-safe getter methods.
type Config struct {
	mu         sync.RWMutex // Single mutex for all mutable fields
	configSync configSyncState

	infrastructure infrastructureConfig
	tls            tlsConfig
	prometheus     prometheusConfig
	// epp            eppConfig
	features   featureFlagsConfig
	saturation scalingPolicyConfig // namespace-aware
}

// LimiterType selects which pipeline.Limiter implementation
// pipeline.NewLimiterFromConfig builds at startup. The two values are
// mutually exclusive in the initial implementation — composing physical and
// quota bounds (min(physical, quota)) is the limiter chain's job, tracked in
// issue #1003.
type LimiterType string

const (
	// LimiterTypeNone means no limiter is configured: the limiters list is absent
	// or empty. Scaling is then unconstrained — the optimizer allocates without a
	// GPU budget and a scale-from-zero wake is published without a capacity check.
	//
	// This is the ScalingPolicy schema's own semantics, where limiters is "zero or
	// more" (docs/proposals/design-scalingpolicy-crd.md §3). Declaring a limiter is
	// what turns limiting on; there is no separate enable flag, and no implicit
	// default limiter for an operator who declared none.
	LimiterTypeNone LimiterType = "none"

	// LimiterTypeInventory builds the TypeInventory-backed limiter
	// (physical GPU discovery via the GPU operator).
	LimiterTypeInventory LimiterType = "inventory"

	// LimiterTypeQuota builds one or more QuotaInventory-backed limiters
	// from an operator-supplied YAML file. Multiple entries are wrapped in
	// a CompositeLimiter that applies them sequentially.
	LimiterTypeQuota LimiterType = "quota"
)

// configSyncState tracks configuration sync state used for startup/readiness checks.
type configSyncState struct {
	configMapsBootstrapComplete bool
	lastConfigMapsSyncAt        time.Time
	lastConfigMapsSyncError     string
}

// infrastructureConfig holds server/controller infrastructure settings
type infrastructureConfig struct {
	metricsAddr          string
	probeAddr            string
	enableLeaderElection bool
	leaderElectionID     string
	leaseDuration        time.Duration
	renewDeadline        time.Duration
	retryPeriod          time.Duration
	restTimeout          time.Duration
	secureMetrics        bool
	watchNamespace       string
	loggerVerbosity      int
	optimizationInterval time.Duration
}

// tlsConfig holds TLS certificate paths
type tlsConfig struct {
	webhookCertPath string
	webhookCertName string
	webhookCertKey  string
	metricsCertPath string
	metricsCertName string
	metricsCertKey  string
}

// // eppConfig holds EPP (Endpoint Pool) integration configuration
// type eppConfig struct {
// 	// Reserved for future EPP-specific configuration
// }

// featureFlagsConfig holds feature flags
type featureFlagsConfig struct {
	scaleToZeroEnabled          bool
	limitedModeEnabled          bool
	scaleFromZeroMaxConcurrency int
}

// ScalingPolicySet represents saturation scaling configuration
// for all models. Maps model ID (or "default" key) to its configuration.
type ScalingPolicySet map[string]ScalingPolicy

// scalingPolicyConfig holds saturation scaling configuration (namespace-aware)
type scalingPolicyConfig struct {
	// Global default configuration
	global ScalingPolicySet

	// Namespace-local configuration overrides (keyed by namespace name)
	namespaceConfigs map[string]ScalingPolicySet
}

// // StaticConfig holds configuration that is immutable after startup.
// // These settings are loaded once at startup and cannot be changed at runtime.
// // EPPConfig holds EPP (Endpoint Pool) integration configuration.
// type EPPConfig struct {
// 	// Reserved for future EPP-specific static configuration
// }

// ============================================================================
// Infrastructure Getters (thread-safe)
// ============================================================================

// MetricsAddr returns the metrics bind address.
// Thread-safe.
func (c *Config) MetricsAddr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.metricsAddr
}

// ProbeAddr returns the health probe bind address.
// Thread-safe.
func (c *Config) ProbeAddr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.probeAddr
}

// EnableLeaderElection returns whether leader election is enabled.
// Thread-safe.
func (c *Config) EnableLeaderElection() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.enableLeaderElection
}

// LeaderElectionID returns the leader election ID.
// Thread-safe.
func (c *Config) LeaderElectionID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.leaderElectionID
}

// LeaseDuration returns the leader election lease duration.
// Thread-safe.
func (c *Config) LeaseDuration() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.leaseDuration
}

// RenewDeadline returns the leader election renew deadline.
// Thread-safe.
func (c *Config) RenewDeadline() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.renewDeadline
}

// RetryPeriod returns the leader election retry period.
// Thread-safe.
func (c *Config) RetryPeriod() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.retryPeriod
}

// RestTimeout returns the REST client timeout.
// Thread-safe.
func (c *Config) RestTimeout() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.restTimeout
}

// SecureMetrics returns whether metrics endpoint uses HTTPS.
// Thread-safe.
func (c *Config) SecureMetrics() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.secureMetrics
}

// WatchNamespace returns the namespace to watch (empty = all namespaces).
// Thread-safe.
func (c *Config) WatchNamespace() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.watchNamespace
}

// LoggerVerbosity returns the logger verbosity level.
// Thread-safe.
func (c *Config) LoggerVerbosity() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.loggerVerbosity
}

// ============================================================================
// TLS Getters (thread-safe)
// ============================================================================

// WebhookCertPath returns the webhook certificate path.
// Thread-safe.
func (c *Config) WebhookCertPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tls.webhookCertPath
}

// WebhookCertName returns the webhook certificate name.
// Thread-safe.
func (c *Config) WebhookCertName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tls.webhookCertName
}

// WebhookCertKey returns the webhook certificate key.
// Thread-safe.
func (c *Config) WebhookCertKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tls.webhookCertKey
}

// MetricsCertPath returns the metrics certificate path.
// Thread-safe.
func (c *Config) MetricsCertPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tls.metricsCertPath
}

// MetricsCertName returns the metrics certificate name.
// Thread-safe.
func (c *Config) MetricsCertName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tls.metricsCertName
}

// MetricsCertKey returns the metrics certificate key.
// Thread-safe.
func (c *Config) MetricsCertKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tls.metricsCertKey
}

// ============================================================================
// Optimization Getters (thread-safe)
// ============================================================================

// OptimizationInterval returns the current optimization interval.
// Thread-safe.
func (c *Config) OptimizationInterval() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.infrastructure.optimizationInterval
}

// ============================================================================
// Feature Flags Getters (thread-safe)
// ============================================================================

// ScaleToZeroEnabled returns whether scale-to-zero is enabled.
// Thread-safe.
func (c *Config) ScaleToZeroEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.features.scaleToZeroEnabled
}

// LimitedModeEnabled returns whether limited mode is enabled.
// Thread-safe.
func (c *Config) LimitedModeEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.features.limitedModeEnabled
}

// throughputAnalyzerName is the analyzer name used by the throughput
// analyzer's saturation-config entry. Duplicated as a literal (rather than
// importing internal/engines/analyzers/throughput.AnalyzerName) because
// internal/config is a lower layer than the analyzers package.
const throughputAnalyzerName = "throughput"

// ThroughputAnalyzerEnabled reports whether any saturation config entry lists
// the throughput analyzer with Enabled nil-or-true. Startup-time gate: when
// no entry enables throughput anywhere, the analyzer is never registered, so
// it cannot participate in scaling decisions and cannot veto scale-down.
//
// This is independent of the per-cycle effectiveEnabled opt-in check in the
// saturation engine, which governs participation per namespace/model once
// the analyzer is registered. Runtime enablement after controller start
// requires a restart because RegisterAnalyzer is frozen after
// StartOptimizeLoop (cmd/main.go); the ConfigMapReconciler uses this method
// to detect that kind of live-config divergence from the frozen decision.
// Thread-safe (ScalingPolicyConfig acquires its own lock).
func (c *Config) ThroughputAnalyzerEnabled() bool {
	for _, sc := range c.ScalingPolicyConfig() {
		for _, aw := range sc.Analyzers {
			if aw.EffectiveType() == throughputAnalyzerName && (aw.Enabled == nil || *aw.Enabled) {
				return true
			}
		}
	}
	return false
}

// ScaleFromZeroMaxConcurrency returns the scale-from-zero max concurrency.
// Thread-safe.
func (c *Config) ScaleFromZeroMaxConcurrency() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.features.scaleFromZeroMaxConcurrency
}

// ScalingPolicyConfig returns the current global saturation scaling configuration.
// Thread-safe. Returns a copy to prevent external modifications.
// For namespace-aware lookups, use ScalingPolicyConfigForNamespace instead.
func (c *Config) ScalingPolicyConfig() map[string]ScalingPolicy {
	return c.ScalingPolicyConfigForNamespace("")
}

// resolveScalingPolicy resolves saturation config for a namespace (namespace-local > global).
// Must be called while holding at least a read lock.
func (c *Config) resolveScalingPolicy(namespace string) map[string]ScalingPolicy {
	// Check namespace-local first (if namespace is provided)
	if namespace != "" {
		if nsConfig, exists := c.saturation.namespaceConfigs[namespace]; exists {
			if len(nsConfig) > 0 {
				return nsConfig
			}
		}
	}

	// Fall back to global
	if len(c.saturation.global) > 0 {
		return c.saturation.global
	}

	return nil
}

// ScalingPolicyConfigForNamespace returns the saturation scaling configuration for the given namespace.
// Resolution order: namespace-local > global
// Thread-safe. Returns a copy to prevent external modifications.
// If namespace is empty, returns global config.
func (c *Config) ScalingPolicyConfigForNamespace(namespace string) map[string]ScalingPolicy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sourceConfig := c.resolveScalingPolicy(namespace)
	return copyScalingPolicyConfig(sourceConfig)
}

// RescaleEnabledForNamespaceLocal reports the EnableRescale flag from a namespace's
// OWN saturation config `default` entry, and whether that namespace has its own
// (non-global) config. It intentionally does NOT fall back to the global config: the
// scope-coupled rescale flag for a namespace quota is governed only by that
// namespace's own config, so the cluster (global) flag never leaks onto it.
func (c *Config) RescaleEnabledForNamespaceLocal(namespace string) (enabled bool, hasLocal bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if namespace == "" {
		return false, false
	}
	nsCfg, ok := c.saturation.namespaceConfigs[namespace]
	if !ok || len(nsCfg) == 0 {
		return false, false
	}
	return nsCfg["default"].EnableRescale, true
}

// RescaleEnabledCluster reports the EnableRescale flag from the global saturation
// config `default` entry — the cluster-budget scope.
func (c *Config) RescaleEnabledCluster() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.saturation.global["default"].EnableRescale
}

// copyScalingPolicyConfig creates a deep copy of the saturation config map.
func copyScalingPolicyConfig(src map[string]ScalingPolicy) map[string]ScalingPolicy {
	if src == nil {
		return make(map[string]ScalingPolicy)
	}
	result := make(map[string]ScalingPolicy, len(src))
	for k, v := range src {
		result[k] = v
	}
	return result
}

// UpdateScalingPolicyConfig updates the global saturation scaling configuration.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
// For namespace-local updates, use UpdateScalingPolicyConfigForNamespace instead.
func (c *Config) UpdateScalingPolicyConfig(config map[string]ScalingPolicy) {
	c.UpdateScalingPolicyConfigForNamespace("", config)
}

// UpdateScalingPolicyConfigForNamespace updates the saturation scaling configuration for the given namespace.
// If namespace is empty, updates global config.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
func (c *Config) UpdateScalingPolicyConfigForNamespace(namespace string, config map[string]ScalingPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Make a copy to prevent external modifications
	newConfig := make(map[string]ScalingPolicy, len(config))
	maps.Copy(newConfig, config)

	var oldCount int
	if namespace == "" {
		// Update global
		oldCount = len(c.saturation.global)
		c.saturation.global = newConfig
		newCount := len(c.saturation.global)
		if oldCount != newCount {
			ctrl.Log.Info("Updated global saturation config", "oldEntries", oldCount, "newEntries", newCount)
		}
	} else {
		// Update namespace-local
		if c.saturation.namespaceConfigs == nil {
			c.saturation.namespaceConfigs = make(map[string]ScalingPolicySet)
		}
		oldCount = len(c.saturation.namespaceConfigs[namespace])
		c.saturation.namespaceConfigs[namespace] = newConfig
		newCount := len(c.saturation.namespaceConfigs[namespace])
		if oldCount != newCount {
			ctrl.Log.Info("Updated namespace-local saturation config", "namespace", namespace, "oldEntries", oldCount, "newEntries", newCount)
		}
	}

}

// RemoveNamespaceConfig removes the namespace-local configuration for the given namespace.
// This is called when a namespace-local ConfigMap is deleted, allowing fallback to global config.
// Thread-safe.
func (c *Config) RemoveNamespaceConfig(namespace string) {
	if namespace == "" {
		return // Don't remove global config
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := false
	if c.saturation.namespaceConfigs != nil {
		if _, exists := c.saturation.namespaceConfigs[namespace]; exists {
			delete(c.saturation.namespaceConfigs, namespace)
			removed = true
		}
	}
	if removed {
		ctrl.Log.Info("Removed namespace-local config", "namespace", namespace)
	}
}

// UpdatePrometheusCacheConfig updates the Prometheus cache configuration.
// Thread-safe.
func (c *Config) UpdatePrometheusCacheConfig(cacheConfig *CacheConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cacheConfig == nil {
		c.prometheus.cache = nil
	} else {
		// Make a copy
		cp := *cacheConfig
		c.prometheus.cache = &cp
	}
}

// NewTestConfig creates a minimal Config for testing purposes.
// It provides sensible defaults for all required fields.
// This helper is intended for use in unit tests, integration tests, and e2e tests
// where a valid Config instance is needed but full configuration is not required.
// NOTE: This function is exported for testing purposes only and should not be used in production code.
func NewTestConfig() *Config {
	cfg := &Config{
		infrastructure: infrastructureConfig{
			metricsAddr:          "0",
			probeAddr:            ":8081",
			enableLeaderElection: false,
			leaderElectionID:     "test-election-id",
			leaseDuration:        60 * time.Second,
			renewDeadline:        50 * time.Second,
			retryPeriod:          10 * time.Second,
			restTimeout:          60 * time.Second,
			secureMetrics:        false,
			watchNamespace:       "",
			loggerVerbosity:      0,
			optimizationInterval: 15 * time.Second,
		},
		tls: tlsConfig{
			webhookCertName: "tls.crt",
			webhookCertKey:  "tls.key",
			metricsCertName: "tls.crt",
			metricsCertKey:  "tls.key",
		},
		features: featureFlagsConfig{
			scaleToZeroEnabled:          false,
			limitedModeEnabled:          false,
			scaleFromZeroMaxConcurrency: 10,
		},
		saturation: scalingPolicyConfig{
			global:           make(ScalingPolicySet),
			namespaceConfigs: make(map[string]ScalingPolicySet),
		},
	}
	return cfg
}

// setPrometheusBaseURLForTesting sets the Prometheus base URL for testing purposes only.
// This is internal and can only be used by tests in the config package.
//
//nolint:unused // Used by tests in config_test.go
func (c *Config) setPrometheusBaseURLForTesting(baseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prometheus.baseURL = baseURL
}

// SetOptimizationIntervalForTest overrides the optimization interval on a test
// Config, including to a non-positive value — Load() always sanitizes it to at
// least MinOptimizationInterval, so this is the only way to exercise a caller's
// handling of a Config that (however it got there) reports a non-positive
// interval. Not for production use.
func SetOptimizationIntervalForTest(c *Config, interval time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.infrastructure.optimizationInterval = interval
}

// --- Bootstrap State Management ---

// ConfigMapsBootstrapComplete returns true once the initial ConfigMap bootstrap has completed.
// Thread-safe.
func (c *Config) ConfigMapsBootstrapComplete() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.configSync.configMapsBootstrapComplete
}

// ConfigMapsBootstrapSyncStatus returns the bootstrap state for ConfigMap synchronization.
// Thread-safe.
func (c *Config) ConfigMapsBootstrapSyncStatus() (bool, time.Time, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.configSync.configMapsBootstrapComplete, c.configSync.lastConfigMapsSyncAt, c.configSync.lastConfigMapsSyncError
}

// MarkConfigMapsBootstrapComplete marks initial ConfigMap bootstrap as completed successfully.
// Thread-safe.
func (c *Config) MarkConfigMapsBootstrapComplete() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configSync.configMapsBootstrapComplete = true
	c.configSync.lastConfigMapsSyncAt = time.Now()
	c.configSync.lastConfigMapsSyncError = ""
}

// MarkConfigMapsBootstrapFailed marks initial ConfigMap bootstrap as failed.
// Thread-safe.
func (c *Config) MarkConfigMapsBootstrapFailed(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configSync.configMapsBootstrapComplete = false
	c.configSync.lastConfigMapsSyncAt = time.Now()
	if err != nil {
		c.configSync.lastConfigMapsSyncError = err.Error()
		return
	}
	c.configSync.lastConfigMapsSyncError = ""
}

// EffectiveLimiterMode returns the GPU limiter implementation to construct. The
// limiters: list on the global saturation "default" entry is the SOLE source: no
// entries selects LimiterTypeNone, a quota entry selects LimiterTypeQuota, and
// otherwise a gpu-inventory/inventory entry selects LimiterTypeInventory.
//
// The list carries the whole intent — declaring a limiter is what turns limiting
// on. There is no separate enable flag: enableLimiter used to gate the saturation
// optimizer independently, so quota entries could be declared and silently not
// enforced, while the scale-from-zero path consulted a limiter the operator never
// asked for. Both halves now follow this one value.
//
// Because it reads the live saturation config, the value changes without a
// restart when the ConfigMap changes.
// Thread-safe.
func (c *Config) EffectiveLimiterMode() LimiterType {
	c.mu.RLock()
	defer c.mu.RUnlock()
	limiters := c.saturation.global["default"].Limiters
	if len(limiters) == 0 {
		return LimiterTypeNone
	}
	for _, l := range limiters {
		if l.Type == string(LimiterTypeQuota) {
			return LimiterTypeQuota
		}
	}
	return LimiterTypeInventory
}

// EffectiveQuotaEntries returns the quota entries the quota limiter should
// enforce, taken from the inline quota entries on the global saturation "default"
// entry (each deep-copied). Empty when none are declared. Reads the live config,
// so changes take effect without a restart.
// Thread-safe.
func (c *Config) EffectiveQuotaEntries() []QuotaLimiterConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var inline []QuotaLimiterConfig
	for _, l := range c.saturation.global["default"].Limiters {
		if l.Type == string(LimiterTypeQuota) {
			inline = append(inline, l.clone())
		}
	}
	return inline
}
