// Package gpuusage keeps the cluster's GPU usage picture up to date, independently
// of any scaling engine.
//
// It exists because the picture used to be a by-product of optimization: the
// saturation engine summed its own population each cycle and published that. Two
// things follow from deriving it that way, and both are load-bearing:
//
//   - it only exists once something is already running. The saturation engine
//     returns before publishing when no variant is active, so a fleet parked at
//     zero — the state scale-from-zero exists to serve — produced no picture at
//     all, and the placement check silently degraded to "unknown", which is
//     permissive. Wakes were published onto accelerators nobody had checked;
//   - it could only ever see what WVA manages. GPUs held by other namespaces,
//     system pods, or another autoscaler were invisible, so every pool reported
//     more free capacity than the cluster had.
//
// Discovering usage from the pods actually occupying GPU nodes fixes both at
// once, and does so before the first scaling decision rather than after it.
package gpuusage

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// DefaultInterval is how often the picture is refreshed.
//
// Matched to the saturation engine's own cadence rather than to the
// scale-from-zero loop's 10Hz: usage changes when pods start and stop, which is
// the same timescale scaling decisions happen on. It must also stay well inside
// the consumer's staleness bound (scalefromzero.gpuUsageMaxAge), or the picture
// ages out between refreshes and the placement check degrades to "unknown"
// exactly as it did before this existed.
const DefaultInterval = 15 * time.Second

// Refresher publishes the cluster's GPU usage on a fixed interval.
//
// It is the SOLE producer of the shared snapshot: both the saturation optimizer
// and the scale-from-zero placement check read what it writes. A second producer
// summing usage a different way is what this replaces — two views of "what is in
// use" are free to disagree, and the two engines would then place decisions
// against different pictures of the same cluster.
type Refresher struct {
	// Discovery observes the cluster. Required.
	Discovery discovery.NamespacedUsageDiscovery
	// Store receives each observation. Defaults to decision.DefaultGPUUsage.
	Store *decision.GPUUsageStore
	// Interval between refreshes. Defaults to DefaultInterval.
	Interval time.Duration
}

// Refresh takes one observation and publishes it.
//
// A failed observation deliberately leaves the previous snapshot in place rather
// than publishing an empty one. Publishing zeros for a failed look would say "the
// cluster is idle" at precisely the moment WVA cannot see it — and unlike a
// missing snapshot, which consumers treat as unknown, a zeroed one is believed.
// Consumers bound how stale a snapshot may be, so a sustained outage still
// degrades to unknown; it just does not lie on the way there.
func (r *Refresher) Refresh(ctx context.Context) error {
	byType, byNamespace, err := r.Discovery.DiscoverUsageByNamespace(ctx)
	if err != nil {
		return fmt.Errorf("discovering GPU usage: %w", err)
	}
	r.store().Publish(byType, byNamespace)
	log.FromContext(ctx).V(logging.DEBUG).Info("Refreshed cluster GPU usage",
		"gpusInUse", byType, "namespaces", len(byNamespace))
	return nil
}

// Start refreshes once and then on every tick until ctx is done. It satisfies
// controller-runtime's manager.Runnable.
//
// The first refresh happens BEFORE the ticker, and that ordering is the point:
// the engines consume this, and a picture that arrives one interval late is a
// full interval in which every decision is taken with no capacity evidence. A
// failed first attempt is logged, not fatal — the cluster may simply not be
// reachable yet, and the next tick retries.
func (r *Refresher) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)
	if err := r.Refresh(ctx); err != nil {
		logger.Error(err, "Initial GPU usage discovery failed; scaling decisions have no capacity "+
			"evidence until a refresh succeeds")
	}

	ticker := time.NewTicker(r.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.Refresh(ctx); err != nil {
				// Info, not Error: a blip is expected and the previous snapshot
				// still stands. The condition that actually harms a decision is
				// the snapshot going stale, which the consumers report.
				logger.V(logging.DEBUG).Info("GPU usage refresh failed; keeping the previous snapshot",
					"error", err.Error())
			}
		}
	}
}

func (r *Refresher) store() *decision.GPUUsageStore {
	if r.Store != nil {
		return r.Store
	}
	return decision.DefaultGPUUsage
}

func (r *Refresher) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return DefaultInterval
}
