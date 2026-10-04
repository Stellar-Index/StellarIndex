// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The resolver's substance gate: a rate for a classic or Soroban asset
// values a trade only when the market it came from clears the
// published-price floor at the trade's time. The executing proof against
// real Postgres is test/integration/usd_fx_resolver_substance_test.go.

const substanceTestIssuer = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"

func directHit(bucket time.Time, vwap string) scriptedResult {
	return scriptedResult{cols: []string{"bucket", "vwap"}, rows: [][]driver.Value{{bucket, vwap}}}
}

func xlmLegHit(bucket time.Time, vwap string) scriptedResult {
	return scriptedResult{cols: []string{"bucket", "vwap", "inverted"}, rows: [][]driver.Value{{bucket, vwap, false}}}
}

func xlmLegMiss() scriptedResult {
	return scriptedResult{cols: []string{"bucket", "vwap", "inverted"}}
}

func measured(volumeUSD string, buckets, spanSeconds int64) scriptedResult {
	return scriptedResult{
		cols: []string{"volume_usd", "buckets", "span_seconds", "valued_buckets"},
		rows: [][]driver.Value{{volumeUSD, buckets, spanSeconds, buckets}},
	}
}

func substanceTestResolver(t *testing.T, now time.Time, script ...scriptedResult) (*VWAPUSDFXResolver, *scriptedConn) {
	t.Helper()
	store, conn := newScriptedStore(t, script...)
	r, err := NewVWAPUSDFXResolver(store, VWAPUSDFXResolverOptions{
		USDPegs:        []string{usdcClassicPeg},
		PegSACWrappers: pegSACWrappersFixture,
		Clock:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	return r, conn
}

func substanceTestAsset(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewClassicAsset(code, substanceTestIssuer)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func boundStrings(t *testing.T, stmt recordedStmt, n int) []string {
	t.Helper()
	got, ok := stmt.arg(t, n).([]string)
	if !ok {
		t.Fatalf("arg $%d is %T, want []string", n, stmt.arg(t, n))
	}
	return got
}

// The wash-ring shape: a $1 rate from cent-sized round trips against the
// peg, and a two-trade book against XLM. Neither market clears the floor,
// so the asset has no USD price — the trade inserts with usd_volume NULL.
func TestUSDPriceAt_RingMarketDoesNotValue(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 34, 0, 0, time.UTC)
	usdx := substanceTestAsset(t, "USDX")
	r, conn := substanceTestResolver(t, at.Add(time.Minute),
		directHit(at.Add(-time.Minute), "1.00000000"),
		measured("4.8", 48, 56400),
		xlmLegHit(at.Add(-time.Hour), "4"),
		directHit(at.Add(-time.Minute), "0.25"), // XLM/USD anchor, ungated
		measured("20", 2, 3600),
	)

	got, ok, err := r.USDPriceAt(context.Background(), usdx, at)
	if err != nil || ok || got != "" {
		t.Fatalf("USDPriceAt(ring asset) = (%q, %t, %v), want (\"\", false, nil)", got, ok, err)
	}
	if len(conn.stmts) != 5 {
		t.Fatalf("issued %d statements, want 5 (direct, its substance, XLM leg, XLM anchor, bridge substance)", len(conn.stmts))
	}
	direct, bridge := conn.stmts[1], conn.stmts[4]
	if want := []string{usdx.String()}; !reflect.DeepEqual(boundStrings(t, direct, 1), want) {
		t.Errorf("direct substance bases = %q, want %q", boundStrings(t, direct, 1), want)
	}
	if want := []string{usdcClassicPeg, usdcSAC}; !reflect.DeepEqual(boundStrings(t, direct, 2), want) {
		t.Errorf("direct substance quotes = %q, want the alias-complete peg set %q", boundStrings(t, direct, 2), want)
	}
	if want := []string{"native", canonical.XLMSacContractID}; !reflect.DeepEqual(boundStrings(t, bridge, 2), want) {
		t.Errorf("bridge substance quotes = %q, want both on-chain XLM forms %q", boundStrings(t, bridge, 2), want)
	}
	if !strings.Contains(direct.sql, "FROM prices_1m") {
		t.Errorf("a recent trade must be measured at minute grain:\n%s", indent(direct.sql))
	}
	if got, want := literalBound(t, direct.sql, ">="), at.Truncate(time.Hour).Add(-24*time.Hour); !got.Equal(want) {
		t.Errorf("window opens at %s, want %s (24h ending at the trade's hour)", got, want)
	}
}

func TestUSDPriceAt_DeepPegMarketValues(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 34, 0, 0, time.UTC)
	r, conn := substanceTestResolver(t, at.Add(time.Minute),
		directHit(at.Add(-time.Minute), "1.08500000"),
		measured("3255", 30, 52200),
	)
	got, ok, err := r.USDPriceAt(context.Background(), substanceTestAsset(t, "DEEP"), at)
	if err != nil || !ok || got != "1.085" {
		t.Fatalf("USDPriceAt(deep) = (%q, %t, %v), want (\"1.085\", true, nil)", got, ok, err)
	}
	if len(conn.stmts) != 2 {
		t.Errorf("issued %d statements, want 2 — a passing direct market never reaches the bridge", len(conn.stmts))
	}
}

// A thin direct market does not block a deep XLM route.
func TestUSDPriceAt_ThinDirectFallsThroughToDeepBridge(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 34, 0, 0, time.UTC)
	r, _ := substanceTestResolver(t, at.Add(time.Minute),
		directHit(at.Add(-time.Minute), "1.00000000"),
		measured("4.8", 48, 56400),
		xlmLegHit(at.Add(-time.Hour), "4"),
		directHit(at.Add(-time.Minute), "0.25"),
		measured("52000", 900, 86000),
	)
	got, ok, err := r.USDPriceAt(context.Background(), substanceTestAsset(t, "TOKN"), at)
	if err != nil || !ok || got != "1" {
		t.Fatalf("USDPriceAt = (%q, %t, %v), want the bridged (\"1\", true, nil)", got, ok, err)
	}
}

