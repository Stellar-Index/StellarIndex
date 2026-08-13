package dashboardauth

// Single-use enforcement for WebAuthn ceremony challenges.
//
// The ceremony cookie (passkey.go) is integrity-bound and — since
// audit-2026-08-13 — time-bound. Neither property makes it
// SINGLE-USE: an HMAC stays valid for as long as the payload lives,
// so a captured `finish-login` request (ceremony cookie + assertion
// body) could be re-sent for the rest of the challenge's lifetime,
// minting a fresh session cookie every time. WebAuthn's whole replay
// story rests on the challenge being spent exactly once; nothing in
// the protocol enforces that for the relying party — the RP must
// remember.
//
// So the RP remembers: after an assertion verifies, the ceremony's
// digest claims a slot, and a second presentation of the same
// ceremony finds the slot taken and is refused. The mechanism is
// deliberately the same one the SEP-10 challenge replay guard uses
// (`internal/auth/sep10/redisreplay.go`, F-1224): Redis SETNX with a
// TTL, claimed AFTER signature verification so bogus submissions
// can't spend slots, and FAIL CLOSED when the store can't be reached
// — an authentication we cannot prove is fresh is not an
// authentication we issue a session for.

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
	// Consume claims the one-shot slot for a ceremony digest.
	// Returns true when the caller is the FIRST to present that
	// ceremony, false when the slot is already taken (a replay), and
	// a non-nil error when the backing store could not be reached.
	// Callers MUST treat the error as "refuse" — see the fail-closed
	// rationale above.
	//
	// ttl bounds how long a spent ceremony is remembered. It only has
	// to outlive the ceremony itself: once the challenge is expired,
	// the expiry check refuses it regardless of the spent-set.
	Consume(ctx context.Context, digest string, ttl time.Duration) (bool, error)
}

// inProcessPasskeyCeremonyGuard is the single-instance default: a
// TTL-bounded set of spent ceremony digests in memory.
//
// Memory is bounded by (successful ceremonies per TTL window), not by
// request volume — only a VERIFIED assertion/attestation spends a
// slot — so the map holds single digits on a real deployment and the
// per-call sweep is free. It is deliberately NOT a substitute for the
// Redis guard on a multi-instance deployment; cmd/stellarindex-api
// logs a warning when it falls back to this.
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
