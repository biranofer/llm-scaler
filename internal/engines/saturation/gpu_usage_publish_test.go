package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// The scale-from-zero engine's capacity check reads decision.DefaultGPUUsage and
// treats an ABSENT snapshot as "unknown", skipping the check rather than denying
// a wake. These specs pin what the saturation engine does and does not publish,
// because both directions have been got wrong here:
//
//   - publishing from inside selectV2Optimizer, after its optimizer guard, meant
//     no snapshot on a default (CostAware) deployment; and
//   - publishing an EMPTY snapshot on the empty-population path conflated "no
//     GPUs are in use" with "nothing could be measured", which is the same
//     branch — a failed Prometheus collection also yields no requests.
var _ = Describe("GPU usage snapshot publication", func() {
	BeforeEach(func() {
		// Reset rather than reassigning the package global: PublishGPUUsage and
		// LatestGPUUsage read that variable unsynchronized, so swapping the
		// pointer races with any concurrent user.
		decision.DefaultGPUUsage.Reset()
	})
	AfterEach(func() {
		decision.DefaultGPUUsage.Reset()
	})

	It("publishes usage independently of which optimizer is selected", func() {
		// enableLimiter defaults to false, so the optimizer is CostAware and
		// selectV2Optimizer returns at its guard. Publication must not be owned
		// by anything downstream of that guard, or the capacity check goes blind
		// on every default deployment.
		e := &Engine{optimizer: pipeline.NewCostAwareOptimizer()}

		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt).To(Equal(e.optimizer), "a non-GreedyByScore optimizer is left untouched")
		Expect(constraints).To(BeNil())

		_, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeFalse(),
			"selectV2Optimizer must not own the publish; optimizeV2 publishes ahead of it")
	})

	It("keeps an absent snapshot distinguishable from a measured-empty one", func() {
		// Absence is the signal for "could not measure", and the consumer must be
		// able to tell it apart from a real reading. An empty measured snapshot is
		// a legitimate value and reports ok=true.
		_, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeFalse(), "nothing published yet")

		decision.PublishGPUUsage(map[string]int{}, map[string]map[string]int{})
		snap, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeTrue(), "a measured-empty snapshot is still a snapshot")
		Expect(snap.ByType).To(BeEmpty())
	})

	It("publishes what the population is holding", func() {
		decision.PublishGPUUsage(
			map[string]int{"A100": 6},
			map[string]map[string]int{"chat": {"A100": 6}},
		)
		snap, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeTrue())
		Expect(snap.ByType).To(HaveKeyWithValue("A100", 6))
		Expect(snap.ByNamespace).To(HaveKeyWithValue("chat", HaveKeyWithValue("A100", 6)))
	})
})
