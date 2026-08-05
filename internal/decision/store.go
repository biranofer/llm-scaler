// Package decision holds the latest per-target scaling decision in memory so it
// can be shared between the optimize pipeline (writer) and the KEDA external
// scaler (reader) without a round-trip through the Kubernetes API or Prometheus.
//
// The VariantAutoscaling CRD has been removed (variants are synthesized in-memory
// each cycle), so the desired-replica decision is not persisted anywhere the
// external scaler could read. This store mirrors the value the actuator emits to
// the wva_desired_replicas gauge, keyed by the scale target's namespace/name.
//
// A process-wide Default store is used, mirroring the package-level metrics
// registries already in this codebase.
package decision

import (
	"sync"
	"time"
)

// Decision is the latest scaling decision WVA computed for one scale target.
type Decision struct {
	// DesiredReplicas is the optimizer's target replica count.
	DesiredReplicas int32
	// UpdatedAt is when the decision was last written.
	UpdatedAt time.Time
}

// Store keeps the latest Decision per scale target, keyed by namespace/name
// (the Deployment/LWS name, equal to the former VariantAutoscaling name). It is
// safe for concurrent use.
type Store struct {
	mu sync.RWMutex
	m  map[string]Decision
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{m: make(map[string]Decision)}
}

func storeKey(namespace, name string) string {
	return namespace + "/" + name
}

// Set records the latest desired replica count for a scale target.
func (s *Store) Set(namespace, name string, desiredReplicas int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[storeKey(namespace, name)] = Decision{
		DesiredReplicas: desiredReplicas,
		UpdatedAt:       time.Now(),
	}
}

// Get returns the latest Decision for a scale target and whether one exists.
func (s *Store) Get(namespace, name string) (Decision, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.m[storeKey(namespace, name)]
	return d, ok
}

// Default is the process-wide decision store shared between the actuator (writer)
// and the external scaler (reader).
var Default = NewStore()

// Set records a decision in the Default store.
func Set(namespace, name string, desiredReplicas int32) {
	Default.Set(namespace, name, desiredReplicas)
}

// Get reads a decision from the Default store.
func Get(namespace, name string) (Decision, bool) {
	return Default.Get(namespace, name)
}
