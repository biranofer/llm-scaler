/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// ConfigMapReconciler reconciles ConfigMaps to update the unified configuration.
// Its sole responsibility is to keep the Config object synchronized with ConfigMap changes.
type ConfigMapReconciler struct {
	client.Reader
	Scheme    *runtime.Scheme
	Config    *config.Config
	Datastore datastore.Datastore
	Recorder  record.EventRecorder

	// ThroughputRegistered is the throughput-analyzer registration decision
	// frozen at startup (cmd/main.go). Analyzer registration cannot change
	// without a controller restart; the reconciler compares live config against
	// this to warn when a runtime edit would be silently inert.
	ThroughputRegistered bool
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles ConfigMap changes and updates the unified configuration.
func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ConfigMap
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, req.NamespacedName, cm); err != nil {
		if apierrors.IsNotFound(err) {
			// ConfigMap was deleted - handle cleanup
			logger.Info("ConfigMap deleted", "name", req.Name, "namespace", req.Namespace)
			r.handleConfigMapDeletion(ctx, req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		errorType := "Failed to get ConfigMap"
		logger.Error(err, errorType, "name", req.Name, "namespace", req.Namespace)
		metrics.RecordError(constants.ComponentController, errorType)
		return ctrl.Result{}, err
	}

	name := cm.GetName()
	namespace := cm.GetNamespace()
	systemNamespace := config.SystemNamespace()

	// Determine if this is a global or namespace-local ConfigMap
	isGlobal := namespace == systemNamespace
	isNamespaceLocal := !isGlobal && r.shouldWatchNamespaceLocalConfigMap(ctx, namespace)

	// Only process if it's global or namespace-local (tracked)
	if !isGlobal && !isNamespaceLocal {
		logger.V(1).Info("Ignoring ConfigMap from untracked namespace", "name", name, "namespace", namespace)
		return ctrl.Result{}, nil
	}

	// Route to appropriate handler based on ConfigMap name. Both scaling-policy
	// names are accepted — the current one and the pre-rename one — so an operator
	// who has not renamed theirs keeps getting live updates. Using the old name is
	// reported once per change by handleScalingPolicyConfigMap.
	if slices.Contains(config.ScalingPolicyConfigMapNames(), name) {
		r.handleScalingPolicyConfigMap(ctx, cm, namespace, isGlobal)
	} else {
		logger.V(1).Info("Ignoring unrecognized ConfigMap", "name", name, "namespace", namespace)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(ConfigMapPredicate(r.Datastore, r.Config)).
		Complete(r)
}

// handleConfigMapDeletion handles ConfigMap deletion events.
func (r *ConfigMapReconciler) handleConfigMapDeletion(ctx context.Context, name, namespace string) {
	logger := log.FromContext(ctx)
	systemNamespace := config.SystemNamespace()

	// Only handle namespace-local ConfigMap deletions (not global)
	if namespace == systemNamespace {
		return
	}

	// Check if this namespace should be tracked
	if !r.shouldWatchNamespaceLocalConfigMap(ctx, namespace) {
		return
	}

	// Remove namespace-local config on deletion. One kind now — the separate
	// scale-to-zero ConfigMap folded into this one.
	if name == config.ScalingPolicyConfigMapName() {
		r.Config.RemoveNamespaceConfig(namespace)
		logger.Info("Removed namespace-local saturation config on ConfigMap deletion", "namespace", namespace)
	}
}

// shouldWatchNamespaceLocalConfigMap returns true if a namespace-local ConfigMap should be watched.
// In single-namespace mode (--watch-namespace set), it watches all ConfigMaps in the watched namespace.
// In multi-namespace mode, it checks exclusion first (highest priority), then VA-based tracking (automatic), then opt-in label (explicit).
func (r *ConfigMapReconciler) shouldWatchNamespaceLocalConfigMap(ctx context.Context, namespace string) bool {
	// In single-namespace mode, watch all ConfigMaps in the watched namespace
	// Explicit CLI flag overrides annotation/label-based filtering
	if r.Config != nil {
		watchNamespace := r.Config.WatchNamespace()
		if watchNamespace != "" && namespace == watchNamespace {
			return true
		}
	}

	// Multi-namespace mode: Check exclusion first (highest priority - overrides everything)
	if isNamespaceExcluded(ctx, r.Reader, namespace) {
		return false
	}

	// Check VA-based tracking (automatic)
	if r.Datastore != nil && r.Datastore.IsNamespaceTracked(namespace) {
		return true
	}

	// Check label-based opt-in (explicit)
	return isNamespaceConfigEnabled(ctx, r.Reader, namespace)
}

// handleScalingPolicyConfigMap handles updates to the saturation scaling ConfigMap.
// Supports both global and namespace-local ConfigMaps.
func (r *ConfigMapReconciler) handleScalingPolicyConfigMap(ctx context.Context, cm *corev1.ConfigMap, namespace string, isGlobal bool) {
	logger := log.FromContext(ctx)

	if cm.GetName() == config.LegacyScalingPolicyConfigMapName &&
		config.ScalingPolicyConfigMapName() != config.LegacyScalingPolicyConfigMapName {
		logger.Info("Reading scaling policy from the pre-rename ConfigMap name. It outgrew "+
			"\"saturation\" — it now carries policy tiers, analyzer definitions, limiters and "+
			"scale-to-zero — so rename it; the old name is still read but will not be forever.",
			"using", config.LegacyScalingPolicyConfigMapName,
			"rename to", config.ScalingPolicyConfigMapName())
	}

	// Parse scaling policy entries
	configs, count := parseScalingPolicyConfig(cm.Data, logger)

	// Update global or namespace-local config
	if isGlobal {
		r.Config.UpdateScalingPolicyConfig(configs)
		logger.Info("Updated global saturation config from ConfigMap", "entries", count)
		r.warnIfThroughputRegistrationDiverged(logger, cm)
	} else {
		// Limiters and quotas are CLUSTER-scope: EffectiveLimiterMode reads them only
		// from the system namespace's ConfigMap, so a tenant cannot grant themselves
		// capacity by declaring a limiter — or raise a quota by declaring a looser
		// one — in their own namespace. That is the point, but silently dropping the
		// block is the same trap enableLimiter was: the config reads as bounded and
		// nothing is bounding it. Say so.
		for key, satConfig := range configs {
			if len(satConfig.Limiters) > 0 {
				logger.Info("Ignoring limiters in a namespace-local saturation ConfigMap: "+
					"limiters and quotas are cluster-scope and are read only from the "+
					"system namespace's ConfigMap. Move this block there, or it has no effect.",
					"namespace", namespace, "entry", key, "systemNamespace", config.SystemNamespace())
			}
			// Same scope as limiters, for a different reason: analyzer definitions
			// are PromQL run against the shared Prometheus, and the analyzer
			// registry is process-wide, so two namespaces defining the same name
			// would collide. A per-namespace WVA install can still declare its own,
			// because that install's own namespace IS its system namespace.
			if len(satConfig.AnalyzerDefinitions) > 0 {
				logger.Info("Ignoring analyzerDefinitions in a namespace-local saturation ConfigMap: "+
					"analyzer definitions are cluster-scope and are read only from the system "+
					"namespace's ConfigMap. A namespace may still SELECT and weight them in its "+
					"policies.",
					"namespace", namespace, "entry", key, "systemNamespace", config.SystemNamespace())
			}
		}
		r.Config.UpdateScalingPolicyConfigForNamespace(namespace, configs)
		logger.Info("Updated namespace-local saturation config from ConfigMap", "namespace", namespace, "entries", count)
	}
}

// warnIfThroughputRegistrationDiverged compares the live global config's
// throughput-analyzer enablement against the registration decision frozen at
// controller startup. Analyzer registration cannot change without a restart,
// so a divergence means this ConfigMap edit is silently inert; it emits a
// Warning event on cm plus a log line telling the operator to restart.
//
// It runs only after the initial ConfigMap bootstrap has completed. The frozen
// decision (ThroughputRegistered) is captured just after bootstrap returns
// (cmd/main.go); during the synchronous bootstrap pass it is still its zero
// value, so comparing against it would emit a spurious "restart required"
// warning on every healthy startup. The manager — and therefore all runtime
// reconciles — starts only after the decision is frozen, so gating on bootstrap
// completion leaves genuine runtime edits fully covered.
//
// Namespace-local config does not participate: startup registration reads only
// the global saturation config, so a namespace-local edit can never cause this
// kind of drift.
func (r *ConfigMapReconciler) warnIfThroughputRegistrationDiverged(logger logr.Logger, cm *corev1.ConfigMap) {
	if !r.Config.ConfigMapsBootstrapComplete() {
		// Bootstrap pass: the registration decision has not been frozen yet.
		return
	}

	want := r.Config.ThroughputAnalyzerEnabled()
	if want == r.ThroughputRegistered {
		return
	}

	logger.Info("Throughput analyzer registration diverged from live config; controller restart required to apply",
		"wantEnabled", want, "registered", r.ThroughputRegistered, "configMap", cm.Name)
	if r.Recorder != nil {
		msg := fmt.Sprintf(
			"Throughput analyzer enablement in config (%t) differs from the registration "+
				"frozen at controller startup (%t); analyzer registration cannot change at "+
				"runtime. Restart the wva-controller-manager to apply.",
			want, r.ThroughputRegistered)
		r.Recorder.Event(cm, corev1.EventTypeWarning, constants.K8SEventThroughputAnalyzerRestartRequired, msg)
	}
}
