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

// PasskeyCeremonyGuard makes a WebAuthn ceremony challenge usable
// exactly once.
//
// Implementations:
//   - production: Redis SETNX (`auth.NewRedisPasskeyCeremonyGuard`),
//     wired in cmd/stellarindex-api when Redis is reachable so the
//     spent-set is shared across API instances.
//   - default: the in-process guard below, installed by
//     [Config.validate] when the field is left nil. Correct on a
//     single instance; behind more than one instance a replay routed
//     to a different instance would not be seen.
type PasskeyCeremonyGuard interface {
	// Consume claims the one-shot slot for a ceremony digest. It returns true for the first
	// presenter, false for a replay, and an error when the store is unreachable, which callers
	// MUST treat as "refuse". ttl only has to outlive the ceremony: after expiry the expiry
	// check refuses it regardless of the spent-set.
	Consume(ctx context.Context, digest string, ttl time.Duration) (bool, error)
}

// inProcessPasskeyCeremonyGuard is the single-instance default: a
// TTL-bounded set of spent ceremony digests in memory.
//
// Only a verified assertion/attestation spends a slot, so memory is bounded by successful
// ceremonies, not request volume. Not a substitute for the Redis guard on a multi-instance
// deployment; cmd/stellarindex-api warns when it falls back to this.
type inProcessPasskeyCeremonyGuard struct {
	now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // digest → moment the record may be dropped
}

func newInProcessPasskeyCeremonyGuard(now func() time.Time) *inProcessPasskeyCeremonyGuard {
	return &inProcessPasskeyCeremonyGuard{now: now, seen: map[string]time.Time{}}
}

// Consume implements [PasskeyCeremonyGuard]. Never errors: the store
// is this process's own memory.
func (g *inProcessPasskeyCeremonyGuard) Consume(_ context.Context, digest string, ttl time.Duration) (bool, error) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	// Sweep first so an expired record can't refuse a fresh ceremony
	// that happens to hash the same way (it can't — challenges are 32
	// random bytes — but the sweep is also what bounds the map).
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
