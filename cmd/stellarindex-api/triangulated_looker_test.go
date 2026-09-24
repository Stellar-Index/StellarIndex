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
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	rdb.AddHook(&writeBetweenReadsHook{valKey: valKey, write: func() {
		_ = mr.Set(provKey, cachekeys.VWAPProvenanceTriangulated)
		_ = mr.Set(valKey, "0.080000000000")
	}})

	val, triangulated, found, err := redisTriangulatedLooker{rdb: rdb}.LookupTriangulatedVWAP(ctx, xlm, gbp, window)
	if err != nil || !found {
		t.Fatalf("lookup = (found=%v, err=%v)", found, err)
	}
	consistent := (val == "0.090000000000" && !triangulated) || (val == "0.080000000000" && triangulated)
	if !consistent {
		t.Errorf("lookup = (%q, triangulated=%v): a value paired with another write's provenance", val, triangulated)
	}
}
