package warmpool

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// The two models the retained pool in these tests holds, and the scale targets
// they are keyed by.
const (
	variantOther = "other-one"
	targetAwake  = "deploy-awake"
	targetOther  = "deploy-other"
)

// switchVariants is a retained pool holding two models.
func switchVariants() []policy.VariantDemand {
	return []policy.VariantDemand{
		{Model: pool.ModelRef{Namespace: "tenant", Variant: "awake-one"}, Target: targetAwake},
		{Model: pool.ModelRef{Namespace: "tenant", Variant: variantOther}, Target: targetOther},
	}
}

// readings answers for the two targets above and knows nothing else.
func readings(awake, other decision.Pressure) func(string, string) (decision.Pressure, bool) {
	return func(_, target string) (decision.Pressure, bool) {
		switch target {
		case targetAwake:
			return awake, true
		case targetOther:
			return other, true
		}
		return decision.Pressure{}, false
	}
}

var switchCfg = SwitchConfig{SpareThreshold: 0.20, MinInterval: 10 * time.Minute}

// A model that has fallen below the spare threshold takes the pool from one that
// has not. This is the rule the pool exists to apply: a retained pool IS the
// capacity, so the GPUs should be under whichever model is closest to running
// out of it.
func TestThePoolSwitchesToTheModelRunningOutOfRoom(t *testing.T) {
	now := time.Now()
	got, reason, switching := chooseAwake(switchCfg, switchVariants(), "awake-one",
		now.Add(-time.Hour),
		readings(
			decision.Pressure{SpareFraction: 0.60},
			decision.Pressure{SpareFraction: 0.05},
		), now)

	if !switching || got != variantOther {
		t.Fatalf("chooseAwake = %q, %q, %v; want a switch to other-one", got, reason, switching)
	}
	if reason != reasonSpare {
		t.Errorf("reason = %q, want the spare-threshold reason", reason)
	}
}

// BOTH below the threshold means NO switch.
//
// Switching here would move the shortage from one model to the other and pay a
// drain and a wake to do it -- the pool cannot make two models comfortable at
// once, and choosing between two unhappy ones is not what this rule is for.
func TestNoSwitchWhenTheAwakeModelIsAlsoShort(t *testing.T) {
	now := time.Now()
	got, reason, switching := chooseAwake(switchCfg, switchVariants(), "awake-one",
		now.Add(-time.Hour),
		readings(
			decision.Pressure{SpareFraction: 0.10},
			decision.Pressure{SpareFraction: 0.05},
		), now)

	if switching {
		t.Errorf("switched to %q though the awake model is also below the threshold", got)
	}
	if reason != reasonNoCandidate {
		t.Errorf("reason = %q, want no-candidate", reason)
	}
}

// A model the optimizer is trying to SCALE UP outranks one that is merely low on
// spare, and takes the pool from a model that is comfortable.
func TestAModelNeedingToScaleUpOutranksALowSpareOne(t *testing.T) {
	now := time.Now()
	variants := append(switchVariants(),
		policy.VariantDemand{Model: pool.ModelRef{Namespace: "tenant", Variant: "third"}, Target: "deploy-third"})

	pressure := func(_, target string) (decision.Pressure, bool) {
		switch target {
		case targetAwake:
			return decision.Pressure{SpareFraction: 0.60}, true
		case targetOther:
			return decision.Pressure{SpareFraction: 0.05}, true
		case "deploy-third":
			return decision.Pressure{SpareFraction: 0.15, NeedsScaleUp: true}, true
		}
		return decision.Pressure{}, false
	}

	got, reason, switching := chooseAwake(switchCfg, variants, "awake-one", now.Add(-time.Hour), pressure, now)

	if !switching || got != "third" {
		t.Fatalf("chooseAwake = %q, %v; want the scale-up candidate, not the merely-low-spare one", got, switching)
	}
	if reason != reasonScaleUp {
		t.Errorf("reason = %q, want the scale-up reason", reason)
	}
}

// If the AWAKE model also needs to scale up, nothing moves -- the same rule as
// the spare case, one tier up. The pool cannot serve two models at once, and the
// one already awake is at least already awake.
func TestNoSwitchWhenTheAwakeModelAlsoNeedsToScaleUp(t *testing.T) {
	now := time.Now()
	got, _, switching := chooseAwake(switchCfg, switchVariants(), "awake-one",
		now.Add(-time.Hour),
		readings(
			decision.Pressure{SpareFraction: 0.02, NeedsScaleUp: true},
			decision.Pressure{SpareFraction: 0.01, NeedsScaleUp: true},
		), now)

	if switching {
		t.Errorf("switched to %q though the awake model also needs to scale up", got)
	}
}

