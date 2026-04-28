package discovery

import (
	"context"
	"errors"
	"sync"
)

// Recorder persists [Hit] records. The dispatcher calls Record once
// per Hit emitted by [Sniff]. Implementations are responsible for
// idempotency on ContractID — a single discovered contract should
// produce one row, not one row per event.
//
// Production wiring: a Postgres adapter against the
// `discovered_assets` table. In-memory [InMemoryRecorder] satisfies
// the same contract for tests + the dev binary.
//
// Errors are surfaced so the caller can decide whether to log +
// continue or stop the dispatcher. For the discovery flow specifically,
// the standard policy is "log + continue" — a recorder outage
// shouldn't block event processing; the discovered contract will
// re-appear in subsequent events and the next event will retry the
// write.
type Recorder interface {
	// Record persists or updates the hit. Returns nil on success.
	// Should be idempotent on Hit.ContractID — repeated Record
	// calls for the same contract update event_count + last-seen,
	// they don't produce duplicate rows.
	Record(ctx context.Context, hit Hit) error

	// IsKnown reports whether a contract has already been recorded.
	// Optional fast-path — callers MAY use this to short-circuit
	// discovery for hot contracts without a write round-trip.
	// A returning impl that doesn't track membership efficiently
	// can return (false, nil) and let Record handle the dedupe.
	IsKnown(ctx context.Context, contractID string) (bool, error)
}

// ErrAlreadyKnown is returned by [Recorder.Record] implementations
// that prefer to surface the known-contract case as an error rather
// than a silent no-op. Production Postgres uses INSERT ... ON
// CONFLICT and never returns this; the in-memory variant uses it for
// ergonomic test assertions.
var ErrAlreadyKnown = errors.New("discovery: contract already recorded")

// InMemoryRecorder is a [Recorder] backed by a sync.Map. Used by
// tests and by the dev binary's "scratch" mode. NOT suitable for
// production — restarts lose all discovered contracts and there's
// no cross-process synchronisation.
//
// Safe for concurrent calls.
type InMemoryRecorder struct {
	mu    sync.Mutex
	hits  map[string]Hit
	count map[string]int
}

// NewInMemoryRecorder constructs an empty in-memory recorder.
func NewInMemoryRecorder() *InMemoryRecorder {
	return &InMemoryRecorder{
		hits:  make(map[string]Hit),
		count: make(map[string]int),
	}
}

// Record stores or updates the hit. The first observation per
// contract is preserved verbatim (so first_seen_at / first_seen_event
// stay stable); subsequent observations only increment the counter.
// Mirrors what a Postgres ON CONFLICT DO UPDATE counterpart will do.
func (r *InMemoryRecorder) Record(_ context.Context, hit Hit) error {
	if hit.ContractID == "" {
		return errors.New("discovery: cannot record hit with empty ContractID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.hits[hit.ContractID]; !ok {
		r.hits[hit.ContractID] = hit
	}
	r.count[hit.ContractID]++
	return nil
}

// IsKnown reports whether contractID has been recorded.
func (r *InMemoryRecorder) IsKnown(_ context.Context, contractID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.hits[contractID]
	return ok, nil
}

// Snapshot returns a copy of the recorded hits. Used by tests to
// assert on-disk state without exposing the internal map. Slice
// order is unspecified; callers sort if they need determinism.
func (r *InMemoryRecorder) Snapshot() []Hit {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Hit, 0, len(r.hits))
	for _, h := range r.hits {
		out = append(out, h)
	}
	return out
}

// Count returns how many times Record was invoked for contractID
// (zero for never-seen). Lets tests verify Record is being called
// per-event rather than once-per-contract.
func (r *InMemoryRecorder) Count(contractID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count[contractID]
}
