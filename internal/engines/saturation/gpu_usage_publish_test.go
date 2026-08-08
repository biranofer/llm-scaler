package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// The scale-from-zero engine's capacity check reads decision.DefaultGPUUsage and
// treats an absent snapshot as "unknown", skipping the check. Two earlier
// placements of the publish call were each unreachable in a state that matters,
// and neither failure was visible — the check simply stopped happening:
//
//   - inside selectV2Optimizer after its optimizer guard, which only admits
//     GreedyByScore, selected only when enableLimiter is true (default false); and
//   - anywhere after optimizeV2's empty-population return, which is the state of a
//     fleet parked at zero — precisely when scale-from-zero runs.
//
// These specs pin the reachability rather than the wiring.
var _ = Describe("GPU usage snapshot publication", func() {
	// publishedUsage runs the accounting exactly as optimizeV2 does and reports
	// the resulting snapshot.
	publishUsage := func(requests []pipeline.ModelScalingRequest) (*decision.GPUUsage, bool) {
		decision.DefaultGPUUsage = decision.NewGPUUsageStore() // isolate from other specs
		decision.PublishGPUUsage(computeCurrentGPUUsage(requests), computeCurrentGPUUsageByNamespace(requests))
		return decision.LatestGPUUsage()
	}

	It("publishes an empty-but-present snapshot for a fleet parked at zero", func() {
		// The distinction the consumer depends on: "nothing is in use" is a real
		// answer and must not read as "no snapshot yet".
		snap, ok := publishUsage(nil)
		Expect(ok).To(BeTrue(),
			"a cycle over an empty population must still publish; otherwise the wake capacity check "+
				"is skipped for exactly the fleet scale-from-zero exists to wake")
		Expect(snap.ByType).To(BeEmpty())
	})

	It("publishes usage independently of which optimizer is selected", func() {
		// enableLimiter defaults to false, so the optimizer is CostAware and
		// selectV2Optimizer returns at its guard. The snapshot must exist anyway.
		e := &Engine{optimizer: pipeline.NewCostAwareOptimizer()}
		decision.DefaultGPUUsage = decision.NewGPUUsageStore()

		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt).To(Equal(e.optimizer), "a non-GreedyByScore optimizer is left untouched")
		Expect(constraints).To(BeNil())

		// selectV2Optimizer must not be the thing that publishes: optimizeV2 does
		// it before this call, so that the guard above cannot suppress it.
		_, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeFalse(),
			"selectV2Optimizer must not own the publish; optimizeV2 publishes ahead of it")
	})
})
