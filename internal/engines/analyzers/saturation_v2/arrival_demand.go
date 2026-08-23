package saturation_v2

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The analyzer's own demand is occupancy: resident KV plus the waiting-queue
// footprint. Both are states of the FLEET rather than properties of the load, so
// both shrink as capacity grows -- and a fleet that is keeping up reads as idle,
// taking the replica count down with it. Measured on an H100 at 14 QPS: demand
// fell from 7.5M tokens to 152k as ten replicas drained a 910-deep queue, and the
// target followed from the ceiling to one, mid-load.
//
// This file adds a second estimate from the offered load, which does not have
// that property, and uses it only as a FLOOR -- it never lowers demand, so it
// cannot permit a scale-down occupancy would not already have permitted.
//
// The rationale, what the estimate is and is not invariant to, and which of its
// numbers are measured rather than assumed, are in
// docs/developer-guide/saturation-demand-floor.md. That is deliberately not
// repeated here: an earlier draft carried it inline, grew to a design essay, and
// in that form accumulated a confident cross-reference to a symbol that exists
// only on an unmerged branch -- the kind of error a document gets reviewed for
// and a comment does not.

// arrivalFloor is the demand implied by the offered load, and the terms it was
// built from. Reason is empty when the floor could be computed.
type arrivalFloor struct {
	// Tokens is the KV a fleet must hold to serve the offered load, or 0 when
	// there is not enough signal to say.
	Tokens float64
	// Lambda, W and TokensPerRequest are kept for the log line: a floor that
	// binds changes a scaling decision, and "which of the three moved" is the
	// first question anyone will ask of it.
	Lambda           float64
	W                float64
	TokensPerRequest float64
	// WSource is "measured" when W came from the engine's own service-time
	// metric and "itl" when it was reconstructed. Worth logging: the
	// reconstruction is decode-only, so a floor built on it understates
	// prefill-heavy work, and that is invisible from the number alone.
	WSource string
	// Reason names the missing input when Tokens is 0.
	Reason string
	// HasArrivalSignal reports that load IS arriving, even when Tokens is 0.
	//
	// It separates the two ways this can decline to answer, which need opposite
	// treatment from the caller: nothing is arriving (expected, and silence is
	// right) versus something is arriving and it cannot be sized (a real gap,
	// worth saying out loud however quiet the fleet looks).
	HasArrivalSignal bool
}

// estimateArrivalDemand computes the KV a fleet must hold to serve the load
// currently arriving, by Little's law.
//
//	L = λ × W          requests concurrently in service
//	demand = L × (avgIn + avgOut)
//
// W is the per-request SERVICE time, with the queue wait removed. End-to-end
// latency will not do: it climbs when the fleet is behind and falls when it
// catches up, which is the capacity-dependent term this exists to exclude.
//
// W is taken from the engine's own measurement (AvgServiceTime) where there is
// one, and reconstructed as avgOut × ITL where there is not. Measured wins
// because it includes prefill, where the reconstruction is decode-only and so
// understates prefill-heavy work. On a decode-dominated shape the two agree
// closely — 24.60s reconstructed against 24.66s measured, prefill 0.055s of it —
// but that is a property of the shape, not a guarantee.
//
// The fallback also catches a case that is not a version difference: SGLang's
// service time is a SUBTRACTION of two series, and PromQL drops unmatched pairs,
// so an absent queue_time empties the whole expression rather than yielding e2e.
// AvgServiceTime is then 0 and this falls back — the right outcome reached by an
// accident of PromQL, worth knowing before someone "fixes" the query.
//
// Returns 0 rather than a guess whenever a term is missing. A fabricated floor
// would raise demand on a fleet nobody is using; the failure mode of this file
// must be "no opinion", never "invented pressure".
//
// See the file header for what this is and is not invariant to, and for why it
// binds more often than "floor" suggests.
func estimateArrivalDemand(input domain.AnalyzerInput) arrivalFloor {
	lambda := input.ArrivalRate
	if lambda <= 0 {
		// The EPP is the only source of a model-level arrival rate. Without it,
		// fall back to what the engines completed: at steady state a queue that
		// is neither growing nor shrinking makes completion rate equal arrival
		// rate. That equality fails exactly when a queue is building, and
		// completions are then capped by capacity — so this understates λ
		// precisely when demand is highest.
		//
		// Tolerable because a building queue is the case OCCUPANCY reads well,
		// so the floor is not what carries that decision. That reasoning needs
		// the queue term to exist, though: with flow control disabled AND the
		// EPP absent, occupancy has no queue component either and both signals
		// understate together. Nothing here detects that combination.
		for _, rm := range input.ReplicaMetrics {
			lambda += rm.RequestRate
		}
	}
	if lambda <= 0 {
		return arrivalFloor{Reason: "no arrival rate (EPP absent and no completions)"}
	}

	avgIn, avgOut, _ := computeModelWorkloadAverages(input.ReplicaMetrics)
	if avgOut <= 0 {
		return arrivalFloor{Reason: "no average output length", HasArrivalSignal: true}
	}

	// Preferred: the engine's own service time, which already covers prefill.
	w := meanOf(input.ReplicaMetrics, func(rm domain.ReplicaMetrics) float64 { return rm.AvgServiceTime })
	source := "measured"
	if w <= 0 {
		// Fallback: decode-only reconstruction. Understates prefill-heavy work.
		itl := meanOf(input.ReplicaMetrics, func(rm domain.ReplicaMetrics) float64 { return rm.AvgITL })
		if itl <= 0 {
			return arrivalFloor{Reason: "no service time and no inter-token latency", HasArrivalSignal: true}
		}
		w = avgOut * itl
		source = "itl"
	}

	// avgIn + avgOut prices a request at its PEAK: a request holds its input for
	// its whole life while its output accumulates, so the KV it occupies AVERAGED
	// over its lifetime is nearer avgIn + avgOut/2. Using the peak is this
	// analyzer's existing convention, not a new one -- see the note on
	// waitingQueueDemand, which calls I+O "a request's KV footprint at its LAST
	// decode step, not its mean" and keeps it deliberately.
	//
	// So against a mean-measuring occupancy the floor sits about 11% high and
	// binds routinely rather than only during a collapse -- a floor in the sense
	// that it never LOWERS demand, but not a rare one. The larger gap actually
	// observed is not explained by this; see the developer guide.
	perRequest := avgIn + avgOut

	return arrivalFloor{
		Tokens:           lambda * w * perRequest,
		Lambda:           lambda,
		W:                w,
		TokensPerRequest: perRequest,
		WSource:          source,
		HasArrivalSignal: true,
	}
}

