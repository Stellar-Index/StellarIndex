package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const offenderContract = "C_NONSTANDARD_DECIMALS_FIXTURE"

type toggleDecimalsReader struct{ err error }

func (r *toggleDecimalsReader) LoadNonstandardDecimalsAssets(context.Context) ([]timescale.NonstandardDecimalsAsset, error) {
	if r.err != nil {
		return nil, r.err
	}
	return []timescale.NonstandardDecimalsAsset{{Asset: offenderContract, Decimals: 6}}, nil
}

// Q198: the first load must complete before the prime returns, so nothing
// constructed after it can observe a cold cache.
func TestPrimeNonstandardDecimalsCache_LoadsBeforeReturning(t *testing.T) {
	cache := v1.NewNonstandardDecimalsCache(&toggleDecimalsReader{}, nil)
	check := wiring.PrimeNonstandardDecimalsCache(context.Background(), cache, discardLogger())

	if d, ok := cache.Lookup(offenderContract); !ok || d != 6 {
		t.Fatalf("Lookup after prime = (%d, %v), want (6, true)", d, ok)
	}
	if err := check.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after successful prime: %v", err)
	}
	if !check.Critical() {
		t.Fatal("nonstandard-decimals readiness must be critical: a cold cache serves wrong prices")
	}
}

// Q198: a failed first load keeps the instance out of rotation until a load
// succeeds; a later failure keeps the last-good snapshot and stays ready.
func TestNonstandardDecimalsChecker_NotReadyUntilFirstLoad(t *testing.T) {
	reader := &toggleDecimalsReader{err: errors.New("pg down")}
	cache := v1.NewNonstandardDecimalsCache(reader, nil)
	check := wiring.PrimeNonstandardDecimalsCache(context.Background(), cache, discardLogger())

	if err := check.Ping(context.Background()); err == nil {
		t.Fatal("Ping on a never-loaded cache = nil, want not-ready")
	}

	reader.err = nil
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := check.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after first successful load: %v", err)
	}

	reader.err = errors.New("pg blip")
	_ = cache.Refresh(context.Background())
	if err := check.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after a later failed refresh: %v (last-good must stay ready)", err)
	}
}
