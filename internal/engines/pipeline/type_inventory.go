package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
)

// TypeInventory tracks GPU capacity, usage, and availability per accelerator type (H100, A100, etc.).
//
// Unlike ClusterInventory which maintains a single pool of all GPUs, TypeInventory
// maintains separate pools for each accelerator type. This ensures that:
//   - H100 workloads can only use H100 GPUs
//   - A100 workloads can only use A100 GPUs
//   - Different accelerator types don't compete for the same pool
//
// The inventory tracks three values per accelerator type:
//   - Limit: total capacity discovered from the cluster
//   - Used: GPUs currently in use (discovered from pods or set manually)
//   - Available: Limit - Used (computed)
//
// This is essential for heterogeneous clusters where workloads have specific
// hardware requirements that cannot be satisfied by other accelerator types.
type TypeInventory struct {
	name           string
	discovery      discovery.CapacityDiscovery
	usageDiscovery discovery.UsageDiscovery // Optional: if set, RefreshAll will auto-discover usage

	mu sync.RWMutex
	// limitByType maps accelerator type (e.g., "H100", "A100") to total GPU capacity
	limitByType map[string]int
	// usedByType maps accelerator type to currently used GPU count
	usedByType map[string]int
	// totalLimit is the sum of all GPU capacity across types
	totalLimit int
	// totalUsed is the sum of all used GPUs across types
	totalUsed int
}

// NewTypeInventory creates a TypeInventory that tracks GPUs per accelerator type.
//
// Parameters:
//   - name: identifier for logging/metrics
//   - disc: interface to discover accelerator capacity from the cluster
//
// For automatic usage discovery, use NewTypeInventoryWithUsage instead.
func NewTypeInventory(name string, disc discovery.CapacityDiscovery) *TypeInventory {
	return &TypeInventory{
		name:        name,
		discovery:   disc,
		limitByType: make(map[string]int),
		usedByType:  make(map[string]int),
	}
}

// NewTypeInventoryWithUsage creates a TypeInventory with automatic usage discovery.
//
// Parameters:
//   - name: identifier for logging/metrics
//   - disc: interface implementing both CapacityDiscovery and UsageDiscovery
//
// When using this constructor, call RefreshAll() to update both limits and usage
// in a single operation.
func NewTypeInventoryWithUsage(name string, disc discovery.FullDiscovery) *TypeInventory {
	return &TypeInventory{
		name:           name,
		discovery:      disc,
		usageDiscovery: disc,
		limitByType:    make(map[string]int),
		usedByType:     make(map[string]int),
	}
}

// Name returns the inventory identifier.
func (i *TypeInventory) Name() string {
	return i.name
}

// RefreshAll updates both limits (capacity) and usage in a single operation.
//
// This is the preferred method when using NewTypeInventoryWithUsage.
// It discovers GPU capacity from nodes and calculates current usage from pods.
//
// Returns an error if usage discovery is not configured (use Refresh + SetUsed instead).
func (i *TypeInventory) RefreshAll(ctx context.Context) error {
	if i.usageDiscovery == nil {
		return errors.New("usage discovery not configured; use SetUsed() or NewTypeInventoryWithUsage()")
	}

	// Refresh limits first
	if err := i.Refresh(ctx); err != nil {
		return err
	}

	// Discover current usage
	usedByType, err := i.usageDiscovery.DiscoverUsage(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover GPU usage: %w", err)
	}

	// Update usage
	i.SetUsed(usedByType)

	return nil
}

