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

// RedisSignupEmailLocker is the Redis-SETNX dashboardauth EmailLocker: it serialises first-login provisioning per email (key `signup:lock:<sha256-hex>`, hashed by the handler so no plaintext email reaches Redis).
// The value is a per-acquire fencing token and Release is a Lua compare-and-delete, so a holder that overruns the 30s TTL cannot delete its successor's lock and reopen the orphan-Account race; the TTL covers a crash.
type RedisSignupEmailLocker struct {
	rdb redis.Cmdable
}

// releaseLockScript DELs the key only when it still equals the caller's fencing token, atomically in one Lua eval.
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

// NewRedisSignupEmailLocker constructs a locker; rdb must be non-nil. Redis-less deployments leave the EmailLocker field nil and use the Suspend-on-conflict path.
func NewRedisSignupEmailLocker(rdb redis.Cmdable) *RedisSignupEmailLocker {
	if rdb == nil {
		panic("auth: NewRedisSignupEmailLocker: rdb must not be nil")
	}
	return &RedisSignupEmailLocker{rdb: rdb}
}

// signupLockKey returns the Redis key for an email-hash. Kept
// separate from `signupKey` (the signup reservation key) so the
// two namespaces don't collide; the Redis ACL allow-list already
// permits the `signup:*` family.
func signupLockKey(emailHash string) string {
	return "signup:lock:" + emailHash
}

// Acquire implements dashboardauth.EmailLocker: SETNX with a fencing token and ttl, returning (true, token, nil) on win, (false, "", nil) on contention, (false, "", err) on Redis or entropy failure.
// The caller treats err as "fall through unlocked" and passes the token back to Release.
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

// Release implements dashboardauth.EmailLocker as compare-and-delete: the key goes only if it still carries this caller's token, so an overrunning holder cannot delete a successor's lock.
// An empty token (never held), an expired key or an absent key is a harmless no-op.
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
