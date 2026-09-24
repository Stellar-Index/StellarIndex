package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// writeBetweenReadsHook applies `write` once, right after the first command
// that reads the value key, standing in for an aggregator publish that lands
// while the reader is mid-lookup.
type writeBetweenReadsHook struct {
	valKey string
	write  func()
	done   bool
}

func (h *writeBetweenReadsHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *writeBetweenReadsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *writeBetweenReadsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if h.done {
			return err
		}
		for _, a := range cmd.Args()[1:] {
			if a == h.valKey {
				h.done = true
				h.write()
				break
			}
		}
		return err
	}
}

// TestLookupTriangulatedVWAP_ValueAndProvenanceAreOneSnapshot: a composite
// publish landing between the reader's value read and its provenance read
// must not produce the direct print labelled triangulated.
func TestLookupTriangulatedVWAP_ValueAndProvenanceAreOneSnapshot(t *testing.T) {
	ctx := context.Background()
	xlm := canonical.NativeAsset()
	gbp, err := canonical.NewFiatAsset("GBP")
	if err != nil {
		t.Fatalf("build fiat:GBP: %v", err)
	}
	window := time.Minute
	valKey := cachekeys.VWAP(xlm, gbp, window).String()
	provKey := cachekeys.VWAPProvenance(xlm, gbp, window).String()

	mr := miniredis.RunT(t)
	if err := mr.Set(valKey, "0.090000000000"); err != nil { // the direct print, no marker
		t.Fatal(err)
	}
	if err := mr.Set(cachekeys.VWAPObservedAt(xlm, gbp, window).String(), cachekeys.FormatVWAPObservedAt(time.Now())); err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(&writeBetweenReadsHook{valKey: valKey, write: func() {
		_ = mr.Set(provKey, cachekeys.VWAPProvenanceTriangulated)
		_ = mr.Set(valKey, "0.080000000000")
	}})

	v, found, err := redisTriangulatedLooker{rdb: rdb}.LookupTriangulatedVWAP(ctx, xlm, gbp, window)
	if err != nil || !found {
		t.Fatalf("lookup = (found=%v, err=%v)", found, err)
	}
	val, triangulated := v.Value, v.Triangulated
	consistent := (val == "0.090000000000" && !triangulated) || (val == "0.080000000000" && triangulated)
	if !consistent {
		t.Errorf("lookup = (%q, triangulated=%v): a value paired with another write's provenance", val, triangulated)
	}
}

// TestLookupTriangulatedVWAP_UnstampedValueIsAMiss: a value with no
// readable observed-at stamp has an unknowable age — a freeze keeps a
// value alive for its whole hold — so it is not served at all rather
// than stamped with the read time (RLT-357).
func TestLookupTriangulatedVWAP_UnstampedValueIsAMiss(t *testing.T) {
	ctx := context.Background()
	xlm := canonical.NativeAsset()
	gbp, err := canonical.NewFiatAsset("GBP")
	if err != nil {
		t.Fatalf("build fiat:GBP: %v", err)
	}
	window := 5 * time.Minute
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	looker := redisTriangulatedLooker{rdb: rdb}
	if err := mr.Set(cachekeys.VWAP(xlm, gbp, window).String(), "0.090000000000"); err != nil {
		t.Fatal(err)
	}
	atKey := cachekeys.VWAPObservedAt(xlm, gbp, window).String()

	for _, stamp := range []string{"", "not-a-time"} {
		if stamp != "" {
			if err := mr.Set(atKey, stamp); err != nil {
				t.Fatal(err)
			}
		}
		if v, found, err := looker.LookupTriangulatedVWAP(ctx, xlm, gbp, window); err != nil || found {
			t.Errorf("stamp %q: lookup = (%+v, found=%v, err=%v), want a miss", stamp, v, found, err)
		}
	}

	observed := time.Date(2026, 9, 24, 11, 40, 0, 0, time.UTC)
	if err := mr.Set(atKey, cachekeys.FormatVWAPObservedAt(observed)); err != nil {
		t.Fatal(err)
	}
	v, found, err := looker.LookupTriangulatedVWAP(ctx, xlm, gbp, window)
	if err != nil || !found || !v.ObservedAt.Equal(observed) {
		t.Errorf("stamped: lookup = (%+v, found=%v, err=%v), want observed_at %s", v, found, err, observed)
	}
}