// meanOf averages a per-replica timing over the replicas that reported one,
// skipping those that reported nothing.
//
// Unweighted on purpose. Both timings it is used for — service time and ITL —
// are per-request costs of the same hardware and model, so every serving replica
// measures the same underlying quantity. Weighting by traffic would let the
// busiest replica's contention stand in for the fleet's baseline, which is the
// opposite of what this estimate wants. Skipping zeros matters as much: a
// replica that has completed nothing yet would otherwise drag the mean toward
// zero and, through it, the floor.
//
// Skipping only protects against an outright zero, though. A replica that has
// just started serving and reports a small but non-zero timing still pulls the
// unweighted mean down by roughly 1/N, lowering the floor for that cycle. The
// effect is bounded and short-lived -- it decays as the replica warms and its
// timing converges on the rest -- and erring low here means the floor holds back
// rather than over-provisions, so it is not worth a warm-up filter that would
// need its own state.
//
// Note this differs from the collector's own merge helper, which falls back to a
// mean that INCLUDES zeros when no request rate is available
// (weightedByRequestRate). The two are reachable together only when a rank
// reports a service time while its request rate is zero, which is close to
// self-contradictory; the inconsistency is recorded rather than unified, because
// aligning them would change DP-collapse behaviour for a case neither helper was
// written for.
func meanOf(replicaMetrics []domain.ReplicaMetrics, pick func(domain.ReplicaMetrics) float64) float64 {
	var sum float64
	var n int
	for _, rm := range replicaMetrics {
		if v := pick(rm); v > 0 {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// raiseRoleDemandTo scales each role's demand so the roles still sum to total.
//
// The floor is a model-level quantity: λ comes from the scheduler with no role
// breakdown, so there is no basis for attributing it to prefill or decode
// directly. Scaling preserves whatever split the analyzer measured rather than
// inventing a P/D model, and keeps RoleDemand consistent with TotalDemand --
// which matters because the optimizer takes per-role spare from the former and
// the model-level signal from the latter, and a fleet whose variants carry roles
// reads only the per-role one.
//
// A no-op when roleDemand is nil (non-disaggregated) or already sums to at least
// total. When the measured split is all zeros there is nothing to scale, so the
// floor is spread evenly rather than dropped.
//
// A role measuring zero alongside a role measuring something keeps zero, and
// that is deliberate: scaling is proportional, and a role with no demand has no
// share to grow. It means a P/D fleet whose prefill momentarily reports nothing
// gets no floor protection on that role for that cycle. The alternative is to
// invent demand for a role that reports none, which is the fabrication this file
// refuses everywhere else -- and the situation is self-correcting, since the
// next cycle that measures prefill at all gives it a share.
func raiseRoleDemandTo(roleDemand map[string]float64, total float64) {
	if len(roleDemand) == 0 || total <= 0 {
		return
	}
	var sum float64
	for _, v := range roleDemand {
		sum += v
	}
	if sum >= total {
		return
	}
	if sum <= 0 {
		even := total / float64(len(roleDemand))
		for role := range roleDemand {
			roleDemand[role] = even
		}
		return
	}
	scale := total / sum
	for role, v := range roleDemand {
		roleDemand[role] = v * scale
	}
}
