// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
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
// Redis). Value is a fixed sentinel — only the existence of the
// key matters; ownership is implicit because the lock is held
// only while a single goroutine processes the callback. We rely
// on TTL expiry rather than a per-caller token because the
// dashboardauth handler always defers Release on the success
// path; the TTL is the safety net for a process crash between
// Acquire and Release.
type RedisSignupEmailLocker struct {
	rdb redis.Cmdable
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
// SETNX with the given TTL. Returns (true, nil) on win,
// (false, nil) on contention, and (false, err) on a Redis-side
// failure. The caller treats the err case as "fall through to
// the legacy non-locked path" rather than refusing to log in.
func (l *RedisSignupEmailLocker) Acquire(ctx context.Context, emailHash string, ttl time.Duration) (bool, error) {
	ok, err := l.rdb.SetNX(ctx, signupLockKey(emailHash), "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx %s: %w", signupLockKey(emailHash), err)
	}
	return ok, nil
}

// Release implements the dashboardauth.EmailLocker contract.
// DEL on the lock key. `redis.Nil` is treated as a successful
// no-op — if the TTL already elapsed (e.g. the caller spent
// longer than 30s in Account.Create + Users.CreateUser) the
// lock is already gone and Release has nothing to do.
func (l *RedisSignupEmailLocker) Release(ctx context.Context, emailHash string) error {
	_, err := l.rdb.Del(ctx, signupLockKey(emailHash)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("redis del %s: %w", signupLockKey(emailHash), err)
	}
	return nil
}
