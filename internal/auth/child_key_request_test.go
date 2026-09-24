// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestChildKeyRequest_InheritsEveryField is the completeness guard for the
// delegation chokepoint: with every parent dimension set, no field of the
// resulting mint request may be left zero. A field added to
// CreateAPIKeyRequest without an inheritance line fails here, because a
// zero there reads as "unlimited" or "never expires".
func TestChildKeyRequest_InheritsEveryField(t *testing.T) {
	parent := Subject{
		Identifier:      "acct-parent",
		Tier:            TierOperator,
		Scopes:          []string{"read"},
		RateLimitPerMin: 120,
		MonthlyQuota:    50_000,
		ExpiresAt:       time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		EmailVerifiedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	req := ChildKeyRequest(parent, "rotated", []string{"read"})

	rv := reflect.ValueOf(req)
	for i := range rv.NumField() {
		if rv.Field(i).IsZero() {
			t.Errorf("ChildKeyRequest left CreateAPIKeyRequest.%s zero for a parent that sets it; "+
				"a delegated key must inherit every dimension of its parent", rv.Type().Field(i).Name)
		}
	}
	if !req.ExpiresAt.Equal(parent.ExpiresAt) {
		t.Errorf("child ExpiresAt = %v, want parent's %v", req.ExpiresAt, parent.ExpiresAt)
	}
}

// TestTimeBoxedKeyCannotMintPermanentChild drives the redis store and
// validator end to end: a parent minted with an expiry surfaces it on its
// Subject, the child minted from that Subject carries the same expiry, and
// the child stops authenticating when the parent does.
func TestTimeBoxedKeyCannotMintPermanentChild(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	mintedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := mintedAt.Add(30 * 24 * time.Hour)
	store := NewRedisAPIKeyStore(rdb, WithStoreClock(func() time.Time { return mintedAt }))
	_, parentPlain, err := store.Create(context.Background(), CreateAPIKeyRequest{
		Identifier: "staff-rotation",
		Tier:       TierOperator,
		ExpiresAt:  expiresAt,
	})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	beforeExpiry := NewRedisAPIKeyValidator(rdb, WithClock(func() time.Time { return expiresAt.Add(-time.Hour) }))
	parent, err := beforeExpiry.Lookup(context.Background(), parentPlain)
	if err != nil {
		t.Fatalf("Lookup parent: %v", err)
	}
	if !parent.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("parent Subject.ExpiresAt = %v, want %v", parent.ExpiresAt, expiresAt)
	}

	_, childPlain, err := store.Create(context.Background(), ChildKeyRequest(parent, "rotated", nil))
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	afterExpiry := NewRedisAPIKeyValidator(rdb, WithClock(func() time.Time { return expiresAt.Add(time.Hour) }))
	if _, err := afterExpiry.Lookup(context.Background(), childPlain); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("child Lookup after the parent's expiry: err = %v, want ErrTokenExpired "+
			"(a 30-day key must not mint a never-expiring successor)", err)
	}
}
