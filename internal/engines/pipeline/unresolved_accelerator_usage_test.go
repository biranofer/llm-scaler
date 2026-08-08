package pipeline

import (
	"context"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
)

// These tests pin what happens to GPU usage reported against an UNRESOLVED
// accelerator, which is a real state: gpuUsageByType keys usage by
// VariantMetadata.AcceleratorName, and accelerator.GetAcceleratorNameFromScaleTarget
// falls back to constants.DefaultAcceleratorName ("unknown") when a workload
// carries neither a nodeSelector/nodeAffinity GPU key nor the
// inference.optimization/acceleratorName label.
//
// The behaviour is a KNOWN LIMITATION rather than something these tests assert
// as desirable: usage that cannot be attributed to a type cannot be charged to a
// pool, and the durable fix is to resolve the accelerator (see the
// accelerator-silent-failure and synthetic-VA notes). They exist so the gap is
// documented and cannot change silently, since both the GPU-aware optimizer and
// the scale-from-zero placement check read these pools.

// TestUnresolvedUsageNeverReachesAnyPool: GetResourcePools iterates the
// DISCOVERED types, so a usage entry under a placeholder key has nowhere to land
// and simply disappears. The per-type budgets the optimizer consumes therefore
// over-state free capacity by however many GPUs unresolved variants hold.
func TestUnresolvedUsageNeverReachesAnyPool(t *testing.T) {
	disc := &mockDiscovery{inventory: map[string]map[string]discovery.AcceleratorModelInfo{
		"node-a": {"H100": {Count: 8}},
	}}
	inv := NewTypeInventory("test", disc)
	if err := inv.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// 2 GPUs on a resolved type, 4 held by variants whose accelerator is unknown.
	inv.SetUsed(map[string]int{"H100": 2, constants.DefaultAcceleratorName: 4})

	pools := inv.GetResourcePools()
	if _, ok := pools[constants.DefaultAcceleratorName]; ok {
		t.Fatalf("pools unexpectedly contain a %q entry: %v", constants.DefaultAcceleratorName, pools)
	}

	h100, ok := pools["H100"]
	if !ok {
		t.Fatal("pools should contain the discovered H100 type")
	}
	if h100.Used != 2 {
		t.Fatalf("H100 Used = %d, want 2", h100.Used)
	}
	// 6 of 8 GPUs are actually occupied, but the budget the optimizer reads says 6
	// are free. This is the limitation.
	if got := h100.Available(); got != 6 {
		t.Fatalf("H100 Available = %d, want 6 (the over-stated budget this test documents)", got)
	}
}

// TestUnresolvedUsageStillCountsTowardTotals shows the other half: SetUsed sums
// every key into totalUsed, so the aggregate DOES include unresolved usage while
// the per-type pools do not. The two views of one inventory disagree, in opposite
// directions.
//
// This is latent rather than active: ResourceConstraints.TotalUsed/TotalAvail are
// populated by DefaultLimiter but nothing reads them — the optimizer merges Pools
// and NamespacePools only. It is pinned here so that a future consumer of the
// totals does not silently inherit an inconsistency.
//
// QuotaInventory deliberately avoids this: its TotalUsed counts only the key set
// TotalLimit does, "so the two stay symmetric". TypeInventory has no such guard,
// which is why this asymmetry looks like an oversight rather than a decision.
func TestUnresolvedUsageStillCountsTowardTotals(t *testing.T) {
	disc := &mockDiscovery{inventory: map[string]map[string]discovery.AcceleratorModelInfo{
		"node-a": {"H100": {Count: 8}},
	}}
	inv := NewTypeInventory("test", disc)
	if err := inv.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	inv.SetUsed(map[string]int{"H100": 2, constants.DefaultAcceleratorName: 4})

	if got := inv.TotalUsed(); got != 6 {
		t.Fatalf("TotalUsed = %d, want 6 (every key is summed, including the placeholder)", got)
	}

	// Sum of per-type pool usage, i.e. what the optimizer can actually see.
	poolUsed := 0
	for _, p := range inv.GetResourcePools() {
		poolUsed += p.Used
	}
	if poolUsed == inv.TotalUsed() {
		t.Fatalf("per-type pools (%d used) and TotalUsed (%d) agree; if unresolved usage is now "+
			"attributed or excluded consistently, update these tests and the limitation note",
			poolUsed, inv.TotalUsed())
	}
}
