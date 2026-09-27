package redisclient_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
)

func redisErrs(class string) float64 {
	return testutil.ToFloat64(obs.RedisCommandErrorsTotal.WithLabelValues(class))
}

func redisErrsAll() float64 {
	var n float64
	for _, c := range obs.RedisErrorClasses {
		n += redisErrs(c)
	}
	return n
}

// buildLive returns a Build()-constructed client whose single pooled
// connection is already established, so a later SetError only affects
// commands, not the connection handshake.
func buildLive(t *testing.T) (redis.UniversalClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redisclient.Build(config.StorageConfig{RedisAddr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return c, mr
}

func TestErrorHook_CountsMisconfWriteRefusal(t *testing.T) {
	c, mr := buildLive(t)
	ctx := context.Background()
	mr.SetError("MISCONF Redis is configured to save RDB snapshots, but it's currently unable to persist to disk")
	before := redisErrs("MISCONF")

	if err := c.Incr(ctx, "usage:k").Err(); err == nil {
		t.Fatal("INCR succeeded against a MISCONF server")
	}
	if got := redisErrs("MISCONF") - before; got != 1 {
		t.Fatalf("MISCONF delta after one command = %v, want 1", got)
	}

	_, _ = c.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Incr(ctx, "a")
		p.Incr(ctx, "b")
		return nil
	})
	if got := redisErrs("MISCONF") - before; got != 3 {
		t.Fatalf("MISCONF delta after a 2-command pipeline = %v, want 3", got)
	}
}

func TestErrorHook_UnknownPrefixIsOther(t *testing.T) {
	c, mr := buildLive(t)
	mr.SetError("FROBNICATED something odd")
	before := redisErrs("other")
	_ = c.Get(context.Background(), "k").Err()
	if got := redisErrs("other") - before; got != 1 {
		t.Fatalf("other delta = %v, want 1", got)
	}
}

func TestErrorHook_NilAndNoscriptAreNotErrors(t *testing.T) {
	c, _ := buildLive(t)
	ctx := context.Background()
	before := redisErrsAll()

	if err := c.Get(ctx, "absent").Err(); !errors.Is(err, redis.Nil) {
		t.Fatalf("GET absent = %v, want redis.Nil", err)
	}
	// First Run on a fresh server answers EVALSHA with NOSCRIPT and falls
	// back to EVAL — expected, not a failure.
	if err := redis.NewScript(`return 1`).Run(ctx, c, nil).Err(); err != nil {
		t.Fatalf("script run: %v", err)
	}
	if got := redisErrsAll() - before; got != 0 {
		t.Fatalf("errors counted for redis.Nil / NOSCRIPT fallback: delta = %v, want 0", got)
	}
}

func TestErrorHook_TransportFailureIsIOAndCountsPipelineOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()
	c := redisclient.Build(config.StorageConfig{RedisAddr: addr})
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	before := redisErrs("io")

	_ = c.Set(ctx, "k", "v", 0).Err()
	if got := redisErrs("io") - before; got != 1 {
		t.Fatalf("io delta after one command = %v, want 1", got)
	}
	_, _ = c.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Incr(ctx, "a")
		p.Incr(ctx, "b")
		return nil
	})
	if got := redisErrs("io") - before; got != 2 {
		t.Fatalf("io delta after a failed pipeline = %v, want 2 (transport failure counts once)", got)
	}
}

func TestErrorHook_CanceledContext(t *testing.T) {
	c, _ := buildLive(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := redisErrs("canceled")
	_ = c.Get(ctx, "k").Err()
	if got := redisErrs("canceled") - before; got != 1 {
		t.Fatalf("canceled delta = %v, want 1", got)
	}
}

// The baseline is taken before Build so the connection handshake is
// inside the measured window: go-redis clones the client's hooks onto the
// init conn, and miniredis, like Redis < 7.2, rejects CLIENT SETINFO.
func TestErrorHook_ConnectionHandshakeIsNotCounted(t *testing.T) {
	mr := miniredis.RunT(t)
	before := redisErrsAll()
	c := redisclient.Build(config.StorageConfig{RedisAddr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if got := redisErrsAll() - before; got != 0 {
		t.Fatalf("errors counted for a fresh connection's handshake: delta = %v, want 0", got)
	}
}

func TestErrorHook_TxPipelineCountsQueuedCommandsOnly(t *testing.T) {
	c, mr := buildLive(t)
	ctx := context.Background()
	mr.SetError("MISCONF Redis is configured to save RDB snapshots, but it's currently unable to persist to disk")
	before := redisErrs("MISCONF")

	if _, err := c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Incr(ctx, "a")
		p.Incr(ctx, "b")
		return nil
	}); err == nil {
		t.Fatal("MULTI/EXEC succeeded against a MISCONF server")
	}
	if got := redisErrs("MISCONF") - before; got != 2 {
		t.Fatalf("MISCONF delta after a 2-command MULTI/EXEC = %v, want 2", got)
	}
}
