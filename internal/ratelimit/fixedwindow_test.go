// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
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

// dropExpireHook fails every standalone EXPIRE and passes the rest through, modelling a reset/MISCONF/OOM between INCR and
// EXPIRE; it cannot intercept an EXPIRE inside a Lua script, which is the point.
type dropExpireHook struct{ dropped int }

func (h *dropExpireHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *dropExpireHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "expire" {
			h.dropped++
			err := errors.New("simulated transport failure on EXPIRE")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

func (h *dropExpireHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestFixedWindowCounter_TTLSurvivesDroppedExpire: a separate best-effort EXPIRE after INCR, if dropped, leaves a key with no
// TTL that nothing revisits, leaking permanent keys into the throttle namespace. INCR and EXPIRE must be one Lua EVAL so Redis
// runs both or neither.
func TestFixedWindowCounter_TTLSurvivesDroppedExpire(t *testing.T) {
	rdb, mr := newRedis(t)
	hook := &dropExpireHook{}
	rdb.AddHook(hook)

	fakeNow := time.Unix(1_700_000_000, 0).UTC()
	c := ratelimit.NewFixedWindowCounter(rdb, time.Hour, func() time.Time { return fakeNow })

	if _, err := c.Incr(context.Background(), "signup-ip:203.0.113.9"); err != nil {
		t.Fatalf("incr: %v", err)
	}

	key := "signup-ip:203.0.113.9:" + strconv.FormatInt(fakeNow.Unix()/3600, 10)
	if ttl := mr.TTL(key); ttl != 2*time.Hour {
		t.Fatalf("TTL(%s) = %v, want %v — the drain TTL must be set atomically with the "+
			"increment that creates the key, not by a droppable follow-up EXPIRE (REL-05)",
			key, ttl, 2*time.Hour)
	}
	if hook.dropped != 0 {
		t.Errorf("client issued %d standalone EXPIRE command(s); the increment must be a "+
			"single atomic call", hook.dropped)
	}
}
