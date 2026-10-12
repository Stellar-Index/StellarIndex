package dashboardauth

// Single-use enforcement for WebAuthn ceremony challenges.
//
// The ceremony cookie (passkey.go) is integrity-bound and time-bound but not single-use: a
// captured finish-login request could be re-sent for the challenge's whole lifetime, minting a
// fresh session each time. WebAuthn leaves replay protection to the relying party, so after an
// assertion verifies, the ceremony digest claims a slot and a second presentation is refused.
//
// Same mechanism as the SEP-10 replay guard (internal/auth/sep10/redisreplay.go): Redis SETNX
// with a TTL, claimed after signature verification so bogus submissions cannot spend slots, and
// fail closed when the store is unreachable.

import (
	"context"
	"sync"
	"time"
)

// PasskeyCeremonyGuard makes a WebAuthn ceremony challenge usable exactly once; production uses Redis SETNX, and the in-process default
// misses replays routed to another instance.
type PasskeyCeremonyGuard interface {
	// Consume claims the one-shot slot for a ceremony digest. It returns true for the first
	// presenter, false for a replay, and an error when the store is unreachable, which callers
	// MUST treat as "refuse". ttl only has to outlive the ceremony: after expiry the expiry
	// check refuses it regardless of the spent-set.
	Consume(ctx context.Context, digest string, ttl time.Duration) (bool, error)
}

// inProcessPasskeyCeremonyGuard is the single-instance default: a TTL-bounded in-memory set of spent digests.
// Only verified ceremonies spend a slot, so memory is bounded by successful ceremonies, not request volume.
type inProcessPasskeyCeremonyGuard struct {
	now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // digest → moment the record may be dropped
}

func newInProcessPasskeyCeremonyGuard(now func() time.Time) *inProcessPasskeyCeremonyGuard {
	return &inProcessPasskeyCeremonyGuard{now: now, seen: map[string]time.Time{}}
}

// Consume implements [PasskeyCeremonyGuard]; it never errors because the store is local memory.
func (g *inProcessPasskeyCeremonyGuard) Consume(_ context.Context, digest string, ttl time.Duration) (bool, error) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	// The sweep is what bounds the map.
	for d, expiry := range g.seen {
		if !expiry.After(now) {
			delete(g.seen, d)
		}
	}
	if _, spent := g.seen[digest]; spent {
		return false, nil
	}
	g.seen[digest] = now.Add(ttl)
	return true, nil
}

// Compile-time interface conformance check.
var _ PasskeyCeremonyGuard = (*inProcessPasskeyCeremonyGuard)(nil)
