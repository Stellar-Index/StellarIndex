// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// SignupVerifier is the v1 boundary for the email-ownership-
// proof step on `POST /v1/signup`. F-1218 (codex audit-2026-05-12):
// the prior signup mints a usable Starter key from any valid-
// looking email string with no proof the submitter owns the
// inbox. The verifier seam closes the loop: the handler issues
// a single-use plaintext token, sends it via email, and the
// `GET /v1/signup/verify?token=…` handler consumes it.
//
// Implementations:
//   - production: [RedisSignupVerifier] (SETNX with TTL, key
//     family `signup:verify:<sha256(token)>` → keyID).
//   - tests / Redis-less dev: in-memory fakes.
//
// Reserve writes the token → keyID mapping; Consume reads + deletes
// in one round-trip (GETDEL) to make the token single-use.
type SignupVerifier interface {
	// Reserve persists the (token, keyID) mapping with the given
	// TTL. Returns [ErrSignupVerifyReserved] when the token is
	// already in flight (rare; the caller's plaintext is generated
	// from crypto/rand and the collision space is 256 bits).
	Reserve(ctx context.Context, token, keyID string, ttl time.Duration) error

	// Consume returns the keyID associated with `token` and
	// deletes the row in one round-trip. Returns
	// [ErrSignupVerifyNotFound] when the token isn't present
	// (expired, invalid, or already consumed).
	Consume(ctx context.Context, token string) (string, error)
}

// ErrSignupVerifyReserved is returned by Reserve when the
// generated plaintext token already maps to a different keyID.
// Should be effectively unreachable with crypto/rand 32-byte
// tokens — surfaces only on operator misconfiguration.
var ErrSignupVerifyReserved = errors.New("auth: signup verification token already reserved")

// ErrSignupVerifyNotFound is returned by Consume when the
// token isn't present in the verifier (expired, invalid, or
// already consumed). The handler maps this to 404 + a
// non-leaky error message.
var ErrSignupVerifyNotFound = errors.New("auth: signup verification token not found or already consumed")

// RedisSignupVerifier is the Redis-SETNX adapter for
// [SignupVerifier]. F-1218 (codex audit-2026-05-12).
//
// Key layout: `signup:verify:<sha256-hex>` → `<keyID>` with
// TTL. The plaintext token is hashed before persistence so a
// Redis dump can't reveal in-flight tokens (the hash is
// one-way; only the plaintext-bearing email recipient can
// reconstruct the key).
//
// TTL is the per-Reserve value; production wires
// [DefaultSignupVerifyTTL] (24 hours) so a customer who reads
// the email later in the day can still complete verification.
type RedisSignupVerifier struct {
	rdb redis.Cmdable
}

// DefaultSignupVerifyTTL is the recommended Reserve TTL when
// the caller doesn't pass a custom value. 24 hours matches the
// dashboard magic-link TTL and gives operators / customers
// breathing room across a workday.
const DefaultSignupVerifyTTL = 24 * time.Hour

// NewRedisSignupVerifier constructs a verifier. rdb MUST be
// non-nil — the api binary only wires this when Redis is
// reachable.
func NewRedisSignupVerifier(rdb redis.Cmdable) *RedisSignupVerifier {
	if rdb == nil {
		panic("auth: NewRedisSignupVerifier: rdb must not be nil")
	}
	return &RedisSignupVerifier{rdb: rdb}
}

// signupVerifyKey returns the Redis key for a token. Hashing
// happens at the boundary so callers pass plaintext + the
// stored row never sees it. Kept disjoint from the F-1218
// reservation namespace (`signup:email:`) and the F-1255
// per-email lock (`signup:lock:`) — three distinct purposes,
// three disjoint namespaces.
func signupVerifyKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "signup:verify:" + hex.EncodeToString(sum[:])
}

// Reserve implements [SignupVerifier.Reserve]. Plaintext token
// in, hashed key out, SETNX with the supplied TTL. A pre-
// existing row with a DIFFERENT keyID returns
// ErrSignupVerifyReserved (won't happen in practice with
// crypto/rand tokens). A pre-existing row with the SAME keyID
// is treated as idempotent (the caller may legitimately retry
// after a transient network blip).
func (v *RedisSignupVerifier) Reserve(ctx context.Context, token, keyID string, ttl time.Duration) error {
	if token == "" || keyID == "" {
		return errors.New("auth: SignupVerifier.Reserve: token and keyID are required")
	}
	if ttl <= 0 {
		ttl = DefaultSignupVerifyTTL
	}
	key := signupVerifyKey(token)
	ok, err := v.rdb.SetNX(ctx, key, keyID, ttl).Result()
	if err != nil {
		return fmt.Errorf("redis setnx %s: %w", key, err)
	}
	if !ok {
		// SETNX lost — read the existing value to check for the
		// idempotent retry case.
		existing, getErr := v.rdb.Get(ctx, key).Result()
		if getErr != nil {
			return fmt.Errorf("redis get %s: %w", key, getErr)
		}
		if existing != keyID {
			return ErrSignupVerifyReserved
		}
		// Same keyID → idempotent retry; refresh the TTL so the
		// customer's window doesn't shrink because they retried.
		if err := v.rdb.Expire(ctx, key, ttl).Err(); err != nil {
			return fmt.Errorf("redis expire %s: %w", key, err)
		}
	}
	return nil
}

// Consume implements [SignupVerifier.Consume]. GETDEL is
// atomic in Redis 6.2+: the read + delete happen in one
// round-trip so two concurrent verify-callbacks for the same
// token can't both succeed (the loser sees ErrSignupVerifyNotFound).
//
// Empty / unknown tokens yield ErrSignupVerifyNotFound — the
// handler maps both to a non-leaky 404, distinguished from
// "real Redis error" only via the wrapped error.
func (v *RedisSignupVerifier) Consume(ctx context.Context, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", ErrSignupVerifyNotFound
	}
	key := signupVerifyKey(token)
	val, err := v.rdb.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrSignupVerifyNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redis getdel %s: %w", key, err)
	}
	if val == "" {
		return "", ErrSignupVerifyNotFound
	}
	return val, nil
}

// NewSignupVerifyToken returns a fresh URL-safe plaintext token
// suitable for inclusion in a verification-email link. 32 bytes
// of crypto/rand → 64 hex chars; the SignupVerifier's
// [signupVerifyKey] hashes this further before persistence.
//
// Callers that already have a token-generation path (e.g. the
// dashboard-auth Generator) can pass their own plaintext into
// `Reserve` instead — the verifier doesn't impose a format on
// the token itself, only that it's stable.
func NewSignupVerifyToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("auth: NewSignupVerifyToken: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
