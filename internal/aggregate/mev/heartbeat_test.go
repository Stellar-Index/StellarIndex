package mev

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

// TestWorker_Heartbeat_FullOnlyWhenEveryHashResolved pins the beat's meaning: full needs a wired
// resolver that returned every ordering hash; anything less, including a run that never reached the
// write, is partial or absent.
func TestWorker_Heartbeat_FullOnlyWhenEveryHashResolved(t *testing.T) {
	// Sandwich-shaped group: three trades, two takers, one taker on two txs, so the lake is asked
	// for every hash.
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GVIC", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txC, taker: "GATK", base: "native", quote: usdc}),
	}
	scanner := &fakeScanner{trades: trades, usd: usdLegs("5.00", "5.00", "5.00")}
	hashes := OrderingTxHashes(trades, nil)
	if len(hashes) == 0 {
		t.Fatal("fixture yields no ordering hashes")
	}

	if got := runWithHeartbeat(t, scanner, nil); len(got) != 1 || got[0] {
		t.Errorf("no resolver: beats = %v, want [false]", got)
	}
	if got := runWithHeartbeat(t, scanner, fakeOrder{}); len(got) != 1 || !got[0] {
		t.Errorf("resolver, every hash: beats = %v, want [true]", got)
	}
	if got := runWithHeartbeat(t, scanner, fakeOrder{err: errors.New("lake down")}); len(got) != 1 || got[0] {
		t.Errorf("resolver error: beats = %v, want [false]", got)
	}
	drop := map[string]bool{hashes[0]: true}
	if got := runWithHeartbeat(t, scanner, fakeOrder{drop: drop}); len(got) != 1 || got[0] {
		t.Errorf("one hash missing: beats = %v, want [false]", got)
	}
	if got := runWithHeartbeat(t, &fakeScanner{err: errors.New("db down")}, fakeOrder{}); len(got) != 0 {
		t.Errorf("scan error: beats = %v, want none", got)
	}
}

func TestWorker_Heartbeat_NoHashesIsFull(t *testing.T) {
	if got := runWithHeartbeat(t, &fakeScanner{}, fakeOrder{}); len(got) != 1 || !got[0] {
		t.Errorf("empty window with resolver: beats = %v, want [true]", got)
	}
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
