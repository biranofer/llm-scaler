package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// latestGPUUsage replaced the population sum, but it inherited one obligation
// from it that the discovered view cannot satisfy on its own.
//
// A namespace-scoped quota is materialised only for namespaces PRESENT in the
// per-namespace usage map (DefaultLimiter.ComputeConstraints treats its keys as
// the active set). Discovery only sees namespaces that have a pod holding a GPU
// right now — so a namespace whose fleet is parked, or which is being optimized
// before anything starts, would drop out and lose its cap entirely, judged
// instead against the cluster aggregate.
var _ = Describe("latestGPUUsage", func() {
	BeforeEach(func() { decision.DefaultGPUUsage.Reset() })
	AfterEach(func() { decision.DefaultGPUUsage.Reset() })

	It("reports not-observed before anything is published", func() {
		_, _, ok := latestGPUUsage(nil)
		Expect(ok).To(BeFalse(), "absent must stay distinguishable from zero usage")
	})

	It("passes the discovered figures through untouched", func() {
		decision.PublishGPUUsage(
			map[string]int{"A100": 4, "H100": 2},
			map[string]map[string]int{"team-a": {"A100": 4}},
		)
		byType, byNS, ok := latestGPUUsage([]pipeline.ModelScalingRequest{{Namespace: "team-a"}})
		Expect(ok).To(BeTrue())
		Expect(byType).To(HaveKeyWithValue("A100", 4))
		Expect(byType).To(HaveKeyWithValue("H100", 2), "usage WVA does not manage is still counted")
		Expect(byNS["team-a"]).To(HaveKeyWithValue("A100", 4))
	})

	It("materialises a namespace being optimized that holds no GPUs", func() {
		decision.PublishGPUUsage(
			map[string]int{"A100": 4},
			map[string]map[string]int{"team-a": {"A100": 4}},
		)
		_, byNS, ok := latestGPUUsage([]pipeline.ModelScalingRequest{
			{Namespace: "team-a"}, {Namespace: "team-parked"},
		})
		Expect(ok).To(BeTrue())
		Expect(byNS).To(HaveKey("team-parked"),
			"a namespace with no GPU-holding pod must still be constrained by its quota")
		Expect(byNS["team-parked"]).To(BeEmpty())
	})

	It("does not mutate the stored snapshot", func() {
		// Get documents its return as the shared copy. Materialising in place
		// would leak invented namespaces into every later reader, including the
		// scale-from-zero engine.
		decision.PublishGPUUsage(map[string]int{"A100": 4}, map[string]map[string]int{"team-a": {"A100": 4}})

		_, _, _ = latestGPUUsage([]pipeline.ModelScalingRequest{{Namespace: "team-parked"}})

		snap, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeTrue())
		Expect(snap.ByNamespace).ToNot(HaveKey("team-parked"),
			"the shared snapshot must be left exactly as the producer published it")
	})
})

var _ = Describe("gpuConstraintProviders", func() {
	clusterQuota := func(name string) *pipeline.DefaultLimiter {
		inv := pipeline.NewQuotaInventory(config.QuotaLimiterConfig{
			Name: name, Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"A100": 4},
		})
		return pipeline.NewDefaultLimiter(name, inv)
	}

	It("returns a DefaultLimiter (ConstraintProvider) as its own single provider", func() {
		dl := clusterQuota("q")
		got := gpuConstraintProviders(dl)
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(BeIdenticalTo(dl))
	})

	It("returns each ConstraintProvider constituent of a CompositeLimiter", func() {
		comp := pipeline.NewCompositeLimiter("c", []pipeline.Limiter{clusterQuota("a"), clusterQuota("b")})
		Expect(gpuConstraintProviders(comp)).To(HaveLen(2))
	})

	It("returns nil for a limiter that is not a ConstraintProvider (NoOpLimiter)", func() {
		Expect(gpuConstraintProviders(pipeline.NewNoOpLimiter("noop"))).To(BeNil())
	})
})
