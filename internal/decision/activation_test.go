package decision

import (
	"testing"
	"time"
)

const (
	actNS    = "chat"
	actModel = "meta/llama-3-8b"
)

// newTestActivations returns a registry whose clock the caller drives.
func newTestActivations(now *time.Time) *Activations {
	a := NewActivations()
	a.now = func() time.Time { return *now }
	return a
}

// TestWithinRetentionHoldsThenLapses is the whole point of the registry: a model
// woken from zero is protected for the retention period and no longer after it.
func TestWithinRetentionHoldsThenLapses(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = 10 * time.Minute

	if a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("a model that was never woken must not be held")
	}

	a.Mark(actNS, actModel)
	if !a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("a model woken just now must be held")
	}

	now = now.Add(retention - time.Second)
	if !a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("a model must still be held one second before its retention lapses")
	}

	now = now.Add(2 * time.Second)
	if a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("the hold must lapse once the retention period has passed, " +
			"handing the model back to normal idle accounting")
	}
}

// TestMarkExtendsTheHold: the engine re-publishes every poll while requests are
// still queued, and each of those is fresh evidence of demand.
func TestMarkExtendsTheHold(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = time.Minute

	a.Mark(actNS, actModel)
	now = now.Add(50 * time.Second)
	a.Mark(actNS, actModel) // still queued: demand is current
	now = now.Add(50 * time.Second)

	if !a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("re-marking must extend the hold; 50s after the second mark is inside a 1m retention")
	}
}

// TestRetentionIsPerModelAndNamespace guards the key: holding one model must not
// hold an unrelated one, in this namespace or another.
func TestRetentionIsPerModelAndNamespace(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = time.Minute

	a.Mark(actNS, actModel)

	if a.WithinRetention(actNS, "other/model", retention) {
		t.Fatal("a different model in the same namespace must not be held")
	}
	if a.WithinRetention("other-ns", actModel, retention) {
		t.Fatal("the same model in a different namespace must not be held")
	}
}

// TestClearReleasesTheHold covers handing a genuinely-serving model back to
// normal accounting before its retention would otherwise lapse.
func TestClearReleasesTheHold(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)
	const retention = time.Minute

	a.Mark(actNS, actModel)
	a.Clear(actNS, actModel)

	if a.WithinRetention(actNS, actModel, retention) {
		t.Fatal("Clear must release the hold")
	}
}

// TestNonPositiveRetentionDisablesTheHold: an operator who configures no
// retention gets no hold, rather than an accidental default.
func TestNonPositiveRetentionDisablesTheHold(t *testing.T) {
	now := time.Now()
	a := newTestActivations(&now)

	a.Mark(actNS, actModel)

	for _, retention := range []time.Duration{0, -time.Minute} {
		if a.WithinRetention(actNS, actModel, retention) {
			t.Fatalf("retention %s must disable the hold", retention)
		}
	}
}
