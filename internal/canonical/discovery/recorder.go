package discovery

import (
	"context"
	"errors"
	"sync"
)

// Recorder persists [Hit] records, idempotent on ContractID: one row per
// contract, not per event. Callers log and continue on error; the contract
// reappears on a later event and the write is retried.
type Recorder interface {
	// Record upserts the hit, updating event_count and last-seen.
	Record(ctx context.Context, hit Hit) error

	// IsKnown reports whether a contract is already recorded; an impl that cannot
	// answer cheaply may return (false, nil) and let Record dedupe.
	IsKnown(ctx context.Context, contractID string) (bool, error)
}

// ErrAlreadyKnown lets a Recorder surface a known contract as an error; the
// Postgres recorder upserts and never returns it.
var ErrAlreadyKnown = errors.New("discovery: contract already recorded")

// InMemoryRecorder is a concurrency-safe [Recorder] for tests and the dev
// binary; it loses everything on restart.
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
	delta := hit.Count
	if delta <= 0 {
		delta = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.hits[hit.ContractID]; !ok {
		r.hits[hit.ContractID] = hit
	}
	r.count[hit.ContractID] += int(delta)
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
