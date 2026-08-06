package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

const orphanRoleDemandMsg = "analyzer attributed demand to a role with no variants"

// buildRoleCapacities warns when an analyzer keys demand by a role that no
// variant serves. That pairing depends on the analyzer's role view matching
// discovery's — an invariant the types cannot express — and a divergence would
// otherwise silently mint a zero-supply bucket that the threshold post-step
// reads as an unservable shortfall and turns into a spurious scale-up. These
// tests pin both that the warning fires when it should and that it stays quiet
// on the normal paths, so it can't decay into per-cycle log noise.
func TestBuildRoleCapacities_LogsDemandForRoleWithNoVariants(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	r := &domain.AnalyzerResult{
		AnalyzerName: "saturation",
		ModelID:      "m1",
		Namespace:    "ns1",
		VariantCapacities: []domain.VariantCapacity{
			{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
		},
		RoleDemand: map[string]float64{"decode": 2000, "prefill": 1500},
	}
	buildCapacities(ctx, r, nil, 1.0, 1.0)

	require.Equal(t, 1, logs.Len(), "expected exactly one warning for the orphaned role")
	entry := logs.All()[0]
	assert.Equal(t, orphanRoleDemandMsg, entry.Message)

	fields := entry.ContextMap()
	assert.Equal(t, "prefill", fields["role"])
	assert.Equal(t, "saturation", fields["analyzer"])
	// modelID/namespace make the line actionable: the engine reconciles many
	// models per cycle into a single controller log.
	assert.Equal(t, "m1", fields["modelID"])
	assert.Equal(t, "ns1", fields["namespace"])
	assert.EqualValues(t, 1500, fields["demand"])
	// ContextMap widens the []string field to []any.
	assert.Equal(t, []any{"decode"}, fields["variantRoles"])

	// The bucket is still built, so the shortfall stays visible downstream.
	assert.Zero(t, r.RoleCapacities["prefill"].TotalSupply)
	assert.EqualValues(t, 1500, r.RoleCapacities["prefill"].TotalDemand)
}

func TestBuildRoleCapacities_QuietWhenEveryRoleHasVariants(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	r := &domain.AnalyzerResult{
		AnalyzerName: "saturation",
		VariantCapacities: []domain.VariantCapacity{
			{VariantName: "p1", Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 5000},
			{VariantName: "d1", Role: "decode", ReplicaCount: 2, PerReplicaCapacity: 4000},
		},
		RoleDemand: map[string]float64{"prefill": 4000, "decode": 8000},
	}
	buildCapacities(ctx, r, nil, 1.0, 1.0)

	assert.Zero(t, logs.FilterMessage(orphanRoleDemandMsg).Len())
}

func TestBuildRoleCapacities_QuietWhenNonDisaggregated(t *testing.T) {
	// No RoleDemand at all: the builder returns nil RoleCapacities without
	// inspecting roles, so there is nothing to warn about.
	ctx, logs := zapObserverCtx(t)

	r := &domain.AnalyzerResult{
		AnalyzerName: "saturation",
		VariantCapacities: []domain.VariantCapacity{
			{VariantName: "v1", Role: domain.RoleBoth, ReplicaCount: 1, PerReplicaCapacity: 5000},
		},
	}
	buildCapacities(ctx, r, nil, 1.0, 1.0)

	assert.Nil(t, r.RoleCapacities)
	assert.Zero(t, logs.FilterMessage(orphanRoleDemandMsg).Len())
}

func TestBuildRoleCapacities_QuietWhenOrphanRoleHasZeroDemand(t *testing.T) {
	// A role with no variants AND no demand is not a scale-up risk, so it must
	// not warn — otherwise a fleet that legitimately drains a role to zero would
	// log every reconcile cycle.
	ctx, logs := zapObserverCtx(t)

	r := &domain.AnalyzerResult{
		AnalyzerName: "saturation",
		VariantCapacities: []domain.VariantCapacity{
			{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
		},
		RoleDemand: map[string]float64{"decode": 2000, "prefill": 0},
	}
	buildCapacities(ctx, r, nil, 1.0, 1.0)

	assert.Zero(t, logs.FilterMessage(orphanRoleDemandMsg).Len())
}
