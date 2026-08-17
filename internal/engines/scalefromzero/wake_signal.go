package scalefromzero

import (
	"context"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// wakeSignalTTL is how long a pool's "does EPP export a flow-control queue"
// verdict is trusted before it is checked again.
//
// The answer changes only when EPP restarts or its config changes, so it does not
// belong on the 100ms wake loop — and a pool with nothing parked would otherwise
// be scraped ten times a second to answer a question about its configuration.
// The alert on this reason waits 10 minutes, so 30-second granularity costs
// nothing an operator can perceive.
const wakeSignalTTL = 30 * time.Second

// poolWakeVerdict is one pool's answer and when it was obtained.
type poolWakeVerdict struct {
	exported bool
	at       time.Time
}

// wakeModelRef is a model and the EPP pool whose queue would wake it.
type wakeModelRef struct {
	namespace string
	modelID   string
	pool      string
}

// wakeSignalState caches per-pool verdicts and remembers which models were last
// reported on, so a model that leaves the fleet has its reason cleared.
type wakeSignalState struct {
	mu       sync.Mutex
	verdicts map[string]poolWakeVerdict
	reported map[string]wakeModelRef
	lastAt   time.Time
}

// recordPoolWakeVerdict caches what a scrape has already proved.
//
// processInactiveModel scrapes the pool anyway on every tick it considers a
// parked model, so for any pool with something parked the verdict is free and
// current; the periodic sweep then only pays for pools nothing is parked on.
func (e *Engine) recordPoolWakeVerdict(pool string, exported bool, now time.Time) {
	if pool == "" {
		return
	}
	e.wakeSignal.mu.Lock()
	defer e.wakeSignal.mu.Unlock()
	if e.wakeSignal.verdicts == nil {
		e.wakeSignal.verdicts = make(map[string]poolWakeVerdict)
	}
	e.wakeSignal.verdicts[pool] = poolWakeVerdict{exported: exported, at: now}
}

// cachedPoolWakeVerdict returns a pool's verdict when it is still fresh.
func (e *Engine) cachedPoolWakeVerdict(pool string, now time.Time) (exported, fresh bool) {
	e.wakeSignal.mu.Lock()
	defer e.wakeSignal.mu.Unlock()
	v, ok := e.wakeSignal.verdicts[pool]
	if !ok || now.Sub(v.at) > wakeSignalTTL {
		return false, false
	}
	return v.exported, true
}

// queueFamilyExported reports whether any flow-control queue sample is present,
// for any model. This is a property of the EPP, not of a model: an idle queue
// reports 0, and a queue that does not exist reports nothing at all.
func queueFamilyExported(values []source.MetricValue) bool {
	for _, value := range values {
		name := value.Labels[metricNameLabel]
		for _, want := range targetEPPMetricNames {
			if name == want {
				return true
			}
		}
	}
	return false
}

// reportWakeSignal publishes the no-wake-signal reason for every model WVA knows
// about — parked or serving — and clears it for models that have gone away.
//
// It covers SERVING models deliberately. A model that is up right now but sits
// behind an EPP with no flow control is the dangerous case, not a safe one: the
// enforcer will park it on the vLLM request counter, which has nothing to do with
// flow control, and only then will it be unwakeable. Reporting at the moment it
// parks is reporting after the trap has closed.
//
// It also does not live in processInactiveModel, where it started. That function
// returns early on half a dozen bootstrap conditions — no pool in the datastore,
// no resolvable candidates, no EPP metrics source, a scrape failure with no
// fallback — and an emission below those returns silently stops updating, leaving
// whatever it last said standing. A sweep that runs off its own clock cannot be
// skipped by any of them.
func (e *Engine) reportWakeSignal(
	ctx context.Context,
	inactiveVAs []wvav1alpha1.VariantAutoscaling,
	inactiveTargets map[string]scaletarget.ScaleTargetAccessor,
	activeVAs []wvav1alpha1.VariantAutoscaling,
	activeTargets map[string]scaletarget.ScaleTargetAccessor,
	now time.Time,
) {
	logger := log.FromContext(ctx)

	e.wakeSignal.mu.Lock()
	due := e.wakeSignal.lastAt.IsZero() || now.Sub(e.wakeSignal.lastAt) >= wakeSignalTTL
	if due {
		e.wakeSignal.lastAt = now
	}
	e.wakeSignal.mu.Unlock()
	if !due {
		return
	}

	refs := make(map[string]wakeModelRef)
	e.collectWakeRefs(ctx, refs, inactiveVAs, inactiveTargets)
	e.collectWakeRefs(ctx, refs, activeVAs, activeTargets)

	for _, ref := range refs {
		exported, fresh := e.cachedPoolWakeVerdict(ref.pool, now)
		if !fresh {
			var ok bool
			exported, ok = e.scrapePoolWakeVerdict(ctx, ref.pool, now)
			if !ok {
				// Could not look. Say nothing rather than assert an absence we have
				// not observed — a false no-wake-signal sends an operator to restart
				// a healthy EPP.
				logger.V(logging.DEBUG).Info("Wake-signal check skipped: pool not readable",
					"pool", ref.pool, "modelID", ref.modelID)
				continue
			}
		}

		var reasons []string
		if !exported {
			reasons = []string{constants.ScalingBlockedNoWakeSignal}
		}
		metrics.SetModelScalingBlockedReasons(ref.namespace, ref.modelID,
			constants.ScalingBlockedReasonsWake, reasons)
	}

	e.pruneWakeReports(refs)
}

// collectWakeRefs adds one entry per model, resolving the pool from the first
// variant whose workload resolves one. Variants of a model serve the same model
// behind the same EPP, so they share a queue.
func (e *Engine) collectWakeRefs(
	ctx context.Context,
	into map[string]wakeModelRef,
	vas []wvav1alpha1.VariantAutoscaling,
	targets map[string]scaletarget.ScaleTargetAccessor,
) {
	for i := range vas {
		va := vas[i]
		key := utils.GetNamespacedKey(va.Namespace, va.Spec.ModelID)
		if existing, ok := into[key]; ok && existing.pool != "" {
			continue
		}
		pool := e.poolNameForVariant(ctx, va, targets)
		if pool == "" {
			continue
		}
		into[key] = wakeModelRef{namespace: va.Namespace, modelID: va.Spec.ModelID, pool: pool}
	}
}

// poolNameForVariant resolves the namespaced EPP pool name behind a variant's
// workload, or "" when it cannot be resolved — the normal bootstrap state, and
// not something to report a missing wake signal for.
func (e *Engine) poolNameForVariant(
	ctx context.Context,
	va wvav1alpha1.VariantAutoscaling,
	targets map[string]scaletarget.ScaleTargetAccessor,
) string {
	scaleTarget, err := e.resolveScaleTarget(ctx, targets, va)
	if err != nil || scaleTarget == nil {
		return ""
	}
	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil || podTemplateSpec.Labels == nil {
		return ""
	}
	pool, err := e.Datastore.PoolGetFromLabels(va.Namespace, podTemplateSpec.Labels)
	if err != nil || pool == nil {
		return ""
	}
	return pool.Namespace + "/" + pool.Name
}

// scrapePoolWakeVerdict reads a pool's EPP directly. Reports ok=false when the
// pool cannot be read at all, which is not the same as the family being absent.
func (e *Engine) scrapePoolWakeVerdict(ctx context.Context, pool string, now time.Time) (exported, ok bool) {
	eppSource := e.Datastore.PoolGetMetricsSource(pool)
	if eppSource == nil {
		return false, false
	}
	results, err := eppSource.Refresh(ctx, source.RefreshSpec{})
	if err != nil {
		return false, false
	}
	result := results["all_metrics"]
	if result == nil {
		// No result for the query at all is a failure to look, not an observation
		// that the family is absent.
		return false, false
	}
	exported = queueFamilyExported(result.Values)
	e.recordPoolWakeVerdict(pool, exported, now)
	return exported, true
}

// pruneWakeReports clears the wake reason for models no longer in the fleet.
//
// Only this producer's reason is cleared: the enforcer owns the policy reasons
// and prunes its own. A model that has gone away entirely is cleared wholesale by
// the enforcer's prune, which crosses ownership on purpose because by then no
// producer runs for it at all.
func (e *Engine) pruneWakeReports(current map[string]wakeModelRef) {
	e.wakeSignal.mu.Lock()
	previous := e.wakeSignal.reported
	e.wakeSignal.reported = make(map[string]wakeModelRef, len(current))
	for key, ref := range current {
		e.wakeSignal.reported[key] = ref
	}
	e.wakeSignal.mu.Unlock()

	for key, ref := range previous {
		if _, still := current[key]; still {
			continue
		}
		metrics.SetModelScalingBlockedReasons(ref.namespace, ref.modelID,
			constants.ScalingBlockedReasonsWake, nil)
	}
}
