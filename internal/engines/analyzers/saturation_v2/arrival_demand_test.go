package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The figures below are from an H100 serving Qwen3-0.6B at 4000 input / 1000
// output tokens, which is the shape the benchmark that exposed this drives.
const (
	measuredITL     = 0.0246 // 24.6 ms, vLLM inter_token_latency_seconds
	measuredAvgOut  = 1000.0
	measuredAvgIn   = 4000.0
	measuredService = measuredAvgOut * measuredITL // 24.6s, vs 24.66s measured e2e-minus-queue
)

// avgIn is fixed at the measured shape: every case here varies ITL, output
// length or replica count, never the prompt size.
func metricsAt(itl, avgOut float64, n int) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.ReplicaMetrics{
			AvgITL: itl, AvgInputTokens: measuredAvgIn, AvgOutputTokens: avgOut,
		})
	}
	return out
}

var _ = Describe("estimateArrivalDemand", func() {
	It("reproduces the demand the measured workload implies", func() {
		// 14 req/s x 24.6s x 5000 tokens = 1,722,000. On the run this comes
		// from, occupancy at the same moment read 152,270 and the target
		// collapsed to one replica mid-load; this floor implies about 4.5 at a
		// per-replica capacity of 550,758, and the fleet was observed to settle
		// near 5.
		f := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 3),
		})
		Expect(f.Reason).To(BeEmpty())
		Expect(f.W).To(BeNumerically("~", measuredService, 0.01))
		Expect(f.Tokens).To(BeNumerically("~", 14*measuredService*5000, 1))
	})

	It("does not move when replicas are added", func() {
		// The property the whole file exists for. Occupancy falls as capacity
		// rises; this must not, or it is just another signal that decays to
		// nothing once the fleet is adequate.
		one := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 1),
		})
		ten := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 10),
		})
		// Tolerance, not equality: averaging over one replica and over ten sums
		// a different number of identical terms, which lands a bit apart in the
		// last place. On 1.7e6 the observed gap is ~5e-10, so anything this side
		// of a cent is asserting invariance rather than float determinism.
		Expect(ten.Tokens).To(BeNumerically("~", one.Tokens, 0.01))
	})

	It("falls back to completion rate when no EPP arrival rate exists", func() {
		// At steady state completions equal arrivals. The equality fails while a
		// queue is building -- which is the case occupancy already reads well,
		// so the floor is not what carries that decision.
		rm := metricsAt(measuredITL, measuredAvgOut, 2)
		rm[0].RequestRate = 4
		rm[1].RequestRate = 3
		f := estimateArrivalDemand(domain.AnalyzerInput{ReplicaMetrics: rm})
		Expect(f.Reason).To(BeEmpty())
		Expect(f.Lambda).To(Equal(7.0))
	})

	It("has no opinion rather than a guess when a term is missing", func() {
		// A fabricated floor would hold up a fleet nobody is using. Each of
		// these must yield zero and say which input was absent.
		By("no arrival rate and no completions")
		f := estimateArrivalDemand(domain.AnalyzerInput{
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 2),
		})
		Expect(f.Tokens).To(BeZero())
		Expect(f.Reason).To(ContainSubstring("arrival rate"))

		By("neither service time nor inter-token latency")
		f = estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(0, measuredAvgOut, 2),
		})
		Expect(f.Tokens).To(BeZero())
		Expect(f.Reason).To(ContainSubstring("inter-token latency"))

		By("no output length")
		f = estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, 0, 2),
		})
		Expect(f.Tokens).To(BeZero())
		Expect(f.Reason).To(ContainSubstring("output length"))
	})

	It("averages a timing unweighted across the replicas that reported one", func() {
		// These are per-request costs of the same hardware and model, so every
		// serving replica measures the same quantity and none should count for
		// more. A replica that has completed nothing reports zero and must not
		// drag the mean down -- through it, the floor.
		rm := []domain.ReplicaMetrics{
			{AvgITL: 0.020, AvgOutputTokens: measuredAvgOut},
			{AvgITL: 0.030, AvgOutputTokens: measuredAvgOut},
			{AvgITL: 0, AvgOutputTokens: measuredAvgOut},
		}
		Expect(meanOf(rm, func(m domain.ReplicaMetrics) float64 { return m.AvgITL })).
			To(BeNumerically("~", 0.025, 1e-9))
	})

	It("prefers the engine's measured service time over the reconstruction", func() {
		// The reconstruction is decode-only: it multiplies output length by
		// inter-token latency and so cannot see prefill. Where the engine
		// publishes service time -- vLLM directly, SGLang as e2e minus queue --
		// that value already includes prefill and must win, or a prefill-heavy
		// workload is sized on decode alone and under-provisioned.
		rm := metricsAt(measuredITL, measuredAvgOut, 2)
		for i := range rm {
			rm[i].AvgServiceTime = 40 // deliberately unlike avgOut x ITL (24.6s)
		}
		f := estimateArrivalDemand(domain.AnalyzerInput{ArrivalRate: 14, ReplicaMetrics: rm})
		Expect(f.WSource).To(Equal("measured"))
		Expect(f.W).To(BeNumerically("~", 40, 1e-9))
		Expect(f.Tokens).To(BeNumerically("~", 14*40*5000, 1))
	})

	It("falls back to the reconstruction when no service time is published", func() {
		// An engine or version that reports inter-token latency but no service
		// time still gets a floor, rather than none at all.
		f := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 2),
		})
		Expect(f.WSource).To(Equal("itl"))
		Expect(f.W).To(BeNumerically("~", measuredService, 0.01))
	})
})

var _ = Describe("raiseRoleDemandTo", func() {
	It("preserves the measured split while raising the total", func() {
		// The floor is model-level: the scheduler reports arrivals with no role
		// breakdown, so there is no basis for attributing it to prefill or
		// decode. Scaling keeps whatever split was measured rather than
		// inventing a P/D model.
		rd := map[string]float64{"prefill": 100, "decode": 300}
		raiseRoleDemandTo(rd, 800)
		Expect(rd["prefill"]).To(BeNumerically("~", 200, 1e-9))
		Expect(rd["decode"]).To(BeNumerically("~", 600, 1e-9))
		Expect(rd["prefill"] + rd["decode"]).To(BeNumerically("~", 800, 1e-9))
	})

	It("leaves a split that already meets the total alone", func() {
		rd := map[string]float64{"decode": 900}
		raiseRoleDemandTo(rd, 800)
		Expect(rd["decode"]).To(Equal(900.0))
	})

	It("spreads evenly when the measured split is all zeros", func() {
		// Every role idle but the load still arriving: proportional scaling has
		// nothing to work from, and dropping the floor here would lose it in
		// exactly the state it is meant to cover.
		rd := map[string]float64{"prefill": 0, "decode": 0}
		raiseRoleDemandTo(rd, 500)
		Expect(rd["prefill"]).To(Equal(250.0))
		Expect(rd["decode"]).To(Equal(250.0))
	})

	It("is a no-op for a non-disaggregated model", func() {
		// aggregateRoleDemand returns nil there, and the model-level TotalDemand
		// carries the decision on its own.
		Expect(func() { raiseRoleDemandTo(nil, 500) }).NotTo(Panic())
	})
})