// Refresh updates the inventory limits from the cluster using the discovery interface.
//
// This aggregates GPU capacity across all nodes for each accelerator type.
// Accelerator names are normalized from full model names (e.g., "NVIDIA-A100-PCIE-80GB")
// to short names (e.g., "A100") to match VA label conventions.
// Should be called before reading pools to ensure fresh data.
// Note: This only updates limits; call SetUsed or RefreshAll to update usage.
func (i *TypeInventory) Refresh(ctx context.Context) error {
	// Discover node -> accelerator type -> count
	nodeInventory, err := i.discovery.Discover(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover accelerator capacity: %w", err)
	}

	// Aggregate by accelerator type across all nodes
	// Normalize full model names to short names for matching with VA labels
	byType := make(map[string]int)
	total := 0

	for _, accelerators := range nodeInventory {
		for fullModelName, info := range accelerators {
			// Normalize "NVIDIA-A100-PCIE-80GB" -> "A100"
			shortName := accelerator.NormalizeAcceleratorName(fullModelName)
			byType[shortName] += info.Count
			total += info.Count
		}
	}

	i.mu.Lock()
	i.limitByType = byType
	i.totalLimit = total
	i.mu.Unlock()

	return nil
}

// SetUsed updates the used GPU counts per accelerator type.
// This should be called with current usage (e.g., from replica counts) before reading pools.
//
// Incoming keys are reconciled against the discovered limit keys, because the two
// sides are written in different vocabularies. Limits are keyed by NORMALIZED
// short names ("A100"); callers key usage by the name a WORKLOAD declares, which
// for the common nodeSelector deployment is the raw product label
// ("NVIDIA-A100-PCIE-80GB"). Without reconciliation such a key never matches a
// limit key, GetResourcePools reports Used = 0 for it, and every pool claims its
// full complement is free however much is actually running — so both the
// GPU-aware optimizer and the scale-from-zero placement check see an empty
// cluster and over-allocate.
//
// Usage that matches no discovered type is dropped rather than stored: it cannot
// be charged to a pool, so counting it in totalUsed would leave the aggregate
// contradicting the sum of the pools. Callers must therefore Refresh (which
// populates the limit keys) before SetUsed; ComputeConstraints does.
func (i *TypeInventory) SetUsed(usedByType map[string]int) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.usedByType = make(map[string]int, len(usedByType))
	total := 0
	for declared, count := range usedByType {
		key, ok := resolveAcceleratorKey(i.limitByType, declared)
		if !ok {
			// No discovered type owns this name — an unresolved accelerator, or a
			// type that has since left the cluster. Unattributable, so not counted.
			continue
		}
		i.usedByType[key] += count
		total += count
	}
	i.totalUsed = total
}

// TotalLimit returns total GPU capacity across all types.
func (i *TypeInventory) TotalLimit() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.totalLimit
}

// TotalUsed returns total GPUs currently in use across all types.
func (i *TypeInventory) TotalUsed() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.totalUsed
}

// TotalAvailable returns total available GPUs (Limit - Used) across all types.
func (i *TypeInventory) TotalAvailable() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	available := i.totalLimit - i.totalUsed
	if available < 0 {
		return 0
	}
	return available
}

// LimitByType returns the GPU capacity limit for a specific accelerator type.
func (i *TypeInventory) LimitByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.limitByType[accType]
}

// UsedByType returns the used GPU count for a specific accelerator type.
func (i *TypeInventory) UsedByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.usedByType[accType]
}

// AvailableByType returns available GPUs (Limit - Used) for a specific accelerator type.
func (i *TypeInventory) AvailableByType(accType string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	available := i.limitByType[accType] - i.usedByType[accType]
	if available < 0 {
		return 0
	}
	return available
}

// GetResourcePools returns per-type resource availability as ResourcePool structs.
func (i *TypeInventory) GetResourcePools() map[string]ResourcePool {
	i.mu.RLock()
	defer i.mu.RUnlock()

	pools := make(map[string]ResourcePool, len(i.limitByType))
	for accType, limit := range i.limitByType {
		used := i.usedByType[accType]
		pools[accType] = ResourcePool{
			Limit: limit,
			Used:  used,
		}
	}
	return pools
}

// AcceleratorTypes returns all known accelerator types.
func (i *TypeInventory) AcceleratorTypes() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()

	types := make([]string, 0, len(i.limitByType))
	for t := range i.limitByType {
		types = append(types, t)
	}
	return types
}

// Ensure TypeInventory implements Inventory interface
var _ Inventory = (*TypeInventory)(nil)
