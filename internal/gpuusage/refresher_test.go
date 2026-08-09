package gpuusage

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

var _ = Describe("Refresher", func() {
	var (
		ctx   context.Context
		store *decision.GPUUsageStore
		disc  *fakeDiscovery
		r     *Refresher
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = decision.NewGPUUsageStore()
		disc = &fakeDiscovery{}
		r = &Refresher{Store: store, Discovery: disc}
	})

	Describe("Refresh", func() {
		It("publishes the cluster and per-namespace views together", func() {
			disc.byType = map[string]int{"A100": 6}
			disc.byNamespace = map[string]map[string]int{"chat": {"A100": 4}, "batch": {"A100": 2}}

			Expect(r.Refresh(ctx)).To(Succeed())

			snap, ok := store.Get()
			Expect(ok).To(BeTrue())
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 6))
			Expect(snap.ByNamespace).To(HaveKeyWithValue("chat", HaveKeyWithValue("A100", 4)))
			Expect(snap.ByNamespace).To(HaveKeyWithValue("batch", HaveKeyWithValue("A100", 2)))
		})

		It("keeps the last observation when a look fails", func() {
			// Consumers treat an ABSENT snapshot as unknown and bound how stale a
			// present one may be, so keeping the previous reading degrades safely.
			// Publishing zeros would not degrade at all — it would be BELIEVED, and
			// a capacity check would report the cluster empty at exactly the moment
			// WVA stopped being able to see it.
			disc.byType = map[string]int{"A100": 8}
			Expect(r.Refresh(ctx)).To(Succeed())

			disc.err = errors.New("the node API is unreachable")
			Expect(r.Refresh(ctx)).ToNot(Succeed(), "a failed observation must reach the caller")

			snap, ok := store.Get()
			Expect(ok).To(BeTrue(), "the previous observation was dropped")
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 8), "zeros must not replace the last good reading")
		})
	})

	Describe("Start", func() {
		It("observes before the first tick", func() {
			// An interval of dead time at startup is an interval in which every
			// scaling decision is taken with no capacity evidence — the defect this
			// package was added to fix. The hour-long interval means a pass proves
			// the observation was not driven by the ticker.
			disc.byType = map[string]int{"H100": 2}
			r.Interval = time.Hour

			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- r.Start(runCtx) }()
			defer cancel()

			Eventually(func() bool {
				_, ok := store.Get()
				return ok
			}, 2*time.Second, 5*time.Millisecond).Should(BeTrue(), "nothing published before the first tick")

			cancel()
			Eventually(done).Should(Receive(BeNil()))
		})

		It("survives a failed first observation", func() {
			// The cluster may not be reachable when the process starts; giving up
			// there would leave WVA with no capacity picture for its whole life.
			disc.err = errors.New("not ready")
			r.Interval = time.Hour

			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- r.Start(runCtx) }()

			Eventually(func() int { return disc.calls }, time.Second, 5*time.Millisecond).
				Should(BeNumerically(">", 0), "Start never attempted an observation")

			cancel()
			Eventually(done).Should(Receive(BeNil()), "a failed first observation must not be fatal")
		})
	})

	Describe("EnsureFresh", func() {
		It("observes again when the published view is stale", func() {
			// The reason this exists: a placement is decided the instant demand
			// appears, and the periodic observation can be a whole interval old by
			// then. The scale-from-zero e2e failed exactly here — a workload became
			// Ready, a wake was considered a second later, and the budget came from
			// an observation taken before that workload existed, so it was waved
			// onto an accelerator that was already full.
			disc.byType = map[string]int{"A100": 0}
			Expect(r.Refresh(ctx)).To(Succeed())

			disc.byType = map[string]int{"A100": 4} // a workload starts and takes the pool
			r.EnsureFresh(ctx, 0)

			snap, _ := store.Get()
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 4),
				"the decision would have been taken against a cluster that no longer exists")
		})

		It("reuses an observation that is still current", func() {
			// The caller runs at 10Hz; re-walking the cache every tick would buy
			// nothing.
			disc.byType = map[string]int{"A100": 1}
			Expect(r.Refresh(ctx)).To(Succeed())
			before := disc.calls

			for range 20 {
				r.EnsureFresh(ctx, time.Minute)
			}
			Expect(disc.calls).To(Equal(before), "a current observation must be reused")
		})

		It("keeps the previous observation when the look fails", func() {
			// A blip must not become a refusal to wake.
			disc.byType = map[string]int{"A100": 2}
			Expect(r.Refresh(ctx)).To(Succeed())

			disc.err = errors.New("transient")
			r.EnsureFresh(ctx, 0)

			snap, ok := store.Get()
			Expect(ok).To(BeTrue())
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 2))
		})

		It("is a no-op on a nil Refresher", func() {
			// The engine field is optional; a nil receiver must not panic the 10Hz
			// loop.
			var nilRefresher *Refresher
			Expect(func() { nilRefresher.EnsureFresh(ctx, 0) }).ToNot(Panic())
		})
	})
})
