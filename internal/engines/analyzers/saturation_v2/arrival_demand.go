package saturation_v2

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The analyzer's own demand is occupancy: resident KV plus the waiting-queue
// footprint. Both are STATES OF THE FLEET rather than properties of the load,
// and both shrink as capacity grows -- resident KV because residence time falls,
// the queue because it drains. So the signal that sizes the fleet is a function
// of the fleet, and on a fleet that is comfortably provisioned it decays toward
// zero and takes the replica count with it.
//
// Measured on an H100 at 14 QPS: demand fell from 7.5M tokens to 152k as ten
// replicas drained a 910-deep queue, and the target followed it from the ceiling
// down to one, mid-load. Nothing was wrong with the arithmetic; the input simply
// stopped describing the workload once the workload was being served.
//
// This file adds a second estimate that does not have that property, and uses it
// only as a FLOOR. It never lowers demand, so it cannot cause a scale-down that
// occupancy would not already have permitted.

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
}

// estimateArrivalDemand computes the KV a fleet must hold to serve the load
// currently arriving, by Little's law.
//
//	L = λ × W          requests concurrently in service
//	demand = L × (avgIn + avgOut)
//
// Every term is invariant to the replica count, which is the whole point:
//
//   - λ is the arrival rate the scheduler reports. Adding replicas does not
//     change how many requests are asked for.
//   - avgIn/avgOut are the request shape.
//   - W is the SERVICE time — how long a request occupies a replica with the
//     queue wait removed. End-to-end latency will not do: it climbs when the
//     fleet is behind and falls when it catches up, which is exactly the
//     capacity-dependent term this exists to exclude, and the one that collapsed
//     50× in the run above.
//
// W is MEASURED where the engine reports it (AvgServiceTime, from vLLM's
// request_inference_time_seconds or SGLang's e2e minus queue) and reconstructed
// as avgOut × ITL only when it is not. The measured value is preferred because
// it includes prefill; the reconstruction is decode-only and so understates
// prefill-heavy work, which under-provisions rather than over. On the run this
// was built from the two agreed to 0.2% — 24.60s reconstructed against 24.66s
// measured, with prefill just 0.055s of it — but that agreement is a property of
// a decode-dominated shape, not a general one.
//
// Keeping the reconstruction as a fallback is what makes this work on an engine
// or version that publishes inter-token latency but no service time, without
// making the common case pay for that.
//
// It returns 0 rather than a guess whenever a term is missing. A fabricated
// floor would raise demand on a fleet nobody is using, and the failure mode of
// this whole file must be "no opinion", never "invented pressure".
func estimateArrivalDemand(input domain.AnalyzerInput) arrivalFloor {
	lambda := input.ArrivalRate
	if lambda <= 0 {
		// The EPP is the only source of a model-level arrival rate. Without it,
		// fall back to what the engines completed: at steady state a queue that
		// is neither growing nor shrinking makes completion rate equal arrival
		// rate. That equality fails exactly when a queue is building -- but a
		// building queue is the case occupancy already measures well, so the
		// floor is not what carries the decision there.
		for _, rm := range input.ReplicaMetrics {
			lambda += rm.RequestRate
		}
	}
	if lambda <= 0 {
		return arrivalFloor{Reason: "no arrival rate (EPP absent and no completions)"}
	}

	avgIn, avgOut, _ := computeModelWorkloadAverages(input.ReplicaMetrics)
	if avgOut <= 0 {
		return arrivalFloor{Reason: "no average output length"}
	}

	// Preferred: the engine's own service time, which already covers prefill.
	w := meanOf(input.ReplicaMetrics, func(rm domain.ReplicaMetrics) float64 { return rm.AvgServiceTime })
	source := "measured"
	if w <= 0 {
		// Fallback: decode-only reconstruction. Understates prefill-heavy work.
		itl := meanOf(input.ReplicaMetrics, func(rm domain.ReplicaMetrics) float64 { return rm.AvgITL })
		if itl <= 0 {
			return arrivalFloor{Reason: "no service time and no inter-token latency"}
		}
		w = avgOut * itl
		source = "itl"
	}

	perRequest := avgIn + avgOut

	return arrivalFloor{
		Tokens:           lambda * w * perRequest,
		Lambda:           lambda,
		W:                w,
		TokensPerRequest: perRequest,
		WSource:          source,
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
