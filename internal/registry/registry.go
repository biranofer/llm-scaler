// Package registry holds the workloads WVA manages, discovered by being called
// about them rather than by looking for them.
//
// WVA used to find its variants by listing every HPA and ScaledObject in the
// cluster and keeping the ones bearing an annotation. That put WVA in the
// position of continuously asking "who is mine?" of an API server that has no
// efficient way to answer — the filter is an annotation, which no selector can
// express, so the answer was always "everything, then discard most of it".
//
// KEDA already tells us. Every external-scaler RPC carries the ScaledObject's
// namespace, name and trigger metadata, and KEDA only calls a scaler it has been
// pointed at. So the call is the registration: an object WVA has been called
// about is an object WVA manages, and one it has not been called about does not
// exist as far as WVA is concerned. No list, no watch, no annotation.
//
// See docs/plans/engine/keda-driven-discovery.md.
package registry

import (
	"maps"
	"sort"
	"sync"
	"time"
)

// DefaultTTL bounds how long an entry survives without being called about.
//
// It has to clear KEDA's polling comfortably. A `type: external` trigger refreshes
// its entry once per pollingInterval (KEDA's default is 30s), and the engines
// tolerate a variant briefly missing far better than they tolerate one that
// flickers in and out, so this errs long: five minutes is ten poll intervals, and
// an entry only reaches it when KEDA has genuinely stopped asking.
//
// It does not need to cover a paused or zero-replica workload on a push trigger.
// Those are held live by their open stream instead — see Hold.
const DefaultTTL = 5 * time.Minute

// Entry is one workload KEDA has called WVA about.
//
// Name is the ScaledObject's name, which is the identity WVA keys everything by:
// there is one ScaledObject per scale target, so it names the variant. The scale
// target itself is resolved separately, since it is the ScaledObject's business
// to declare it and it can be overridden in metadata.
type Entry struct {
	Namespace string
	Name      string
	// Metadata is the trigger's `metadata` map, exactly as KEDA delivered it.
	// It is the whole per-variant configuration surface — see scalermeta.
	Metadata map[string]string
	// FirstSeen is when this entry was first registered; it survives refreshes,
	// so it dates the workload rather than the last call about it.
	FirstSeen time.Time
	// LastSeen is the most recent call about this entry.
	LastSeen time.Time
	// Streaming reports whether a StreamIsActive is currently open for it, which
	// keeps it live regardless of LastSeen.
	Streaming bool
	// Target is what a read of the ScaledObject resolved, and Zero until one has
	// succeeded. See SetTarget.
	Target Target
	// TargetAt dates that read. Zero means the entry has never been enriched.
	TargetAt time.Time
}

// Target is the part of a ScaledObject WVA needs and the KEDA call does not
// carry: which workload to scale, and the envelope KEDA will hold it within.
//
// It is read from the object rather than taken from trigger metadata because
// KEDA owns these fields — min/max are what its HPA enforces, so duplicating
// them into metadata would create a second copy that can disagree with the one
// actually in force.
type Target struct {
	APIVersion  string
	Kind        string
	Name        string
	MinReplicas *int32
	MaxReplicas *int32
}

// Fresh reports whether the entry's target read is recent enough to use.
//
// Enrichment is deliberately decoupled from the call rate: the scale-from-zero
// loop runs at 10Hz and re-reading each ScaledObject at that rate — uncached, as
// it must be to avoid a cluster-wide informer — would put a request per variant
// per 100ms on the API server. The envelope changes about as often as someone
// edits a manifest, so a cadence in the tens of seconds loses nothing.
func (e Entry) Fresh(now time.Time, maxAge time.Duration) bool {
	return !e.TargetAt.IsZero() && now.Sub(e.TargetAt) <= maxAge
}

// Registry is the set of live entries. Safe for concurrent use: gRPC handlers
// write it from many streams at once while the engine loops read snapshots.
type Registry struct {
	mu  sync.Mutex
	m   map[string]*entry
	ttl time.Duration
	// now is the clock, overridden in tests.
	now func() time.Time
}

// entry is the stored form. holds counts open streams rather than recording a
// bool, because KEDA can hold more than one stream for the same object across a
// reconnect — the old one is not always closed before the new one opens, and a
// bool would let the stale close clear the fresh hold.
type entry struct {
	Entry
	holds int
}

// New returns an empty registry. A non-positive ttl uses DefaultTTL.
func New(ttl time.Duration) *Registry {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Registry{m: make(map[string]*entry), ttl: ttl, now: time.Now}
}

// Default is the process-wide registry, written by the external scaler and read
// by the engine loops.
var Default = New(DefaultTTL)

func key(namespace, name string) string { return namespace + "/" + name }

