package reflector

import (
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Regression for the 2026-08-29 r1 v0.48.0 page of
// stellarindex_ingestion_oracle_unknown_symbols: every reflector-fx
// event carries a VES and an XAU slot, and since PR #247 the decoder
// recorded them as raw:VES / raw:XAU (7 rows each in 2h) and bumped
// the unknown-symbols counter on every event. The cause was the
// allow-lists, not the decoder: VES is ISO-4217 fiat (ADR-0010) and
// XAU is the spot-gold commodity in the rwa: namespace (ADR-0028).
// Pins that the two slots now decode to fiat:VES / rwa:XAU at their
// original vector positions (DAT-03: op_index unchanged, so the
// generation-guarded upsert rewrites the raw: row in place on replay)
// and that the alert's counter no longer increments for them.
func TestRealDecoder_fxVESAndXAUMappedNotRaw(t *testing.T) {
	ves := xdr.ScSymbol("VES")
	vesSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &ves}
	xau := xdr.ScSymbol("XAU")
	xauSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &xau}
	bodyB64 := encodeUpdateBody(t,
		[]xdr.ScVal{vesSv, xauSv},
		// Synthetic magnitudes at the 14-decimal scale, NOT the live
		// feed's: the real 2026-04-23 capture decodes to VES ≈ 2.07e-3
		// and XAU ≈ 4720.90, so this comment used to state values the
		// repo's own fixtures contradict (wave-D SI-OC-05).
		//
		// The constants are left as they are on purpose. This test
		// asserts round-trip identity of whatever it encodes, and
		// sdkDecodeUpdateBody has no magnitude-dependent branch — the
		// only price predicate on the whole path is Price.Sign() <= 0 —
		// so a 7.3e-6 input exercises byte-identical code to a 2.07e-3
		// one. Restating them would be churn that buys no coverage.
		//
		// Real-magnitude coverage is real_fixture_test.go, which runs
		// the actual captures and pins zero raw rows on FX with rwa:XAU
		// as the only non-fiat slot.
		// Values below: 1 VES ≈ 7.3e-6 USD; 1 XAU ≈ 4,100 USD.
		[]*big.Int{big.NewInt(730_000_000), big.NewInt(410_000_000_000_000_000)},
	)
	e := &events.Event{
		Topic:      []string{TopicSymbolReflector, TopicSymbolUpdate, encodeTimestampTopic(t, 1)},
		Value:      bodyB64,
		ContractID: "CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC",
		Ledger:     1,
		TxHash:     reflectorTxHash,
	}

	counter := obs.SourceUnknownSymbolsTotal.WithLabelValues("reflector")
	before := testutil.ToFloat64(counter)
	updates, err := decodeUpdate(e, VariantFX, DefaultDecimals, "", time.Now())
	if err != nil {
		t.Fatalf("decodeUpdate: %v", err)
	}
	if got := testutil.ToFloat64(counter) - before; got != 0 {
		t.Errorf("stellarindex_source_unknown_symbols_total{source=reflector} rose by %v for VES/XAU; want 0 (both are allow-listed)", got)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (fiat:VES + rwa:XAU), got %d", len(updates))
	}

	wantVES, err := canonical.NewFiatAsset("VES")
	if err != nil {
		t.Fatalf("VES must be on the ADR-0010 fiat allow-list: %v", err)
	}
	wantXAU, err := canonical.NewRWAAsset("XAU")
	if err != nil {
		t.Fatalf("XAU must be on the ADR-0028 rwa allow-list: %v", err)
	}
	want := []struct {
		asset canonical.Asset
		wire  string
		price int64
	}{
		{wantVES, "fiat:VES", 730_000_000},
		{wantXAU, "rwa:XAU", 410_000_000_000_000_000},
	}
	for i, w := range want {
		u := updates[i]
		if !u.Asset.IsMapped() {
			t.Errorf("updates[%d].Asset = %s: still a raw: row, want %s", i, u.Asset, w.wire)
		}
		if !u.Asset.Equal(w.asset) || u.Asset.String() != w.wire {
			t.Errorf("updates[%d].Asset = %s want %s", i, u.Asset, w.wire)
		}
		if u.OpIndex != uint32(i) {
			t.Errorf("updates[%d].OpIndex = %d want %d (DAT-03: slot position unchanged)", i, u.OpIndex, i)
		}
		if !u.Quote.Equal(usdFiat) {
			t.Errorf("updates[%d].Quote = %s want %s", i, u.Quote, usdFiat)
		}
		if u.Price.BigInt().Cmp(big.NewInt(w.price)) != 0 {
			t.Errorf("updates[%d].Price = %s want %d", i, u.Price, w.price)
		}
		if err := u.Validate(); err != nil {
			t.Errorf("updates[%d].Validate: %v", i, err)
		}
	}
}
