package steadystate

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// The capacity-build step is the sole owner of model-level supply, utilization
// and RoleCapacities: analyzers emit only (D, P) plus per-role demand, and the
// builder assembles the rest from the same VariantCapacities. These specs pin
// that assembly down directly, since no analyzer publishes those fields anymore.
var _ = Describe("buildCapacities", func() {
	// No thresholds in play for the assembly specs: scaleUp=1, scaleDown=1 keeps
	// applyUniversalThreshold's arithmetic trivial so the assertions isolate the
	// supply/utilization/role assembly.
	const (
		noScaleUp   = 1.0
		noScaleDown = 1.0
	)

	It("is a no-op on a nil result", func() {
		Expect(func() { buildCapacities(ctx, nil, nil, noScaleUp, noScaleDown) }).NotTo(Panic())
	})

	It("assembles model-level supply from each variant's (ReplicaCount, P)", func() {
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				TotalDemand: 12000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000},
					{VariantName: "v2", ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 4000},
				},
			},
		}
		buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)

		Expect(r.TotalSupply).To(Equal(14000.0))            // 2×5000 + 1×4000
		Expect(r.TotalAnticipatedSupply).To(Equal(19000.0)) // (2+1)×5000 + 1×4000
	})

	It("derives utilization as demand over supply", func() {
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				TotalDemand: 7000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 2, PerReplicaCapacity: 5000},
				},
			},
		}
		buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
		Expect(r.Utilization).To(Equal(0.7)) // 7000 / 10000
	})

	It("leaves utilization at zero when there is no supply (no divide-by-zero)", func() {
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				TotalDemand: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 0, PerReplicaCapacity: 5000},
				},
			},
		}
		buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
		Expect(r.TotalSupply).To(BeZero())
		Expect(r.Utilization).To(BeZero())
	})

	It("overwrites any stale supply/utilization an analyzer may have set", func() {
		// Analyzers are contractually required to leave these zero; if one sets
		// them anyway the builder's derived values must win.
		r := &allocation.NamedAnalyzerResult{
			TotalSupply:            999999,
			TotalAnticipatedSupply: 888888,
			Utilization:            42,
			Result: &domain.AnalyzerResult{
				TotalDemand: 1000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 2000},
				},
			},
		}
		buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
		Expect(r.TotalSupply).To(Equal(2000.0))
		Expect(r.TotalAnticipatedSupply).To(Equal(2000.0))
		Expect(r.Utilization).To(Equal(0.5))
	})

	It("joins discovery identity onto the per-variant capacities", func() {
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 1000},
				},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"v1": {VariantName: "v1", Cost: 12.5, AcceleratorName: "H100", Role: "decode"},
		}
		buildCapacities(ctx, r, meta, noScaleUp, noScaleDown)

		// Only Role is joined now: cost and accelerator are not on VariantCapacity
		// at all, and the optimizer reads them from VariantMetadata instead.
		vc := r.Result.VariantCapacities[0]
		Expect(vc.Role).To(Equal("decode"))
	})

	Describe("RoleCapacities assembly", func() {
		It("returns nil RoleCapacities when the analyzer emitted no per-role demand", func() {
			// Non-disaggregated: the optimizer falls back to the model-level totals.
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					TotalDemand: 5000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", Role: domain.RoleBoth, ReplicaCount: 1, PerReplicaCapacity: 10000},
					},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
			Expect(r.RoleCapacities).To(BeNil())
		})

		It("pairs the analyzer's per-role demand with per-role supply", func() {
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					TotalDemand: 12000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "p1", Role: "prefill", ReplicaCount: 1, PendingReplicas: 1, PerReplicaCapacity: 5000},
						{VariantName: "d1", Role: "decode", ReplicaCount: 2, PerReplicaCapacity: 4000},
					},
					RoleDemand: map[string]float64{"prefill": 4000, "decode": 8000},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)

			Expect(r.RoleCapacities).To(HaveLen(2))

			prefill := r.RoleCapacities["prefill"]
			Expect(prefill.Role).To(Equal("prefill"))
			Expect(prefill.TotalSupply).To(Equal(5000.0))             // 1×5000
			Expect(prefill.TotalAnticipatedSupply).To(Equal(10000.0)) // (1+1)×5000
			Expect(prefill.TotalDemand).To(Equal(4000.0))             // from RoleDemand

			decode := r.RoleCapacities["decode"]
			Expect(decode.TotalSupply).To(Equal(8000.0))
			Expect(decode.TotalAnticipatedSupply).To(Equal(8000.0))
			Expect(decode.TotalDemand).To(Equal(8000.0))
		})

		It("keeps a 'both' bucket in a mixed fleet so the optimizer can find it", func() {
			// cost_aware_optimizer looks up RoleCapacities[state.Role] with an empty
			// role canonicalized to "both". A "both" variant in an otherwise
			// disaggregated model must therefore get its own bucket rather than
			// silently falling back to the model-level signals.
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					TotalDemand: 9000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "p1", Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 5000},
						{VariantName: "b1", Role: domain.RoleBoth, ReplicaCount: 2, PerReplicaCapacity: 3000},
					},
					RoleDemand: map[string]float64{"prefill": 4000, domain.RoleBoth: 5000},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)

			Expect(r.RoleCapacities).To(HaveKey(domain.RoleBoth))
			both := r.RoleCapacities[domain.RoleBoth]
			Expect(both.Role).To(Equal(domain.RoleBoth))
			Expect(both.TotalSupply).To(Equal(6000.0)) // 2×3000
			Expect(both.TotalDemand).To(Equal(5000.0))
		})

		It("groups an empty-role variant into the 'both' bucket's supply", func() {
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
						{VariantName: "legacy", Role: "", ReplicaCount: 1, PerReplicaCapacity: 1000},
					},
					RoleDemand: map[string]float64{"decode": 2000, domain.RoleBoth: 500},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
			Expect(r.RoleCapacities[domain.RoleBoth].TotalSupply).To(Equal(1000.0))
		})

		It("yields zero supply for a role the analyzer charged but no variant serves", func() {
			// Demand attributed to a role with no variants behind it produces a
			// zero-supply bucket rather than being dropped, so the shortfall stays
			// visible to the threshold post-step.
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
					},
					RoleDemand: map[string]float64{"decode": 2000, "prefill": 1500},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)

			prefill := r.RoleCapacities["prefill"]
			Expect(prefill.TotalSupply).To(BeZero())
			Expect(prefill.TotalDemand).To(Equal(1500.0))
		})

		It("uses the discovery-joined role, not the analyzer's, when grouping supply", func() {
			// Step (1) overwrites Role from discovery before step (3) groups by it,
			// so per-role supply follows the authoritative identity.
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 4000},
					},
					RoleDemand: map[string]float64{"decode": 1000},
				},
			}
			meta := map[string]domain.VariantMetadata{
				"v1": {VariantName: "v1", Role: "decode"},
			}
			buildCapacities(ctx, r, meta, noScaleUp, noScaleDown)

			Expect(r.RoleCapacities["decode"].TotalSupply).To(Equal(4000.0))
		})
	})
})
