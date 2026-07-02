// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/ratelimit"
)

// TestFixedWindowCounter_IncrementsAndKeyShape pins the key derivation
// the auth throttles' Redis keys depend on: keyBase + ":" +
// unix/window bucket, counting per window.
func TestFixedWindowCounter_IncrementsAndKeyShape(t *testing.T) {
	rdb, mr := newRedis(t)
	fakeNow := time.Unix(1_700_000_000, 0).UTC()
	c := ratelimit.NewFixedWindowCounter(rdb, time.Hour, func() time.Time { return fakeNow })
	ctx := context.Background()

	for i := int64(1); i <= 3; i++ {
		n, err := c.Incr(ctx, "signup-ip:1.2.3.4")
		if err != nil {
			t.Fatalf("incr %d: %v", i, err)
		}
		if n != i {
			t.Errorf("count = %d, want %d", n, i)
		}
	}

	windowStart := fakeNow.Unix() / 3600
	key := "signup-ip:1.2.3.4:" + strconv.FormatInt(windowStart, 10)
	got, err := mr.Get(key)
	if err != nil {
		t.Fatalf("expected key %s in redis: %v", key, err)
	}
	if got != "3" {
		t.Errorf("key %s = %q, want %q", key, got, "3")
	}
	// Drain TTL set on first touch: 2× window.
	if ttl := mr.TTL(key); ttl != 2*time.Hour {
		t.Errorf("TTL = %v, want %v", ttl, 2*time.Hour)
	}
}

// TestFixedWindowCounter_WindowRollover — a new window bucket starts a
// fresh count.
func TestFixedWindowCounter_WindowRollover(t *testing.T) {
	rdb, _ := newRedis(t)
	fakeNow := time.Unix(1_700_000_000, 0).UTC()
	c := ratelimit.NewFixedWindowCounter(rdb, time.Hour, func() time.Time { return fakeNow })
	ctx := context.Background()

	if _, err := c.Incr(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Incr(ctx, "k"); err != nil {
		t.Fatal(err)
	}

	fakeNow = fakeNow.Add(time.Hour) // next bucket
	n, err := c.Incr(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("count after rollover = %d, want 1", n)
	}
}

// TestFixedWindowCounter_RedisErrorPropagates — transport failures
// surface as wrapped errors so callers can pick their fail-open /
// fail-closed policy.
func TestFixedWindowCounter_RedisErrorPropagates(t *testing.T) {
	rdb, mr := newRedis(t)
	c := ratelimit.NewFixedWindowCounter(rdb, time.Hour, nil)
	mr.Close()

	if _, err := c.Incr(context.Background(), "k"); err == nil {
		t.Fatal("want error after redis close, got nil")
	}
}