// A historical trade is held to the hour-grain floor over the window that
// ended at ITS hour: a market deep then and dead now still values it, and
// the same 8 active hours measured at minute grain would not pass.
func TestUSDPriceAt_HistoricalTradeMeasuredAtItsOwnInstant(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	at := now.Add(-30 * 24 * time.Hour).Add(17 * time.Minute)
	r, conn := substanceTestResolver(t, now,
		directHit(at.Add(-time.Minute), "1.08500000"),
		measured("2400", 8, 25200),
	)
	got, ok, err := r.USDPriceAt(context.Background(), substanceTestAsset(t, "OLD"), at)
	if err != nil || !ok || got != "1.085" {
		t.Fatalf("USDPriceAt(historical) = (%q, %t, %v), want (\"1.085\", true, nil)", got, ok, err)
	}
	stmt := conn.stmts[1]
	if !strings.Contains(stmt.sql, "FROM prices_1h") {
		t.Errorf("a trade past the minute rung must be measured at hour grain:\n%s", indent(stmt.sql))
	}
	if got, want := literalBound(t, stmt.sql, "<="), at.Truncate(time.Hour).Add(-time.Hour); !got.Equal(want) {
		t.Errorf("window closes at %s, want %s (buckets closed by the trade's hour)", got, want)
	}

	recent := now.Add(-2 * time.Hour)
	r, _ = substanceTestResolver(t, now,
		directHit(recent.Add(-time.Minute), "1.08500000"),
		measured("2400", 8, 25200),
		xlmLegMiss(),
	)
	if got, ok, err := r.USDPriceAt(context.Background(), substanceTestAsset(t, "OLD"), recent); err != nil || ok {
		t.Errorf("8 active minutes at minute grain = (%q, %t, %v), want below the 20-bucket floor", got, ok, err)
	}
}

