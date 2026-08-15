//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// flakeyFXResolver implements timescale.USDVolumeFXResolver. It resolves the
// prices it knows unless `fail` is set, in which case it returns a transient
// error — the exact class (deadlock / statement-timeout / pool blip / Patroni
// failover) that VWAPUSDFXResolver.USDPriceAt surfaces and does NOT
// negative-cache, so each independent write is a fresh chance to miss.
type flakeyFXResolver struct {
	prices map[string]string
	fail   bool
}

func (r *flakeyFXResolver) USDPriceAt(_ context.Context, asset c.Asset, _ time.Time) (string, bool, error) {
	if r.fail {
		return "", false, errors.New("transient pg fault (deadlock/statement-timeout)")
	}
	p, ok := r.prices[asset.String()]
	if !ok {
		return "", false, nil
	}
	return p, true, nil
}

// TestUSDVolumeGenerationAwareNullPreservation is the proven-red regression
// for W1-flowtradeingest-1: in production persist_per_source=true, a DEX trade
// is DOUBLE-WRITTEN at deriveGeneration=0 by both the dispatcher's
// BatchInsertTrades and the projector's InsertTrade on the same store. If one
// writer resolves a usd_volume and the other races the same PK while its FX
// resolver hits a transient fault (returning nil), the pre-fix upsert
// (`usd_volume = EXCLUDED.usd_volume` gated only by
// `derive_generation <= EXCLUDED.derive_generation`, equal included) overwrote
// the populated value with NULL — permanently deflating the pair's volume.
//
// The fix is GENERATION-AWARE, not a blanket COALESCE: at the LIVE / equal
// generation a NULL incoming must NOT regress a populated value, but a
// strictly-HIGHER-generation re-derive is still allowed to write an honest
// NULL (store.go:147-155, the tier-3b de-poisoning case). This test pins both:
//
//   - EQUAL (gen-0) NULL must PRESERVE the stored value. FAILS on the unfixed
//     `usd_volume = EXCLUDED.usd_volume` (row goes NULL). To reproduce red:
//     revert the CASE in both writers to `usd_volume = EXCLUDED.usd_volume`.
//   - HIGHER-generation NULL must CLEAR the stored value. FAILS on the naive
//     unconditional `COALESCE(EXCLUDED.usd_volume, trades.usd_volume)` the
//     skeptic panel rejected (which would block the honest de-poisoning NULL).
func TestUSDVolumeGenerationAwareNullPreservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// A fresh SEP-41 base with no market (never resolvable) and a quote
	// token the resolver prices — the tier-3 FX path, where a transient miss
	// yields a nil usd_volume for the racing writer.
	base, err := c.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatalf("base asset: %v", err)
	}
	quote, err := c.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatalf("quote asset: %v", err)
	}
	pair, err := c.NewPair(base, quote)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	const (
		source = "soroswap" // SubclassDEX
		nonce  = 42
	)
	ledger := int64(50_000_000 + nonce)

	// 2.5 quote units (25,000,000 stroops at 1e7) x $2.00 = $5.00 — a small,
	// below-ceiling single-leg print, so W1-flow-price-serve-1's bound does
	// not apply and the value populates.
	res := &flakeyFXResolver{prices: map[string]string{quote.String(): "2.00"}}
	store.SetUSDVolumeFXResolver(res)

	readUSD := func() sql.NullString {
		const q = `SELECT usd_volume::text FROM trades WHERE source = $1 AND ledger = $2`
		var uv sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, source, ledger).Scan(&uv); err != nil {
			t.Fatalf("read usd_volume: %v", err)
		}
		return uv
	}
	isFive := func(uv sql.NullString) bool {
		return uv.Valid && (uv.String == "5.00000000" || uv.String == "5")
	}

	// Writer A, gen 0: resolves the quote leg → usd_volume = $5.00 lands.
	trA := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trA); err != nil {
		t.Fatalf("InsertTrade (writer A): %v", err)
	}
	if uv := readUSD(); !isFive(uv) {
		t.Fatalf("writer A: usd_volume = %v, want $5.00 populated", uv)
	}

	// Writer B, gen 0: same PK, but its resolver hits a transient fault →
	// computes NULL. At EQUAL generation the fix must PRESERVE $5.00.
	// Pre-fix (`usd_volume = EXCLUDED.usd_volume`) this row goes NULL.
	res.fail = true
	trB := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trB); err != nil {
		t.Fatalf("InsertTrade (writer B, transient miss): %v", err)
	}
	if uv := readUSD(); !isFive(uv) {
		t.Fatalf("EQUAL-generation NULL must NOT regress a populated value: "+
			"usd_volume = %v, want $5.00 preserved (W1-flowtradeingest-1)", uv)
	}

	// Higher-generation honest NULL (the tier-3b de-poisoning case): a
	// properly-wired re-derive at gen 5 that legitimately computes NULL must
	// be allowed to CLEAR the value. The reDeriveNullVolumeGuard requires
	// resolution to be installed for a gen>0 NULL write; install it (no-pegs
	// no-op is enough to flip the installed flag).
	if err := timescale.InstallUSDVolumeResolution(store, nil, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	store.SetDeriveGeneration(5)
	trC := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trC); err != nil {
		t.Fatalf("InsertTrade (writer C, gen-5 honest NULL): %v", err)
	}
	if uv := readUSD(); uv.Valid {
		t.Fatalf("HIGHER-generation re-derive must be able to write an honest NULL "+
			"(de-poisoning): usd_volume = %q, want NULL — a blanket COALESCE would "+
			"wrongly preserve it", uv.String)
	}
}