// A switch that is due still waits out MinInterval.
//
// This applies to the scale-up case too, deliberately. A scale-up need is what
// makes a switch worth making, not a licence to make it repeatedly: the signal
// flaps as replicas come and go, and a pool chasing it would spend its time
// draining and waking rather than serving.
func TestASwitchWaitsOutTheMinimumInterval(t *testing.T) {
	now := time.Now()
	got, reason, switching := chooseAwake(switchCfg, switchVariants(), "awake-one",
		now.Add(-time.Minute), // the last switch was one minute ago
		readings(
			decision.Pressure{SpareFraction: 0.60},
			decision.Pressure{SpareFraction: 0.01, NeedsScaleUp: true},
		), now)

	if switching {
		t.Errorf("switched to %q one minute after the last switch; MinInterval is %s", got, switchCfg.MinInterval)
	}
	if reason != reasonTooSoon {
		t.Errorf("reason = %q, want too-soon -- the candidate is named so the log says what is waiting", reason)
	}
	if got != variantOther {
		t.Errorf("candidate = %q, want other-one reported even though the switch is deferred", got)
	}

	// ...and goes through once the interval has passed.
	_, _, switching = chooseAwake(switchCfg, switchVariants(), "awake-one",
		now.Add(-11*time.Minute),
		readings(
			decision.Pressure{SpareFraction: 0.60},
			decision.Pressure{SpareFraction: 0.01, NeedsScaleUp: true},
		), now)
	if !switching {
		t.Error("the switch should proceed once MinInterval has passed")
	}
}

// An UNMEASURED awake model stops the comparison. Every rule here is relative to
// what the awake model is putting up with, so without that reading there is
// nothing to be more urgent than -- and switching anyway would be acting on
// silence, at the cost of every model in the pool.
func TestNoSwitchWhenTheAwakeModelHasNoReading(t *testing.T) {
	now := time.Now()
	pressure := func(_, target string) (decision.Pressure, bool) {
		if target == targetOther {
			return decision.Pressure{SpareFraction: 0.01, NeedsScaleUp: true}, true
		}
		return decision.Pressure{}, false // the awake model was never measured
	}

	got, reason, switching := chooseAwake(switchCfg, switchVariants(), "awake-one",
		now.Add(-time.Hour), pressure, now)

	if switching {
		t.Errorf("switched to %q on the strength of no reading for the awake model", got)
	}
	if reason != reasonUnmeasured {
		t.Errorf("reason = %q, want unmeasured", reason)
	}
}

// An EMPTY pool has no incumbent to compare against, and the first model that
// wants the GPUs should get them. Requiring a reading for a model that is not
// there would leave a retained pool asleep forever.
func TestAnEmptyPoolTakesTheFirstModelThatWantsIt(t *testing.T) {
	now := time.Now()
	got, _, switching := chooseAwake(switchCfg, switchVariants(), "",
		time.Time{}, // nothing has ever been switched
		readings(
			decision.Pressure{SpareFraction: 0.60},
			decision.Pressure{SpareFraction: 0.05},
		), now)

	if !switching || got != variantOther {
		t.Errorf("chooseAwake = %q, %v; want the short model to take an empty pool", got, switching)
	}
}

// With the threshold unset, only the scale-up rule applies. A pool that named no
// threshold has not asked to be switched on spare capacity, and inventing one
// would move its GPUs on a number nobody chose.
func TestWithNoThresholdOnlyScaleUpSwitches(t *testing.T) {
	now := time.Now()
	cfg := SwitchConfig{MinInterval: 10 * time.Minute}

	_, _, switching := chooseAwake(cfg, switchVariants(), "awake-one", now.Add(-time.Hour),
		readings(
			decision.Pressure{SpareFraction: 0.60},
			decision.Pressure{SpareFraction: 0.01},
		), now)
	if switching {
		t.Error("switched on spare capacity though no threshold was configured")
	}

	_, _, switching = chooseAwake(cfg, switchVariants(), "awake-one", now.Add(-time.Hour),
		readings(
			decision.Pressure{SpareFraction: 0.60},
			decision.Pressure{SpareFraction: 0.01, NeedsScaleUp: true},
		), now)
	if !switching {
		t.Error("a scale-up need should still switch with no threshold configured")
	}
}

// Between two candidates at the same urgency, the one with least room wins --
// not whichever happened to be listed first.
func TestTheTightestCandidateWins(t *testing.T) {
	now := time.Now()
	variants := append(switchVariants(),
		policy.VariantDemand{Model: pool.ModelRef{Namespace: "tenant", Variant: "third"}, Target: "deploy-third"})

	pressure := func(_, target string) (decision.Pressure, bool) {
		switch target {
		case targetAwake:
			return decision.Pressure{SpareFraction: 0.60}, true
		case targetOther:
			return decision.Pressure{SpareFraction: 0.15}, true
		case "deploy-third":
			return decision.Pressure{SpareFraction: 0.02}, true
		}
		return decision.Pressure{}, false
	}

	got, _, switching := chooseAwake(switchCfg, variants, "awake-one", now.Add(-time.Hour), pressure, now)
	if !switching || got != "third" {
		t.Errorf("chooseAwake = %q, %v; want third, which has the least spare", got, switching)
	}
}
