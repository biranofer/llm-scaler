package warmpool

import (
	"sort"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
)

// SwitchConfig is when a retained pool changes which model it has awake.
//
// Only retained pools use it. An ordinary pool switches nothing: it lends a
// bridge to whichever variant is short and takes it back when the replicas
// arrive, and that is demand-led and needs no policy. A retained pool has no
// replicas coming -- it IS the capacity -- so something has to decide, and doing
// nothing means the model that happened to be awake first keeps the GPUs.
type SwitchConfig struct {
	// SpareThreshold is the fraction of its capacity a variant must have spare
	// to be considered comfortable. Below it, the variant wants the pool.
	//
	// Zero disables the spare rule, leaving only the scale-up rule below.
	SpareThreshold float64

	// MinInterval is the shortest time between two switches of one pool.
	//
	// A switch is not free: the model being replaced drains its in-flight
	// requests and sleeps, and the model taking over wakes. Without a floor, two
	// variants hovering either side of the threshold would trade the GPUs back
	// and forth and spend most of their time doing neither's work.
	MinInterval time.Duration
}

// switchReason says why a pool did or did not change its awake model, for the
// log line. The pool holds accelerators either way, so "nothing happened" needs
// to be as legible as a switch.
type switchReason string

const (
	reasonScaleUp     switchReason = "the candidate needs to scale up and the awake model does not"
	reasonSpare       switchReason = "the candidate is below the spare threshold and the awake model is not"
	reasonNoCandidate switchReason = "no variant is under more pressure than the awake one"
	reasonTooSoon     switchReason = "a switch is due but the last one was too recent"
	reasonUnmeasured  switchReason = "no pressure reading for the awake model, so there is nothing to compare against"
)

// urgency ranks how badly a variant wants the pool. Higher wins.
//
// Two tiers rather than one number, because the two signals mean different
// things and the stronger one has to be able to override a close call on the
// weaker. A variant the optimizer is trying to grow is short of capacity NOW; a
// variant merely below its spare threshold is heading that way.
type urgency int

const (
	urgencyComfortable urgency = iota
	urgencyLowSpare
	urgencyNeedsScaleUp
)

func urgencyOf(p decision.Pressure, cfg SwitchConfig) urgency {
	switch {
	case p.NeedsScaleUp:
		return urgencyNeedsScaleUp
	case cfg.SpareThreshold > 0 && p.SpareFraction < cfg.SpareThreshold:
		return urgencyLowSpare
	default:
		return urgencyComfortable
	}
}

// chooseAwake decides which model a retained pool should have awake next.
//
// It returns the variant to switch to, the reason, and whether to switch at all.
//
// The rule, in the order it is applied:
//
//  1. A candidate must be under MORE pressure than the model currently awake.
//     Equal pressure is not enough: if the awake model is also below the spare
//     threshold, or also needs to scale up, switching moves the shortage from
//     one model to the other and pays a drain and a wake to do it.
//  2. Ties go to the variant with the least spare capacity, so the pool lands on
//     the one furthest into trouble rather than whichever was listed first.
//  3. A switch may not follow another within MinInterval.
//
// Nothing is decided from an absent reading. A variant the optimizer has not
// measured is neither a candidate nor a reason to stay: acting either way would
// be acting on silence, and on a retained pool the cost of being wrong is the
// serving capacity of every model in it.
func chooseAwake(
	cfg SwitchConfig,
	variants []policy.VariantDemand,
	awake string,
	lastSwitch time.Time,
	pressureFor func(namespace, target string) (decision.Pressure, bool),
	now time.Time,
) (string, switchReason, bool) {
	// What the incumbent is putting up with. Without it there is no comparison
	// to make -- rule 1 is entirely relative.
	var awakePressure decision.Pressure
	awakeKnown := false
	for _, v := range variants {
		if v.Model.Variant == awake && v.Target != "" {
			awakePressure, awakeKnown = pressureFor(v.Model.Namespace, v.Target)
			break
		}
	}
	// An EMPTY pool is the exception: with nothing awake there is no incumbent to
	// be measured, and the first model to want the GPUs should get them.
	if awake != "" && !awakeKnown {
		return "", reasonUnmeasured, false
	}
	awakeUrgency := urgencyComfortable
	if awake != "" {
		awakeUrgency = urgencyOf(awakePressure, cfg)
	}

	type candidate struct {
		variant string
		urgency urgency
		spare   float64
	}
	var best *candidate
	for _, v := range variants {
		if v.Model.Variant == awake || v.Target == "" {
			continue
		}
		p, known := pressureFor(v.Model.Namespace, v.Target)
		if !known {
			continue
		}
		u := urgencyOf(p, cfg)
		// STRICTLY more pressure than the incumbent. This is the whole of the
		// user-visible rule: do not switch to a model that is no worse off than
		// the one already awake.
		if u <= awakeUrgency {
			continue
		}
		c := candidate{variant: v.Model.Variant, urgency: u, spare: p.SpareFraction}
		if best == nil || c.urgency > best.urgency ||
			(c.urgency == best.urgency && c.spare < best.spare) {
			picked := c
			best = &picked
		}
	}
	if best == nil {
		return "", reasonNoCandidate, false
	}

	// The floor applies to the scale-up case too. A scale-up need is what makes
	// a switch worth making at all, not a licence to make it twice a minute:
	// the signal flaps as replicas come and go, and a pool that chased it would
	// spend its time draining and waking rather than serving.
	if !lastSwitch.IsZero() && now.Sub(lastSwitch) < cfg.MinInterval {
		return best.variant, reasonTooSoon, false
	}

	reason := reasonSpare
	if best.urgency == urgencyNeedsScaleUp {
		reason = reasonScaleUp
	}
	return best.variant, reason, true
}

// sortedVariantNames is the variants of one pool, ordered, so a log line reads
// the same way twice for the same set.
func sortedVariantNames(variants []policy.VariantDemand) []string {
	out := make([]string, 0, len(variants))
	for _, v := range variants {
		out = append(out, v.Model.Variant)
	}
	sort.Strings(out)
	return out
}

// DefaultMinSwitchInterval is the floor applied when a pool names none.
//
// Ten minutes because a switch costs a drain plus a wake -- seconds to tens of
// seconds of one model's capacity -- and the demand it responds to moves on the
// scale of minutes. Short enough to follow a real shift in load, long enough
// that noise either side of the threshold cannot drive it.
const DefaultMinSwitchInterval = 10 * time.Minute
