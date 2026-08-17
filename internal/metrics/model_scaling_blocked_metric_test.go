package metrics

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var _ = Describe("SetModelScalingBlocked", func() {
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

	It("emits one series per reason, at 1", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlocked("ns-a", "Qwen/Qwen3-0.6B", []string{
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

		SetModelScalingBlocked("ns-a", "m", []string{
			constants.ScalingBlockedVariantFloor,
			constants.ScalingBlockedEngineUnsupported,
		})
		SetModelScalingBlocked("ns-a", "m", []string{constants.ScalingBlockedVariantFloor})

		Expect(reasons(registry)).To(ConsistOf(constants.ScalingBlockedVariantFloor))
	})

	It("removes every series for a model once nothing blocks it", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlocked("ns-a", "m", []string{constants.ScalingBlockedPolicyForbidsZero})
		SetModelScalingBlocked("ns-a", "m", nil)

		Expect(series(registry)).To(BeEmpty(),
			"a cleared reason must vanish, not linger at 0")
	})

	// The clear is scoped by DeletePartialMatch on namespace+model, so it must not
	// reach a sibling. Getting this wrong is silent: the surviving model simply
	// stops reporting, and nothing says so.
	It("clears only the model it was called for", func() {
		registry := prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())

		SetModelScalingBlocked("ns-a", "m1", []string{constants.ScalingBlockedVariantFloor})
		SetModelScalingBlocked("ns-a", "m2", []string{constants.ScalingBlockedVariantFloor})
		SetModelScalingBlocked("ns-b", "m1", []string{constants.ScalingBlockedVariantFloor})

		SetModelScalingBlocked("ns-a", "m1", nil)

		got := series(registry)
		Expect(got).To(HaveLen(2))
		for _, m := range got {
			ns := getLabelValue(m, constants.LabelNamespace)
			model := getLabelValue(m, constants.LabelModelName)
			Expect(ns + "/" + model).NotTo(Equal("ns-a/m1"))
		}
	})

	It("is a no-op before InitMetrics rather than a panic", func() {
		modelScalingBlocked = nil
		Expect(func() {
			SetModelScalingBlocked("ns", "m", []string{constants.ScalingBlockedVariantFloor})
		}).NotTo(Panic())
	})
})