// Observe registers the workload, or refreshes it if already known. This is the
// whole of discovery: every external-scaler RPC calls it before doing anything
// else.
//
// Metadata is copied, not retained: it arrives on a protobuf message whose
// lifetime is the request's, and an entry outlives the call that made it.
//
// A refresh replaces the metadata rather than merging it, so editing a trigger
// takes effect on the next call instead of leaving removed keys behind forever.
// FirstSeen is preserved.
//
// **Changed metadata invalidates the entry's target read.** This is how WVA
// learns about an edited ScaledObject without watching one. KEDA rebuilds its
// scaler cache when a ScaledObject's generation changes — it re-issues
// GetMetricSpec and re-opens StreamIsActive — so a call carrying metadata
// different from what is stored is evidence that the OBJECT changed, not just
// that time passed. The fields WVA reads separately (scaleTargetRef, min/max)
// may well have changed in the same edit, so the cached read is dropped and the
// next enrichment pass re-reads immediately instead of serving a stale envelope
// for the rest of its window.
//
// It is not a complete substitute for a watch: an edit that touches ONLY
// min/max, leaving the trigger alone, still waits out the freshness window. That
// is the deliberate trade — see docs/plans/engine/keda-driven-discovery.md.
func (r *Registry) Observe(namespace, name string, metadata map[string]string) {
	if namespace == "" || name == "" {
		return
	}
	k := key(namespace, name)
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.m[k]
	if !ok {
		e = &entry{Entry: Entry{Namespace: namespace, Name: name, FirstSeen: now}}
		r.m[k] = e
	} else if !maps.Equal(e.Metadata, metadata) {
		// Zero the DATE, not the target. Clearing the target would drop the
		// variant out of the fleet until the next enrichment pass — a scaling gap
		// caused by an edit that may not even have touched the scale target. A
		// zero date reads as stale, so the next pass re-reads immediately, while
		// the last known envelope keeps serving in the meantime. Same policy as a
		// failed read, for the same reason.
		e.TargetAt = time.Time{}
	}
	e.Metadata = maps.Clone(metadata)
	e.LastSeen = now
}

// Hold marks the entry as having an open stream and returns the release. While
// held, the entry never expires.
//
// This exists because a workload parked at zero on a push trigger is called
// exactly once — StreamIsActive — and then never again until something happens.
// KEDA does not poll IsActive on a push trigger, and the HPA does not query
// metrics for a workload it is not scaling, so LastSeen would age out the very
// entries whose whole purpose is to be woken from zero.
//
// Hold registers the entry too: the stream may be the first call WVA sees.
// Release is idempotent.
func (r *Registry) Hold(namespace, name string, metadata map[string]string) func() {
	if namespace == "" || name == "" {
		return func() {}
	}
	r.Observe(namespace, name, metadata)

	k := key(namespace, name)
	r.mu.Lock()
	if e, ok := r.m[k]; ok {
		e.holds++
		e.Streaming = true
	}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			e, ok := r.m[k]
			if !ok {
				return
			}
			if e.holds > 0 {
				e.holds--
			}
			if e.holds == 0 {
				e.Streaming = false
				// Deliberately NOT deleted here. A closed stream means KEDA is not
				// streaming any more, not that the workload is gone — KEDA closes
				// and re-opens streams across its own reconciles. Let the TTL
				// decide, so a reconnect inside the window is a refresh rather
				// than a re-discovery.
				e.LastSeen = r.now()
			}
		})
	}
}

// Snapshot returns the live entries, evicting any that have expired.
//
// Eviction happens here rather than on a timer because this is the only place
// staleness matters: an expired entry that nobody reads costs a map slot, and
// pruning on read means there is no background goroutine to own, stop or leak.
//
// Ordered by namespace then name so that engine logs, metrics and any
// iteration-order-sensitive behaviour are reproducible across cycles.
func (r *Registry) Snapshot() []Entry {
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Entry, 0, len(r.m))
	for k, e := range r.m {
		if !e.Streaming && now.Sub(e.LastSeen) > r.ttl {
			delete(r.m, k)
			continue
		}
		copied := e.Entry
		copied.Metadata = maps.Clone(e.Metadata)
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get returns one entry if it is live. Expiry is left to Snapshot: a single
// lookup has no business mutating the set, and an expired entry read here is
// reported absent either way.
func (r *Registry) Get(namespace, name string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.m[key(namespace, name)]
	if !ok {
		return Entry{}, false
	}
	if !e.Streaming && r.now().Sub(e.LastSeen) > r.ttl {
		return Entry{}, false
	}
	copied := e.Entry
	copied.Metadata = maps.Clone(e.Metadata)
	return copied, true
}

// SetTarget records what a read of the entry's ScaledObject resolved.
//
// Kept on the entry rather than in a second map so that enrichment cannot
// outlive the entry it describes: expiry drops both together, and a workload
// that comes back is re-read rather than resuming against whatever its envelope
// used to be.
//
// A no-op for an entry that is not registered — the read was for a workload KEDA
// has since stopped asking about, and reviving it here would put a variant back
// in the fleet that no call justifies.
func (r *Registry) SetTarget(namespace, name string, t Target) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.m[key(namespace, name)]
	if !ok {
		return
	}
	e.Target = t
	e.TargetAt = r.now()
}

// TTL reports how long an entry survives without being called about. Read by the
// Enricher, which has to refresh well inside it.
func (r *Registry) TTL() time.Duration { return r.ttl }

// Len reports how many entries are held, live or not. For metrics and tests.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

// Forget removes an entry outright. Used when a call proves the object is gone
// (a NotFound on the targeted read), where waiting out the TTL would keep a
// deleted workload in the fleet the optimizer is balancing.
func (r *Registry) Forget(namespace, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, key(namespace, name))
}
