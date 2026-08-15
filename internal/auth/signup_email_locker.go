// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisSignupEmailLocker is the Redis-SETNX adapter for the
// dashboardauth EmailLocker seam (F-1255, codex audit-2026-05-12).
// It serialises first-login provisioning per email so two
// /v1/auth/callback callers can't both create speculative
// Account rows before the email-unique-index Users insert
// resolves a winner.
//
// Key layout: `signup:lock:<sha256-hex>` (the dashboardauth
// handler does the hashing so the plaintext email never reaches
// Redis). The value is a per-acquire random FENCING token, not a
// fixed sentinel: Acquire stores it via SETNX and Release deletes
// the key only if it still carries that token (a Lua
// compare-and-delete). This closes F-C (audit-2026-08-14): if a
// holder overruns the 30s TTL (a slow Account.Create +
// Users.CreateUser), the key can expire and a concurrent caller
// can SETNX-acquire it afresh; a blind DEL would then delete the
// SUCCESSOR's lock, reintroducing the F-1255 orphan race. The
// token-scoped delete makes the overrunning holder's Release a
// no-op instead. The TTL remains the safety net for a process
// crash between Acquire and Release.
type RedisSignupEmailLocker struct {
	rdb redis.Cmdable
}

// releaseLockScript is a Redis compare-and-delete: it DELs the
// key only when its current value still equals the caller's
// fencing token. Running it as a single Lua eval makes the
// get-then-del atomic, so no successor's lock can be deleted
// between the check and the delete.
var releaseLockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// newLockToken returns 128 bits of hex-encoded randomness used as
// the per-acquire fencing token.
func newLockToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read lock token entropy: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// NewRedisSignupEmailLocker constructs a locker. rdb MUST be
// non-nil — the api binary only wires this when Redis is
// reachable. Redis-less deployments leave the dashboardauth
// EmailLocker field nil and fall back to the Suspend-on-conflict
// recovery path.
func NewRedisSignupEmailLocker(rdb redis.Cmdable) *RedisSignupEmailLocker {
	if rdb == nil {
		panic("auth: NewRedisSignupEmailLocker: rdb must not be nil")
	}
	return &RedisSignupEmailLocker{rdb: rdb}
}

// signupLockKey returns the Redis key for an email-hash. Kept
// separate from `signupKey` (the F-1218 reservation key) so the
// two namespaces don't collide; the ACL allow-list (F-1254)
// already permits the `signup:*` family.
func signupLockKey(emailHash string) string {
	return "signup:lock:" + emailHash
}

// Acquire implements the dashboardauth.EmailLocker contract.
// SETNX with a per-acquire fencing token and the given TTL.
// Returns (true, token, nil) on win, (false, "", nil) on
// contention, and (false, "", err) on a Redis-side (or entropy)
// failure. The caller treats the err case as "fall through to
// the legacy non-locked path" rather than refusing to log in,
// and passes the returned token back to Release.
func (l *RedisSignupEmailLocker) Acquire(ctx context.Context, emailHash string, ttl time.Duration) (bool, string, error) {
	token, err := newLockToken()
	if err != nil {
		return false, "", err
	}
	ok, err := l.rdb.SetNX(ctx, signupLockKey(emailHash), token, ttl).Result()
	if err != nil {
		return false, "", fmt.Errorf("redis setnx %s: %w", signupLockKey(emailHash), err)
	}
	if !ok {
		return false, "", nil
	}
	return true, token, nil
}

// Release implements the dashboardauth.EmailLocker contract.
// Compare-and-delete: the key is removed only if it still carries
// this caller's `token`, so a holder that overran its TTL cannot
// delete a successor's freshly-acquired lock (F-C). An empty
// token means the caller never held the lock, so Release is a
// no-op. A key that already TTL-expired (or was never set) leaves
// the CAD a harmless zero-match no-op.
func (l *RedisSignupEmailLocker) Release(ctx context.Context, emailHash, token string) error {
	if token == "" {
		return nil
	}
	err := releaseLockScript.Run(ctx, l.rdb, []string{signupLockKey(emailHash)}, token).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("redis cad %s: %w", signupLockKey(emailHash), err)
	}
	return nil
}
