package decision

import (
	"sync"
	"time"
)

// Activations records when a model was last woken from zero, so the scale-to-zero
// path can leave it alone for its retention period.
//
// A model woken from zero has served nothing yet — the request that triggered the
// wake is still queued in the EPP while the pod pulls and loads. The scale-to-zero
// idle signal is a request counter over the retention window
// (sum(increase(vllm:request_success_total[retentionPeriod]))), which reads zero
// for exactly that model, so without this the enforcer sees a brand-new replica as
// idle and zeroes it again — the wake never survives long enough to serve the
// request that asked for it.
//
// Keyed by (namespace, modelID) rather than by scale target: retention is a
// per-model policy (config.ScaleToZeroRetentionPeriod), and a wake may bring up
// several targets at once (a P/D serving set), all of which must be held.
type Activations struct {
	mu sync.RWMutex
	m  map[string]time.Time
	// now is the clock, overridden in tests.
	now func() time.Time
}

// NewActivations returns an empty registry.
func NewActivations() *Activations {
	return &Activations{m: make(map[string]time.Time), now: time.Now}
}

func activationKey(namespace, modelID string) string {
	return namespace + "/" + modelID
}

// Mark records that the model was woken from zero now. Repeated calls extend the
// hold, which is what a still-pending queue should do: the engine re-publishes
// every poll while requests are waiting.
func (a *Activations) Mark(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[activationKey(namespace, modelID)] = a.now()
}

// Clear forgets the model's activation. Called once the model is genuinely
// serving, so normal idle accounting takes over.
func (a *Activations) Clear(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.m, activationKey(namespace, modelID))
}

// WithinRetention reports whether the model was woken from zero less than
// retention ago, and so must not be scaled back to zero yet. A model that was
// never woken (or whose hold has lapsed) reports false, leaving normal
// scale-to-zero accounting in charge.
//
// A non-positive retention disables the hold.
func (a *Activations) WithinRetention(namespace, modelID string, retention time.Duration) bool {
	if retention <= 0 {
		return false
	}
	a.mu.RLock()
	at, ok := a.m[activationKey(namespace, modelID)]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	return a.now().Sub(at) < retention
}

// DefaultActivations is the process-wide registry, written by the
// scale-from-zero engine and read by the scale-to-zero enforcement gate.
var DefaultActivations = NewActivations()

// MarkActivated records a wake-from-zero in the default registry.
func MarkActivated(namespace, modelID string) {
	DefaultActivations.Mark(namespace, modelID)
}

// WithinActivationRetention reports whether the default registry is still
// holding the model up after a wake-from-zero.
func WithinActivationRetention(namespace, modelID string, retention time.Duration) bool {
	return DefaultActivations.WithinRetention(namespace, modelID, retention)
}
