package steadystate

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// blockedEngine builds an engine with its own metric registry, so each test sees
// only the series it published.
func blockedEngine(t *testing.T) (*Engine, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	return &Engine{lastBlockedModels: make(map[string]blockedModelRef)}, registry
}

// blockedModels returns the model_name label of every published reason series.
func blockedModels(t *testing.T, registry *prometheus.Registry) []string {
	t.Helper()
	return seriesLabels(t, registry, constants.WVAModelScalingBlocked, constants.LabelModelName)
}

// publish emits one reason for a model and records it, as the enforcer does.
func publish(e *Engine, namespace, modelID string) {
	reasons := []string{constants.ScalingBlockedVariantFloor}
	metrics.SetModelScalingBlockedReasons(namespace, modelID,
		constants.ScalingBlockedReasonsPolicy, reasons)
	e.recordBlockedModel(namespace, modelID, reasons)
}

// The log line is emitted on transition only: on a loop that runs every
// optimization interval, logging the unchanged verdict means logging it forever.
func TestRecordBlockedModel_ReportsOnlyTransitions(t *testing.T) {
	e, _ := blockedEngine(t)
	floor := []string{constants.ScalingBlockedVariantFloor}

	assert.True(t, e.recordBlockedModel("ns", "m", floor), "first sighting of a blocked model")
	assert.False(t, e.recordBlockedModel("ns", "m", floor), "unchanged verdict must stay quiet")
	assert.True(t, e.recordBlockedModel("ns", "m", nil), "resolving is worth a line")
	assert.False(t, e.recordBlockedModel("ns", "m", nil), "and then quiet again")
	assert.True(t, e.recordBlockedModel("ns", "m", floor), "regressing is worth a line")
	assert.True(t, e.recordBlockedModel("ns", "m",
		[]string{constants.ScalingBlockedVariantFloor, constants.ScalingBlockedEngineUnsupported}),
		"a second reason appearing changes the verdict")
}

// A healthy model must not produce a line just because the process restarted, or
// every restart would log once per model to report that nothing is wrong.
func TestRecordBlockedModel_FirstSightingOfHealthyModelIsQuiet(t *testing.T) {
	e, _ := blockedEngine(t)
	assert.False(t, e.recordBlockedModel("ns", "m", nil))
}

// A departed model has no producer left to clear it. Its reason series would
// otherwise keep asserting that a workload which no longer exists will never
// park — and, because presence is the signal, keep alerting about it.
func TestPruneBlockedModels_DropsDepartedModel(t *testing.T) {
	e, registry := blockedEngine(t)

	publish(e, "ns", "stays")
	publish(e, "ns", "gone")
	require.ElementsMatch(t, []string{"stays", "gone"}, blockedModels(t, registry))

	e.pruneBlockedModels(map[string]bool{utils.GetNamespacedKey("ns", "stays"): true})

	assert.ElementsMatch(t, []string{"stays"}, blockedModels(t, registry))
	assert.NotContains(t, e.lastBlockedModels, utils.GetNamespacedKey("ns", "gone"),
		"bookkeeping for the departed model must be dropped")
}

// Mirrors pruneAnalyzerSeries: a cycle that enumerates no models is usually
// transient — a collector hiccup, config not loaded yet — and must not be read as
// "every model went away". The genuinely empty fleet goes through
// evictAllBlockedModels, on a path that has already proved the list succeeded.
func TestPruneBlockedModels_EmptyActiveSetIsNoOp(t *testing.T) {
	e, registry := blockedEngine(t)

	publish(e, "ns", "m")
	e.pruneBlockedModels(map[string]bool{})

	assert.ElementsMatch(t, []string{"m"}, blockedModels(t, registry))
	assert.Contains(t, e.lastBlockedModels, utils.GetNamespacedKey("ns", "m"))
}

func TestPruneBlockedModels_NilMapDoesNotPanic(t *testing.T) {
	e, _ := blockedEngine(t)
	e.lastBlockedModels = nil
	assert.NotPanics(t, func() {
		e.pruneBlockedModels(map[string]bool{utils.GetNamespacedKey("ns", "m"): true})
	})
}

func TestEvictAllBlockedModels_ClearsEverything(t *testing.T) {
	e, registry := blockedEngine(t)

	publish(e, "ns", "m1")
	publish(e, "other", "m2")
	require.Len(t, blockedModels(t, registry), 2)

	e.evictAllBlockedModels()

	assert.Empty(t, blockedModels(t, registry))
	assert.Empty(t, e.lastBlockedModels, "bookkeeping must be cleared too")
}

