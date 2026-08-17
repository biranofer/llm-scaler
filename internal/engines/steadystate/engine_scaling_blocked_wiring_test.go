package steadystate

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// scaleToZeroBlockReasons is unit-tested directly and the metric is unit-tested
// directly; what neither covers is that applyScaleToZeroEnforcement actually
// CALLS them, with the right owned set, on every path that returns early. Those
// early returns are the whole risk: the first implementation emitted below the
// empty-decisions return, so a quiet cycle — exactly when a stale "will never
// park" series is most misleading — published nothing at all.
var _ = Describe("applyScaleToZeroEnforcement blocked-reason wiring", func() {
	const (
		modelID   = "wired-model"
		namespace = "wired-ns"
	)

	var ctx = context.Background()

	target := func(image string) scaletarget.ScaleTargetAccessor {
		return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "server", Image: image}},
					},
				},
			},
		})
	}

	vllm := map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")}
	sglang := map[string]scaletarget.ScaleTargetAccessor{"a": target("lmsysorg/sglang:latest")}

	// engineFor builds an engine with its own metric registry, so the reasons
	// gathered below are only the ones this spec published.
	engineFor := func(scaleToZero bool) (*Engine, *prometheus.Registry) {
		registry := prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())

		cfg := config.NewTestConfig()
		cfg.UpdateScalingPolicyConfigForNamespace(namespace, map[string]config.ScalingPolicy{
			"default": {ScaleToZero: &config.ScaleToZeroEnvelope{
				Enabled: ptrTo(scaleToZero), RetentionPeriod: "10m",
			}},
		})
		return &Engine{
			Config:            cfg,
			lastBlockedModels: make(map[string]blockedModelRef),
			ScaleToZeroEnforcer: allocation.NewEnforcer(
				func(context.Context, string, string, time.Duration) (float64, error) {
					return 0, nil // idle
				},
			),
		}, registry
	}

	publishedReasons := func(registry *prometheus.Registry) []string {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		var out []string
		for _, mf := range mfs {
			if mf.GetName() != constants.WVAModelScalingBlocked {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == constants.LabelReason {
						out = append(out, l.GetValue())
					}
				}
			}
		}
		return out
	}

	decisions := func() []domain.VariantDecision {
		return []domain.VariantDecision{
			{VariantName: "v1", ModelID: modelID, Namespace: namespace, Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
		}
	}

	floored := func() []domain.VariantReplicaState {
		min := 1
		return []domain.VariantReplicaState{{VariantName: "v1", MinReplicas: &min}}
	}

	permitsZero := func() []domain.VariantReplicaState {
		min := 0
		return []domain.VariantReplicaState{{VariantName: "v1", MinReplicas: &min}}
	}

	It("reports variant-floor when scale-to-zero is enabled and a variant is floored", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, floored())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedVariantFloor))
	})

	It("reports policy-forbids-zero when every variant permits zero but the policy does not", func() {
		e, registry := engineFor(false)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedPolicyForbidsZero))
	})

	It("reports engine-unsupported for a non-vLLM model that would otherwise park", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), sglang, permitsZero())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedEngineUnsupported))
	})

	It("publishes nothing for a coherent configuration", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(BeEmpty())
	})

	// The regression that prompted this file. The emission must sit ABOVE the
	// empty-decisions return: the reasons are configuration reconciled against
	// discovered bounds and do not depend on there being a decision to enforce.
	It("still reports when the optimizer produced no decisions at all", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			nil, vllm, floored())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedVariantFloor),
			"a quiet cycle is exactly when a stale reason is most misleading")
	})

	// Clearing is the other half of the contract: the series must vanish when the
	// operator fixes the configuration, not linger until the process restarts.
	It("clears the reason once the contradiction is resolved", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, floored())
		Expect(publishedReasons(registry)).NotTo(BeEmpty())

		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(BeEmpty())
	})

	// The enforcer owns only the policy reasons. If it cleared everything, it
	// would erase the scale-from-zero loop's verdict on every interval.
	It("leaves the wake reason alone", func() {
		e, registry := engineFor(true)
		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsWake,
			[]string{constants.ScalingBlockedNoWakeSignal})

		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedNoWakeSignal))
	})
})

// wva_model_replicas is the join key the symptom alert needs, so the wiring
// matters as much as the metric: an omitted sample is an alert that never fires.
var _ = Describe("applyScaleToZeroEnforcement model replica wiring", func() {
	const (
		modelID   = "replica-model"
		namespace = "replica-ns"
	)

	var ctx = context.Background()

	vllm := map[string]scaletarget.ScaleTargetAccessor{
		"a": scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "server", Image: "vllm/vllm-openai:latest"}},
					},
				},
			},
		}),
	}

	engine := func() (*Engine, *prometheus.Registry) {
		registry := prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
		cfg := config.NewTestConfig()
		cfg.UpdateScalingPolicyConfigForNamespace(namespace, map[string]config.ScalingPolicy{
			"default": {ScaleToZero: &config.ScaleToZeroEnvelope{
				Enabled: ptrTo(true), RetentionPeriod: "10m",
			}},
		})
		return &Engine{
			Config:            cfg,
			lastBlockedModels: make(map[string]blockedModelRef),
			ScaleToZeroEnforcer: allocation.NewEnforcer(
				func(context.Context, string, string, time.Duration) (float64, error) {
					return 1, nil // busy: leave the decisions alone
				},
			),
		}, registry
	}

	replicaSamples := func(registry *prometheus.Registry) []float64 {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		var out []float64
		for _, mf := range mfs {
			if mf.GetName() != constants.WVAModelReplicas {
				continue
			}
			for _, m := range mf.GetMetric() {
				out = append(out, m.GetGauge().GetValue())
			}
		}
		return out
	}

	states := func(replicas ...int) []domain.VariantReplicaState {
		out := make([]domain.VariantReplicaState, 0, len(replicas))
		for i, r := range replicas {
			min := 0
			out = append(out, domain.VariantReplicaState{
				VariantName:     string(rune('a' + i)),
				CurrentReplicas: r,
				MinReplicas:     &min,
			})
		}
		return out
	}

	It("sums replicas across a model's variants", func() {
		e, registry := engine()
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			nil, vllm, states(2, 3))

		Expect(replicaSamples(registry)).To(ConsistOf(float64(5)))
	})

	It("publishes zero for a parked model", func() {
		e, registry := engine()
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			nil, vllm, states(0, 0))

		Expect(replicaSamples(registry)).To(ConsistOf(float64(0)),
			"zero is the sample the symptom alert reads; omitting it disables the alert")
	})

	// A model whose variants have not been discovered yet is UNKNOWN, not at zero.
	// Publishing 0 would claim it is parked, and the symptom alert would fire for a
	// model that is merely still being discovered.
	It("says nothing when no variant state is known", func() {
		e, registry := engine()
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			nil, vllm, nil)

		Expect(replicaSamples(registry)).To(BeEmpty())
	})

	It("clears the series when the model goes away", func() {
		e, registry := engine()
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			nil, vllm, states(1))
		Expect(replicaSamples(registry)).NotTo(BeEmpty())

		e.pruneBlockedModels(map[string]bool{"other-ns/other-model": true})

		Expect(replicaSamples(registry)).To(BeEmpty(),
			"a deleted workload must not keep a parked-at-zero sample alive")
	})
})
