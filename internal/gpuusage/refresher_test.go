package gpuusage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// fakeDiscovery returns a scripted observation, or an error once armed.
type fakeDiscovery struct {
	byType      map[string]int
	byNamespace map[string]map[string]int
	err         error
	calls       int
}

func (f *fakeDiscovery) DiscoverUsageByNamespace(context.Context) (map[string]int, map[string]map[string]int, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.byType, f.byNamespace, nil
}

func TestRefreshPublishesBothViews(t *testing.T) {
	store := decision.NewGPUUsageStore()
	r := &Refresher{
		Store: store,
		Discovery: &fakeDiscovery{
			byType:      map[string]int{"A100": 6},
			byNamespace: map[string]map[string]int{"chat": {"A100": 4}, "batch": {"A100": 2}},
		},
	}

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap, ok := store.Get()
	if !ok {
		t.Fatal("nothing published")
	}
	if snap.ByType["A100"] != 6 {
		t.Errorf("ByType[A100] = %d, want 6", snap.ByType["A100"])
	}
	if snap.ByNamespace["chat"]["A100"] != 4 || snap.ByNamespace["batch"]["A100"] != 2 {
		t.Errorf("ByNamespace = %v, want both namespaces carried through", snap.ByNamespace)
	}
}

// TestFailedRefreshKeepsTheLastObservation pins the difference between "unknown"
// and "idle".
//
// Consumers treat an absent snapshot as unknown and bound how stale a present one
// may be, so keeping the previous reading degrades safely. Publishing zeros for a
// failed look would not degrade at all — it would be BELIEVED, and a capacity
// check would report the cluster as empty at the moment WVA stopped being able to
// see it.
func TestFailedRefreshKeepsTheLastObservation(t *testing.T) {
	store := decision.NewGPUUsageStore()
	disc := &fakeDiscovery{byType: map[string]int{"A100": 8}}
	r := &Refresher{Store: store, Discovery: disc}

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	disc.err = errors.New("the node API is unreachable")
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("a failed observation must be reported to the caller")
	}

	snap, ok := store.Get()
	if !ok {
		t.Fatal("the previous observation was dropped")
	}
	if snap.ByType["A100"] != 8 {
		t.Errorf("ByType[A100] = %d, want the last good reading of 8, not zeros", snap.ByType["A100"])
	}
}

// TestStartObservesBeforeTicking pins the ordering the engines depend on: the
// first observation happens immediately, not one interval later. An interval of
// dead time at startup is an interval in which every scaling decision is taken
// with no capacity evidence — which is the defect this package was added to fix.
func TestStartObservesBeforeTicking(t *testing.T) {
	store := decision.NewGPUUsageStore()
	disc := &fakeDiscovery{byType: map[string]int{"H100": 2}}
	// Long enough that a tick cannot plausibly fire during the test, so a passing
	// run proves the FIRST refresh was not driven by the ticker.
	r := &Refresher{Store: store, Discovery: disc, Interval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := store.Get(); ok {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("no observation was published before the first tick")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Start returned %v, want nil on context cancellation", err)
	}
}

// TestStartSurvivesAFailedFirstObservation: the cluster may not be reachable when
// the process starts, and a refresher that gave up there would leave WVA with no
// capacity picture for its whole lifetime.
func TestStartSurvivesAFailedFirstObservation(t *testing.T) {
	disc := &fakeDiscovery{err: errors.New("not ready")}
	r := &Refresher{Store: decision.NewGPUUsageStore(), Discovery: disc, Interval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// Give Start a moment to make (and survive) its first attempt.
	time.Sleep(50 * time.Millisecond)
	cancel()

	if err := <-done; err != nil {
		t.Errorf("Start returned %v; a failed first observation must not be fatal", err)
	}
	if disc.calls == 0 {
		t.Error("Start never attempted an observation")
	}
}
