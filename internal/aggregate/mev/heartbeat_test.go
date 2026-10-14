package mev

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

type recordingHeartbeat struct{ beats []bool }

func (h *recordingHeartbeat) Beat(_ context.Context, full bool) { h.beats = append(h.beats, full) }

// fakeOrder resolves every requested hash except those in drop; err short-circuits.
type fakeOrder struct {
	drop map[string]bool
	err  error
}

func (f fakeOrder) TxIndexes(_ context.Context, hashes []string) (map[string]uint32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]uint32, len(hashes))
	for i, h := range hashes {
		if !f.drop[h] {
			out[h] = uint32(i) //nolint:gosec // test index, bounded by len(hashes)
		}
	}
	return out, nil
}

func runWithHeartbeat(t *testing.T, scanner *fakeScanner, order TxOrderResolver) []bool {
	t.Helper()
	hb := &recordingHeartbeat{}
	w := NewWorker(scanner, &fakeSink{}, WorkerConfig{Heartbeat: hb, Order: order})
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	if _, _, err := w.RunOnce(context.Background(), now); err != nil && scanner.err == nil {
		t.Fatalf("RunOnce: %v", err)
	}
	return hb.beats
}

func newHeartbeatRedis(t *testing.T) (*miniredis.Miniredis, *RedisHeartbeat) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	hb, err := NewRedisHeartbeat(HeartbeatOptions{Cache: rdb})
	if err != nil {
		t.Fatal(err)
	}
	return mr, hb
}

func TestRedisHeartbeat_RoundTripAndTTL(t *testing.T) {
	ctx := context.Background()
	mr, hb := newHeartbeatRedis(t)

	full, present, err := hb.Pulse(ctx)
	if err != nil || full || present {
		t.Fatalf("empty: (%v,%v,%v), want (false,false,nil)", full, present, err)
	}
	hb.Beat(ctx, true)
	if full, present, err = hb.Pulse(ctx); err != nil || !full || !present {
		t.Fatalf("after full beat: (%v,%v,%v), want (true,true,nil)", full, present, err)
	}
	if ttl := mr.TTL(cachekeys.MEVHeartbeat().String()); ttl != cachekeys.MEVHeartbeatTTL {
		t.Errorf("ttl = %v, want %v", ttl, cachekeys.MEVHeartbeatTTL)
	}
	hb.Beat(ctx, false)
	if full, present, err = hb.Pulse(ctx); err != nil || full || !present {
		t.Fatalf("after partial beat: (%v,%v,%v), want (false,true,nil)", full, present, err)
	}
	mr.FastForward(cachekeys.MEVHeartbeatTTL + time.Second)
	if full, present, err = hb.Pulse(ctx); err != nil || full || present {
		t.Fatalf("after expiry: (%v,%v,%v), want (false,false,nil)", full, present, err)
	}
}

func TestRedisHeartbeat_ReadErrorIsReturned(t *testing.T) {
	mr, hb := newHeartbeatRedis(t)
	hb.Beat(context.Background(), true)
	mr.Close()
	if _, _, err := hb.Pulse(context.Background()); err == nil {
		t.Fatal("Pulse after redis closed: want error, got nil")
	}
}

func TestNewRedisHeartbeat_RequiresCache(t *testing.T) {
	if _, err := NewRedisHeartbeat(HeartbeatOptions{}); err == nil {
		t.Fatal("nil cache accepted")
	}
}