// Idle cycles repeat every reconcile, so the second and later ones must be
// no-ops rather than re-deleting or panicking.
func TestEvictAllBlockedModels_IsIdempotent(t *testing.T) {
	e, registry := blockedEngine(t)

	publish(e, "ns", "m")
	assert.NotPanics(t, func() {
		e.evictAllBlockedModels()
		e.evictAllBlockedModels()
	})
	assert.Empty(t, blockedModels(t, registry))
}

// The fleet comes back after an idle stretch: reasons must reappear, and the
// bookkeeping must be rebuilt so the next prune can still find them.
func TestBlockedModels_RepublishAfterEvictAll(t *testing.T) {
	e, registry := blockedEngine(t)

	publish(e, "ns", "m")
	e.evictAllBlockedModels()
	require.Empty(t, blockedModels(t, registry))

	publish(e, "ns", "m")

	assert.ElementsMatch(t, []string{"m"}, blockedModels(t, registry))
	assert.Contains(t, e.lastBlockedModels, utils.GetNamespacedKey("ns", "m"))
}

// The clear crosses ownership on purpose: a departed model's wake reason has no
// producer left either, and the scale-from-zero loop keeps no bookkeeping of its
// own to prune from.
func TestPruneBlockedModels_ClearsAnotherProducersReason(t *testing.T) {
	e, registry := blockedEngine(t)

	publish(e, "ns", "gone")
	metrics.SetModelScalingBlockedReasons("ns", "gone",
		constants.ScalingBlockedReasonsWake,
		[]string{constants.ScalingBlockedNoWakeSignal})
	require.Len(t, blockedModels(t, registry), 2)

	e.pruneBlockedModels(map[string]bool{utils.GetNamespacedKey("ns", "other"): true})

	assert.Empty(t, blockedModels(t, registry))
}

// The initial-cooldown hold: WVA declines to park a model it has not watched long
// enough, because the idle query reads Prometheus history rather than WVA's own
// observations. Pure, so the rule is pinned directly — it is easy to get backwards.
func TestWithinInitialCooldown(t *testing.T) {
	now := time.Now()
	const cooldown = 5 * time.Minute

	assert.True(t, withinInitialCooldown(now.Add(-1*time.Minute), now, cooldown),
		"a model seen a minute ago is still inside a five-minute hold")
	assert.False(t, withinInitialCooldown(now.Add(-6*time.Minute), now, cooldown),
		"past the hold, parking is allowed again")
	assert.False(t, withinInitialCooldown(now.Add(-cooldown), now, cooldown),
		"exactly at the boundary the hold is over")

	// An operator asking for the previous behaviour must be able to have it.
	assert.False(t, withinInitialCooldown(now, now, 0),
		"a zero cooldown disables the hold outright")

	// The first cycle: the model has not been recorded yet. Unknown must not be
	// read as "long enough", or the hold would never apply to the very case it
	// exists for.
	assert.True(t, withinInitialCooldown(time.Time{}, now, cooldown),
		"an unseen model is held, not parked")
	assert.False(t, withinInitialCooldown(time.Time{}, now, 0))
}

// firstSeen must survive the reason updates that happen every cycle, or the hold
// would restart on each pass and the model could never park at all.
func TestRecordBlockedModel_KeepsFirstSeenStable(t *testing.T) {
	e, _ := blockedEngine(t)

	e.recordBlockedModel("ns", "m", nil)
	first := e.watchedSince("ns", "m")
	require.False(t, first.IsZero(), "the first sighting must be stamped")

	e.recordBlockedModel("ns", "m", []string{constants.ScalingBlockedVariantFloor})
	e.recordBlockedModel("ns", "m", nil)

	assert.Equal(t, first, e.watchedSince("ns", "m"),
		"changing reasons must not restart the initial-cooldown clock")
}

// A model that departs and returns is watched afresh: the controller has no
// standing observation of it either.
func TestWatchedSince_ResetsAfterPrune(t *testing.T) {
	e, _ := blockedEngine(t)

	e.recordBlockedModel("ns", "m", nil)
	require.False(t, e.watchedSince("ns", "m").IsZero())

	e.pruneBlockedModels(map[string]bool{utils.GetNamespacedKey("ns", "other"): true})

	assert.True(t, e.watchedSince("ns", "m").IsZero(),
		"a departed model is unknown again, so its hold starts over when it returns")
}
