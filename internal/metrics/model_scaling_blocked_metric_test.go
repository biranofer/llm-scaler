package metrics

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var _ = Describe("SetModelScalingBlockedReasons", func() {
	series := func(registry *prometheus.Registry) []*dto.Metric {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == constants.WVAModelScalingBlocked {
				return mf.GetMetric()
			}
		}
		return nil
	}

	reasons := func(registry *prometheus.Registry) []string {
		got := series(registry)
		out := make([]string, 0, len(got))
		for _, m := range got {
			out = append(out, getLabelValue(m, constants.LabelReason))
		}
		return out
	}

	policy := constants.ScalingBlockedReasonsPolicy
	wake := constants.ScalingBlockedReasonsWake

	It("emits one series per active reason, at 1", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlockedReasons("ns-a", "Qwen/Qwen3-0.6B", policy, []string{
			constants.ScalingBlockedVariantFloor,
			constants.ScalingBlockedEngineUnsupported,
		})

		got := series(registry)
		Expect(got).To(HaveLen(2))
		for _, m := range got {
			Expect(m.GetGauge().GetValue()).To(Equal(float64(1)))
			Expect(getLabelValue(m, constants.LabelNamespace)).To(Equal("ns-a"))
			Expect(getLabelValue(m, constants.LabelModelName)).To(Equal("Qwen/Qwen3-0.6B"))
		}
		Expect(reasons(registry)).To(ConsistOf(
			constants.ScalingBlockedVariantFloor,
			constants.ScalingBlockedEngineUnsupported,
		))
	})

	// Presence is the signal, so a reason that stops holding must lose its series.
	// Leaving it at 1 alerts forever about a problem the operator has already
	// fixed, which is worse than never having reported it.
	It("removes a reason that no longer holds", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlockedReasons("ns-a", "m", policy, []string{
			constants.ScalingBlockedVariantFloor,
			constants.ScalingBlockedEngineUnsupported,
		})
		SetModelScalingBlockedReasons("ns-a", "m", policy, []string{constants.ScalingBlockedVariantFloor})

		Expect(reasons(registry)).To(ConsistOf(constants.ScalingBlockedVariantFloor))
	})

	It("removes every owned series once nothing blocks", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlockedReasons("ns-a", "m", policy, []string{constants.ScalingBlockedPolicyForbidsZero})
		SetModelScalingBlockedReasons("ns-a", "m", policy, nil)

		Expect(series(registry)).To(BeEmpty(),
			"a cleared reason must vanish, not linger at 0")
	})

	// The whole reason the API takes an `owned` set. The enforcer runs once per
	// optimization interval and the scale-from-zero loop runs at 10Hz; if either
	// cleared the other's reasons, the metric would flap for reasons having
	// nothing to do with the cluster.
	It("leaves another producer's reasons alone", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlockedReasons("ns-a", "m", wake, []string{constants.ScalingBlockedNoWakeSignal})
		SetModelScalingBlockedReasons("ns-a", "m", policy, []string{constants.ScalingBlockedVariantFloor})

		Expect(reasons(registry)).To(ConsistOf(
			constants.ScalingBlockedNoWakeSignal,
			constants.ScalingBlockedVariantFloor,
		))

		// ...and the reverse order, since the 10Hz producer is the one that will
		// usually run second.
		SetModelScalingBlockedReasons("ns-a", "m", wake, nil)
		Expect(reasons(registry)).To(ConsistOf(constants.ScalingBlockedVariantFloor),
			"clearing the wake reason must not clear the policy reason")
	})

	It("ignores an active reason the caller does not own", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		// A reason nobody owns could never be cleared again, so it must not be set.
		SetModelScalingBlockedReasons("ns-a", "m", wake, []string{constants.ScalingBlockedVariantFloor})

		Expect(series(registry)).To(BeEmpty())
	})

	It("keeps models independent", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlockedReasons("ns-a", "m1", policy, []string{constants.ScalingBlockedVariantFloor})
		SetModelScalingBlockedReasons("ns-a", "m2", policy, []string{constants.ScalingBlockedVariantFloor})
		SetModelScalingBlockedReasons("ns-b", "m1", policy, []string{constants.ScalingBlockedVariantFloor})

		SetModelScalingBlockedReasons("ns-a", "m1", policy, nil)

		got := series(registry)
		Expect(got).To(HaveLen(2))
		for _, m := range got {
			key := getLabelValue(m, constants.LabelNamespace) + "/" + getLabelValue(m, constants.LabelModelName)
			Expect(key).NotTo(Equal("ns-a/m1"))
		}
	})

	It("is a no-op before InitMetrics rather than a panic", func() {
		modelScalingBlocked = nil
		Expect(func() {
			SetModelScalingBlockedReasons("ns", "m", policy, []string{constants.ScalingBlockedVariantFloor})
		}).NotTo(Panic())
	})
})

var _ = Describe("ClearModelScalingBlocked", func() {
	series := func(registry *prometheus.Registry) []*dto.Metric {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		for _, mf := range mfs {
			if mf.GetName() == constants.WVAModelScalingBlocked {
				return mf.GetMetric()
			}
		}
		return nil
	}

	// A model that goes away has no producer left to clear it, and its last reason
	// asserts that a workload which no longer exists will never park. Unlike the
	// per-producer clear, this one crosses ownership on purpose.
	It("removes every reason for a model, whoever set it", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlockedReasons("ns-a", "gone", constants.ScalingBlockedReasonsPolicy,
			[]string{constants.ScalingBlockedVariantFloor})
		SetModelScalingBlockedReasons("ns-a", "gone", constants.ScalingBlockedReasonsWake,
			[]string{constants.ScalingBlockedNoWakeSignal})
		SetModelScalingBlockedReasons("ns-a", "stays", constants.ScalingBlockedReasonsPolicy,
			[]string{constants.ScalingBlockedVariantFloor})

		ClearModelScalingBlocked("ns-a", "gone")

		got := series(registry)
		Expect(got).To(HaveLen(1))
		Expect(getLabelValue(got[0], constants.LabelModelName)).To(Equal("stays"))
	})

	It("is a no-op before InitMetrics rather than a panic", func() {
		modelScalingBlocked = nil
		Expect(func() { ClearModelScalingBlocked("ns", "m") }).NotTo(Panic())
	})
})
