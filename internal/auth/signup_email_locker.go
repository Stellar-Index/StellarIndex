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

// RedisSignupEmailLocker is the Redis-SETNX EmailLocker serialising first-login provisioning per hashed email; Release is a Lua compare-and-delete on a per-acquire fencing token so an overrunning holder cannot delete a successor's lock.
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

// signupLockKey returns the Redis key for an email-hash, separate from `signupKey` so the namespaces do not collide; the Redis ACL
// allow-list already permits `signup:*`.
func signupLockKey(emailHash string) string {
	return "signup:lock:" + emailHash
}

// Acquire is SETNX with a fencing token and ttl, returning (true, token, nil) on win, (false, "", nil) on contention, (false, "", err) on Redis or entropy failure (the caller falls through unlocked).
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

// Release is a compare-and-delete: the key goes only if it still carries this caller's token; an empty token or an expired or absent key is a no-op.
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