// One verdict per (market, asset, hour) serves every trade in the hour;
// a failed measurement is returned, not cached.
func TestUSDPriceAt_SubstanceVerdictCachedPerHourErrorsNot(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 10, 0, 0, time.UTC)
	tokn := substanceTestAsset(t, "TOKN")
	r, conn := substanceTestResolver(t, at.Add(time.Hour),
		directHit(at.Add(-time.Minute), "2"),
		scriptedResult{err: errors.New("connection reset")},
		directHit(at, "2"),
		measured("3255", 30, 52200),
		directHit(at.Add(time.Minute), "2"),
	)
	if _, _, err := r.USDPriceAt(context.Background(), tokn, at); err == nil {
		t.Fatal("a substance measurement error must propagate (the trade then inserts NULL)")
	}
	for i, ts := range []time.Time{at.Add(time.Minute), at.Add(2 * time.Minute)} {
		if got, ok, err := r.USDPriceAt(context.Background(), tokn, ts); err != nil || !ok || got != "2" {
			t.Fatalf("call %d: USDPriceAt = (%q, %t, %v), want (\"2\", true, nil)", i, got, ok, err)
		}
	}
	if len(conn.stmts) != 5 {
		t.Errorf("issued %d statements, want 5: the error re-measures, the third call reuses the verdict", len(conn.stmts))
	}
}

// XLM is the bridge's anchor and off-chain assets cannot be self-dealt on
// a Stellar market, so neither is measured.
func TestUSDPriceAt_AnchorsAndOffChainAssetsAreNotGated(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 34, 0, 0, time.UTC)
	btc, err := canonical.NewCryptoAsset("BTC")
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range []canonical.Asset{btc, canonical.NativeAsset()} {
		r, conn := substanceTestResolver(t, at.Add(time.Minute), directHit(at.Add(-time.Minute), "0.25"))
		if got, ok, err := r.USDPriceAt(context.Background(), asset, at); err != nil || !ok || got != "0.25" {
			t.Errorf("USDPriceAt(%s) = (%q, %t, %v), want (\"0.25\", true, nil)", asset, got, ok, err)
		}
		if len(conn.stmts) != 1 {
			t.Errorf("%s: issued %d statements, want 1 — no substance read", asset, len(conn.stmts))
		}
	}
}

// A substance-refused trade is labelled "thin" while usd_volume stays NULL;
// a leg with no market at all stays "no".
func TestUsdLabel_ThinVersusNoMarket(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 34, 0, 0, time.UTC)
	base, err := canonical.NewClassicAsset("AAA", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	run := func(script ...scriptedResult) (*string, string) {
		r, _ := substanceTestResolver(t, at.Add(time.Minute), script...)
		tr := canonical.Trade{
			Source: "sdex", Timestamp: at,
			Pair:        canonical.Pair{Base: base, Quote: substanceTestAsset(t, "USDX")},
			BaseAmount:  canonical.NewAmount(big.NewInt(10_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(10_000_000)),
		}
		v, err := tradeUSDVolumeChecked(context.Background(), tr, nil, r)
		if err != nil {
			t.Fatal(err)
		}
		return v, (&Store{usdVolumeFXResolver: r}).usdLabel(tr, v)
	}

	v, label := run(
		directHit(at.Add(-time.Minute), "1.00000000"), measured("4.8", 48, 56400),
		xlmLegHit(at.Add(-time.Hour), "4"), directHit(at.Add(-time.Minute), "0.25"), measured("20", 2, 3600),
		scriptedResult{cols: []string{"bucket", "vwap"}}, xlmLegMiss(), // tier 4 prices the base leg: no market
	)
	if v != nil || label != "thin" {
		t.Errorf("thin market: usd_volume=%v label=%q, want nil/thin", v, label)
	}

	v, label = run(
		scriptedResult{cols: []string{"bucket", "vwap"}},
		xlmLegMiss(),
		scriptedResult{cols: []string{"bucket", "vwap"}},
		xlmLegMiss(),
	)
	if v != nil || label != "no" {
		t.Errorf("no market: usd_volume=%v label=%q, want nil/no", v, label)
	}
}
