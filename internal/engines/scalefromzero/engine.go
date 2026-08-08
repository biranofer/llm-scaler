/*
Copyright 2025 The llm-d Authors

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

package scalefromzero

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/controller-runtime/pkg/log"

	accel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// Constants for condition
const (
	MetricsReasonAvailable  = "ScaleFromZero"
	MetricsMessageAvailable = "Scaled from zero due to pending requests"
	reasonDetails           = ": pending request - scale-up"
	targetEPPMetricLabel    = "target_model_name"
	metricNameLabel         = "__name__"
)

// targetEPPMetricNames are the EPP flow-control queue-depth gauges the engine
// reads to decide whether a workload parked at zero has demand waiting, in
// preference order. llm-d's EPP renamed the family to the llm_d_epp_ prefix and
// marks the inference_extension_ name "[Deprecated: Use
// llm_d_epp_flow_control_queue_size]". Both are exported today, so read the new
// name and fall back to the old one rather than pinning either — pinning the
// deprecated name would start failing silently the moment it is dropped.
var targetEPPMetricNames = []string{
	constants.EPPFlowControlQueueSize,
	constants.SchedulerFlowControlQueueSize,
}

// pendingRequestsForModel reports whether the EPP flow-control queue currently
// holds requests for modelID, returning the matching sample.
//
// Only one metric family is consulted: the first name in targetEPPMetricNames
// that the EPP actually exports. The deprecated family mirrors the preferred one,
// so once the preferred family is present its verdict is authoritative and
// falling through to the alias would only double-count.
func pendingRequestsForModel(values []source.MetricValue, modelID string) (source.MetricValue, bool) {
	for _, name := range targetEPPMetricNames {
		exported := false
		for _, value := range values {
			if value.Labels[metricNameLabel] != name {
				continue
			}
			exported = true
			if value.Value > 0 && value.Labels[targetEPPMetricLabel] == modelID {
				return value, true
			}
		}
		if exported {
			return source.MetricValue{}, false
		}
	}
	return source.MetricValue{}, false
}

type Engine struct {
	client         client.Client
	executor       executor.Executor
	recorder       record.EventRecorder
	Datastore      datastore.Datastore
	maxConcurrency int
	config         *config.Config // Unified configuration (injected from main.go)
	// gpuLimiter supplies the GPU/quota constraints a wake must fit within. Nil
	// means no capacity check — see gpuConstraints.
	gpuLimiter pipeline.Limiter
}

// SetGPULimiter injects the limiter whose constraints bound a wake, so the
// engine does not wake a variant onto an accelerator with no free GPUs. Optional:
// with no limiter the engine wakes without a capacity check, which is the
// behaviour it had before selection existed.
func (e *Engine) SetGPULimiter(l pipeline.Limiter) {
	e.gpuLimiter = l
}

// NewEngine creates a new instance of the scale-from-zero engine.
// cfg must be non-nil (validated in main.go before engine creation).
func NewEngine(client client.Client, recorder record.EventRecorder, ds datastore.Datastore, cfg *config.Config) (*Engine, error) {
	if cfg == nil {
		return nil, errors.New("config is nil in NewEngine - this should not happen")
	}

	maxConcurrency := cfg.ScaleFromZeroMaxConcurrency()
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("invalid scale-from-zero max concurrency: must be positive, got %d", maxConcurrency)
	}

	engine := Engine{
		client:         client,
		recorder:       recorder,
		Datastore:      ds,
		maxConcurrency: maxConcurrency,
		config:         cfg,
	}

	// TODO: replace by an hybrid, polling and reactive executor when available
	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     100 * time.Millisecond, // frequent polling to quickly detect scale-from-zero opportunities
		RetryBackoff: 100 * time.Millisecond,
	})

	return &engine, nil
}

// StartOptimizeLoop starts the optimization loop for the scale-from-zero engine.
// It runs until the context is cancelled.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.executor.Start(ctx)
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// Get all inactive (replicas == 0) VAs
	inactiveVAs, scaleTargets, err := utils.InactiveVariantAutoscaling(ctx, e.client)
	if err != nil {
		return err
	}

	logger.V(logging.DEBUG).Info("Found inactive VariantAutoscaling resources", "count", len(inactiveVAs))

	// Which roles a model is already running. A model whose decode is up can
	// serve, so it is the saturation engine's business, not this engine's.
	// Failing to read the active population is not fatal: treat coverage as
	// unknown (nothing covered), which at worst re-publishes an activation for a
	// target that is already awake.
	activeVAs, activeTargets, activeErr := utils.ActiveVariantAutoscaling(ctx, e.client)
	if activeErr != nil {
		logger.V(logging.DEBUG).Error(activeErr, "Could not list active variants; treating every role as uncovered")
	}
	coverage := activeRoleCoverage(activeVAs, activeTargets)

	// Wake decisions are made per model, not per variant: a P/D model needs a
	// decode (optionally with a prefill) chosen together against one GPU budget,
	// and every variant of a model shares one EPP queue, so grouping also
	// collapses what used to be one EPP scrape per inactive variant into one per
	// model.
	groups := groupInactiveByModel(inactiveVAs)
	logger.V(logging.DEBUG).Info("Grouped inactive variants by model", "models", len(groups))

	var wg sync.WaitGroup
	sem := make(chan struct{}, e.maxConcurrency)
	errorCh := make(chan error, e.maxConcurrency)

	// Start error aggregation in a separate goroutine to prevent deadlock
	var aggregatedErrors []error
	var errorWg sync.WaitGroup
	errorWg.Add(1)
	go func() {
		defer errorWg.Done()
		for err := range errorCh {
			if err != nil {
				aggregatedErrors = append(aggregatedErrors, err)
			}
		}
	}()

modelLoop:
	for _, group := range groups {
		// Check if context is cancelled, but don't return immediately
		select {
		case <-ctx.Done():
			logger.V(logging.DEBUG).Info("Context cancelled, stopping new work")
			break modelLoop
		default:
		}

		logger.V(logging.DEBUG).Info("Processing model", "modelID", group.modelID, "namespace", group.namespace)
		wg.Add(1)

		// This call blocks if the channel is full (concurrency limit reached)
		sem <- struct{}{}
		go func(g modelGroup) {
			defer wg.Done()
			defer func() { <-sem }()

			err := e.processInactiveModel(ctx, scaleTargets, g, coverage[g.key()])
			if err != nil {
				logger.V(logging.DEBUG).Error(err, "Error processing model", "modelID", g.modelID, "namespace", g.namespace)
				errorCh <- err
			} else {
				errorCh <- nil
			}
		}(group)
	}

	// Wait for all goroutines to complete, then close error channel
	wg.Wait()
	close(errorCh)

	// Wait for error aggregation to complete
	errorWg.Wait()

	// After all work is done, if the context was cancelled, return that error
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(aggregatedErrors) > 0 {
		return errors.Join(aggregatedErrors...)
	}
	return nil
}

// resolveScaleTarget returns the VA's scale target from the pre-populated map,
// falling back to a live fetch.
func (e *Engine) resolveScaleTarget(ctx context.Context, scaleTargets map[string]scaletarget.ScaleTargetAccessor, va wvav1alpha1.VariantAutoscaling) (scaletarget.ScaleTargetAccessor, error) {
	objName := va.GetScaleTargetName()
	if target, found := scaleTargets[utils.GetNamespacedKey(va.Namespace, objName)]; found {
		return target, nil
	}
	return scaletarget.FetchScaleTarget(ctx, e.client, va.Name, va.GetScaleTargetKind(), objName, va.Namespace)
}

// processInactiveModel decides whether a model with no running capacity should
// be woken, and if so which of its variants to wake.
//
// It replaces a per-variant loop that woke every inactive variant of the model.
// That both over-allocated (several variants for one model's demand) and
// under-served: a variant whose accelerator had no free GPUs was woken anyway,
// its pods sat Pending, and the request that asked for it timed out — a wake
// that looks like progress and delivers none.
func (e *Engine) processInactiveModel(
	ctx context.Context,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	group modelGroup,
	covered coverage,
) error {
	logger := log.FromContext(ctx)

	// Check if inferencepool datastore is empty: this can happen during bootstrapping
	if len(e.Datastore.PoolList()) == 0 {
		logger.Info("Inferencepool datastore is empty - skipping processing inactive model", "modelID", group.modelID)
		return nil
	}

	candidates, pool, err := e.buildCandidates(ctx, scaleTargets, group)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		// Every variant was skipped (no resolvable pool yet, typically during
		// bootstrap); buildCandidates has already logged why.
		return nil
	}

	// Use EPP source from registry
	namespacedPoolName := pool.Namespace + "/" + pool.Name
	eppSource := e.Datastore.PoolGetMetricsSource(namespacedPoolName)
	if eppSource == nil {
		// This is unexpected - pool exists but metrics source is missing
		err := fmt.Errorf("EPP metrics source not found in registry for pool %s", namespacedPoolName)
		logger.Error(err, "Datastore inconsistency detected",
			"modelID", group.modelID,
			"namespace", group.namespace,
			"pool", namespacedPoolName)
		return err
	}

	// One scrape per model per tick. Every variant of a model shares this queue,
	// so the old per-variant scrape re-fetched identical data N times through a
	// mutex-serialized HTTP call on a 100ms loop.
	startTime := time.Now()
	results, err := eppSource.Refresh(ctx, source.RefreshSpec{})
	duration := time.Since(startTime).Seconds()
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeQueueLength)

	if err != nil {
		reason := prometheus.CategorizePrometheusError(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeQueueLength, reason)
		return err
	}

	// Check for pending requests using EPP flowcontrol queue size metrics
	result := results["all_metrics"]
	pending, pendingRequestExist := pendingRequestsForModel(result.Values, group.modelID)
	if !pendingRequestExist {
		// Scale-from-zero loop runs every 100ms; log at DEBUG to avoid flooding.
		logger.V(logging.DEBUG).Info("Scale-from-zero: skipping model, no pending requests in flow control queue",
			"namespace", group.namespace,
			"modelID", group.modelID)
		return nil
	}
	logger.Info(
		"Target workload has pending requests, scaling up from zero",
		"metricName", pending.Labels[metricNameLabel],
		"metric", pending.Labels, "value", pending.Value)

	selected, outcome := selectServingSet(SelectionInput{
		Namespace:      group.namespace,
		Candidates:     candidates,
		DecodeCovered:  covered.decode,
		PrefillCovered: covered.prefill,
		Constraints:    e.gpuConstraints(ctx, group.namespace),
		RequirePrefill: e.requirePrefill(group.modelID, group.namespace),
	})
	if len(selected) == 0 {
		// Operator-visible: demand exists and nothing was woken for it.
		logger.Info("Scale-from-zero: no variant woken for a model with pending requests",
			"namespace", group.namespace,
			"modelID", group.modelID,
			"reason", string(outcome))
		return nil
	}

	var errs []error
	for _, c := range selected {
		va, ok := group.variantByName(c.VariantName)
		if !ok {
			continue
		}
		if err := e.publishActivation(ctx, scaleTargets, va, pool, 1, outcome); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// publishActivation records the wake for one selected variant: it publishes the
// activation decision KEDA acts on, then updates the shared decision cache and
// the variant's status.
func (e *Engine) publishActivation(
	ctx context.Context,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	va wvav1alpha1.VariantAutoscaling,
	pool *poolutil.EndpointPool,
	targetWorkloadReplicas int,
	outcome SelectionOutcome,
) error {
	logger := log.FromContext(ctx)
	objName := va.GetScaleTargetName()

	// 1. Publish the activation decision. Writing the decision store is the whole
	// actuation: it flips the external scaler's IsActive predicate to true and
	// wakes any open StreamIsActive, so KEDA scales the target off zero. WVA does
	// NOT patch the scale subresource itself — KEDA/HPA is the single writer, and
	// a direct patch here would fight the HPA that owns the same field.
	// TODO: Right now we are scaling all the VA for the same target model. We need to scale only the VA that has the lowest cost.
	decision.Set(va.Namespace, objName, int32(targetWorkloadReplicas))
	// Acknowledge the wake so scale-to-zero leaves the model alone for its
	// retention period. The model has served nothing yet — the request that
	// caused this is still queued in the EPP — so the idle request counter the
	// enforcer reads is zero and would zero the replica straight back out from
	// under the request that asked for it.
	decision.MarkActivated(va.Namespace, va.Spec.ModelID)
	logger.Info("Published scale-from-zero activation for Target Workload",
		"variant", va.Name, "target VA model", va.Spec.ModelID,
		"inferencepool", pool.EndpointPicker.ServiceName, "servingSet", string(outcome))

	// 2. Create or update VariantDecision
	va.Status.Actuation.Applied = false
	// Determine accelerator - try status first, then labels
	var accelerator string
	accelerator = va.Status.DesiredOptimizedAlloc.Accelerator
	if accelerator == "" {
		// Try to get from deployment/LWS nodeSelector/nodeAffinity, or VA labels
		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		if scaleTarget, found := scaleTargets[key]; found {
			accelerator = accel.GetAcceleratorNameFromScaleTarget(&va, scaleTarget)
		} else {
			// Deployment/LWS not cached, fall back to VA label via nil deployment/LWS
			accelerator = accel.GetAcceleratorNameFromScaleTarget(&va, nil)
		}
	}

	decision, hasDecision := common.DecisionCache.Get(va.Name, va.Namespace)
	if !hasDecision {
		cost, err := strconv.ParseFloat(va.Spec.VariantCost, 64)
		if err != nil {
			return err
		}
		d := domain.VariantDecision{
			VariantName:        va.Name,
			Namespace:          va.Namespace,
			ModelID:            va.Spec.ModelID,
			Cost:               cost,
			TargetReplicas:     targetWorkloadReplicas, // Scale up to 1 replica
			CurrentReplicas:    targetWorkloadReplicas,
			DesiredReplicas:    targetWorkloadReplicas,
			LastRunTime:        metav1.Now(),
			SaturationBased:    false,
			SafetyOverride:     false,
			ModelBasedDecision: false,
			AcceleratorName:    accelerator,
			MetricsAvailable:   true,
			MetricsReason:      MetricsReasonAvailable,
			MetricsMessage:     MetricsMessageAvailable,
		}
		d.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonScaleFromZero, string(domain.DecisionReasonScaleFromZero)+reasonDetails)
		common.DecisionCache.Set(va.Name, va.Namespace, d)
	} else {
		if decision.CurrentReplicas == 0 {
			decision.TargetReplicas = targetWorkloadReplicas
			decision.CurrentReplicas = targetWorkloadReplicas
			decision.DesiredReplicas = targetWorkloadReplicas
			decision.LastRunTime = metav1.Now()
			decision.SaturationBased = false
			decision.SafetyOverride = false
			decision.ModelBasedDecision = false
			decision.SetDecisionReason(domain.ActionScaleUp, domain.DecisionReasonScaleFromZero, string(domain.DecisionReasonScaleFromZero)+reasonDetails)
			decision.AcceleratorName = accelerator
			decision.MetricsAvailable = true
			decision.MetricsReason = MetricsReasonAvailable
			decision.MetricsMessage = MetricsMessageAvailable
			common.DecisionCache.Set(va.Name, va.Namespace, decision)
		} else {
			logger.Info("Target variant decision.CurrentReplicas is not zero", "value", decision.CurrentReplicas)
		}
	}

	// 3. Updates VA status.
	numReplicas := int32(targetWorkloadReplicas)
	va.Status.DesiredOptimizedAlloc = wvav1alpha1.OptimizedAlloc{
		NumReplicas: &numReplicas,
		LastRunTime: metav1.Now(),
		Accelerator: accelerator,
	}

	// Set condition based on decision characteristics
	wvav1alpha1.SetCondition(&va,
		wvav1alpha1.TypeOptimizationReady,
		metav1.ConditionTrue,
		"ScaleFromZeroMode",
		"scalefromzero decision: "+string(domain.DecisionReasonScaleFromZero)+reasonDetails)

	// Record event just before Actuation.Applied = true
	if hasDecision && targetWorkloadReplicas > 0 {
		e.recorder.Eventf(&va, corev1.EventTypeNormal, constants.K8SEventScaledUp, string(domain.DecisionReasonScaleFromZero)+reasonDetails)
	}
	va.Status.Actuation.Applied = true

	// Log scaling decision for E2E and operators (mirrors saturation engine "Applied ... via shared cache").
	logger.Info("Scale-from-zero decision written to cache",
		"va", va.Name,
		"namespace", va.Namespace,
		"targetReplicas", targetWorkloadReplicas,
		"reason", string(domain.DecisionReasonScaleFromZero)+reasonDetails)

	return nil
}
