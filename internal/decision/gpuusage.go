package decision

import (
	"sync"
	"time"
)

// GPUUsage is a snapshot of how many GPUs are currently in use, as the
// saturation engine accounted for them on its last cycle.
//
// It exists so the scale-from-zero engine can ask whether a variant it is about
// to wake would actually get GPUs, without standing up a second accounting path.
// ConstraintProvider.ComputeConstraints takes usage as an input, and the
// saturation engine already computes exactly these two maps every cycle; a
// second implementation summing replicas independently would be free to drift
// from the one the optimizer actually honours, and the two engines would then
// disagree about what is free.
type GPUUsage struct {
	// ByType maps accelerator type to GPUs in use cluster-wide.
	ByType map[string]int
	// ByNamespace maps namespace to accelerator type to GPUs in use. Its key set
	// also defines which namespaces get namespace-scoped caps materialised.
	ByNamespace map[string]map[string]int
	// TakenAt is when the snapshot was published.
	TakenAt time.Time
}

// GPUUsageStore holds the latest GPUUsage snapshot.
type GPUUsageStore struct {
	mu   sync.RWMutex
	snap *GPUUsage
	now  func() time.Time
}

// NewGPUUsageStore returns an empty store.
func NewGPUUsageStore() *GPUUsageStore {
	return &GPUUsageStore{now: time.Now}
}

// Publish records a new snapshot. The maps are deep-copied, so the caller may
// keep mutating its own.
func (s *GPUUsageStore) Publish(byType map[string]int, byNamespace map[string]map[string]int) {
	snap := &GPUUsage{
		ByType:      make(map[string]int, len(byType)),
		ByNamespace: make(map[string]map[string]int, len(byNamespace)),
	}
	for k, v := range byType {
		snap.ByType[k] = v
	}
	for ns, perType := range byNamespace {
		cp := make(map[string]int, len(perType))
		for k, v := range perType {
			cp[k] = v
		}
		snap.ByNamespace[ns] = cp
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	snap.TakenAt = s.now()
	s.snap = snap
}

// Get returns the latest snapshot and whether one has been published. The
// returned value is the stored copy and must not be mutated.
//
// A false second return means the saturation engine has not completed a cycle
// yet. Callers must treat that as "unknown", not "nothing is in use": denying a
// wake because usage is unknown would keep a model down for the first cycle
// after every restart, which is exactly when a queued request is waiting.
func (s *GPUUsageStore) Get() (*GPUUsage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snap == nil {
		return nil, false
	}
	return s.snap, true
}

// DefaultGPUUsage is the process-wide snapshot store, written by the saturation
// engine and read by the scale-from-zero engine.
var DefaultGPUUsage = NewGPUUsageStore()

// PublishGPUUsage records a snapshot in the default store.
func PublishGPUUsage(byType map[string]int, byNamespace map[string]map[string]int) {
	DefaultGPUUsage.Publish(byType, byNamespace)
}

// LatestGPUUsage reads the default store's snapshot.
func LatestGPUUsage() (*GPUUsage, bool) {
	return DefaultGPUUsage.Get()
}
