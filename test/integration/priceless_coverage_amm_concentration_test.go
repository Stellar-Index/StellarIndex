//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricelesscoverage"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPricelessCoverage_AMMConcentration executes the priceless-popular
// tripwire's candidate SQL (timescale.Store.PopularPricelessCandidates)
// and its classifier against a real TimescaleDB, on the four counterparty
// populations the trades hypertable actually holds.
//
// T016: the concentration NUMERATOR used to require `maker IS NOT NULL
// AND taker IS NOT NULL` while the vol7d DENOMINATOR took every row, so
// the two were measured over DIFFERENT populations. Only the SDEX decoder
// records both sides; every Soroban AMM (aquarius, soroswap, phoenix,
// comet, sushiswap_v3) leaves the resting side to the pool and records a
// taker only — measured on r1 2026-09-19, 100% of the 27.8k 24h rows of
// all five AMM sources have maker NULL. The share of an AMM-only asset
// was therefore 0 BY CONSTRUCTION, the wash exclusion could never fire
// for it, and a farm painting volume on an AMM self-selected straight
// into the coverage alert the tripwire exists to keep honest. Two such
// assets were live on r1 the same day, above the $10k popularity floor
// with 0.95 / 0.9999 of their volume swapped by ONE account, both
// reporting a 0 concentration share.
//
// The four fixtures are one population each, and each pins the CORRECTED
// share, not merely "non-zero":
//
//	AMM wash   aquarius, maker NULL: 19/20 of $20k one taker  → 0.95, excluded
//	AMM broad  aquarius, maker NULL: five takers, 4/20 each   → 0.20, fires
//	SDEX pair  sdex, both sides: two pairs, 10/20 each        → 0.50, fires
//	CEX feed   binance, neither side recorded                 → 0.00, fires
//
// The SDEX and CEX rows are the no-regression half: the order book's
// pair keying is unchanged by the fix, and volume from a venue that
// records no account at all must still PAGE (it cannot be measured, and
// the tripwire fails loud, never quiet) instead of being suppressed.
func TestPricelessCoverage_AMMConcentration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	mustAsset := func(code string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustPair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Four base assets, one per counterparty population, quoted against a
	// single non-proxy asset so nothing here is priceable (prices_1m is
	// never refreshed) and only the four bases become candidates.
	ammWash := mustAsset("AMMWSH")
	ammBroad := mustAsset("AMMBRD")
	sdexPair := mustAsset("SDXPAR")
	cexFeed := mustAsset("CEXFED")
	quote := mustAsset("ZQOT")

	// Counterparty accounts. Only string identity matters to the roll —
	// the stored columns are plain text.
	const (
		washer    = "GAWASHERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		passerby  = "GAPASSERBYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		restingA  = "GARESTINGAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		crossingA = "GACROSSINGAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		crossingB = "GACROSSINGBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	broadTakers := []string{
		"GABROADONEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GABROADTWOAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GABROADSIXAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GABROADFORAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GABROADFIVAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}

	now := time.Now().UTC().Truncate(time.Minute)
	nonce := 0
	add := func(source string, base c.Asset, maker, taker string) {
		nonce++
		tr := mkIntegrationTrade(source, nonce, now.Add(-time.Duration(nonce)*time.Minute),
			mustPair(base, quote), 1_000_000_000, 1_000_000_000)
		tr.Maker, tr.Taker = maker, taker
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s/%d: %v", source, nonce, err)
		}
	}

	for i := 0; i < 20; i++ {
		// AMM wash: one account round-tripping through the pool for 19 of
		// the 20 fills; the pool itself is never an account (maker NULL).
		taker := washer
		if i == 19 {
			taker = passerby
		}
		add("aquarius", ammWash, "", taker)

		// AMM broad: the same venue shape, five distinct swappers.
		add("aquarius", ammBroad, "", broadTakers[i%len(broadTakers)])

		// Order book: both sides recorded, two pairs sharing one maker.
		counter := crossingA
		if i%2 == 1 {
			counter = crossingB
		}
		add("sdex", sdexPair, restingA, counter)

		// External CEX feed: neither side is recorded at all.
		add("binance", cexFeed, "", "")
	}

	// The quote leg is not a USD peg, so insert-time usd_volume is NULL.
	// Stamp a flat $1,000 per fill: every asset then holds $20,000 of 7d
	// and 24h volume — over the popularity floor ($10k) and over the
	// substance serve floor ($1k), so the wash exclusion is the ONLY
	// guard that can decide any of them.
	if _, err := store.DB().ExecContext(ctx, `UPDATE trades SET usd_volume = 1000`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}

	sigs, err := store.PopularPricelessCandidates(ctx)
	if err != nil {
		t.Fatalf("PopularPricelessCandidates: %v", err)
	}
	byID := make(map[string]timescale.AssetCoverageSignals, len(sigs))
	for _, s := range sigs {
		byID[s.AssetID] = s
	}

	for _, tc := range []struct {
		label                   string
		asset                   c.Asset
		wantTopPair, wantAttrib float64
	}{
		{"amm_wash", ammWash, 0.95, 1.0},
		{"amm_broad", ammBroad, 0.20, 1.0},
		{"sdex_pair", sdexPair, 0.50, 1.0},
		{"cex_feed", cexFeed, 0.00, 0.0},
	} {
		sig, ok := byID[tc.asset.String()]
		if !ok {
			t.Errorf("%s: %s missing from the candidate set", tc.label, tc.asset)
			continue
		}
		if sig.Volume7dUSD != 20_000 {
			t.Errorf("%s: vol_7d = %v, want 20000", tc.label, sig.Volume7dUSD)
		}
		if math.Abs(sig.TopAccountPairVolShare-tc.wantTopPair) > 1e-9 {
			t.Errorf("%s: top_account_pair_share = %v, want %v",
				tc.label, sig.TopAccountPairVolShare, tc.wantTopPair)
		}
		if math.Abs(sig.AttributedVolShare-tc.wantAttrib) > 1e-9 {
			t.Errorf("%s: attributed_vol_share = %v, want %v",
				tc.label, sig.AttributedVolShare, tc.wantAttrib)
		}
	}

	// End to end: the real SQL through the real classifier. The wash farm
	// on the AMM must NOT be paged for; the other three must be.
	var logbuf bytes.Buffer
	w := pricelesscoverage.New(store, pricelesscoverage.Options{
		Logger: slog.New(slog.NewJSONHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	w.Sweep(ctx)

	paged := make(map[string]bool)
	sc := bufio.NewScanner(bytes.NewReader(logbuf.Bytes()))
	for sc.Scan() {
		var rec struct {
			Msg     string `json:"msg"`
			AssetID string `json:"asset_id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("sweep log line %q: %v", sc.Text(), err)
		}
		if strings.HasPrefix(rec.Msg, "priceless-popular coverage gap") && rec.AssetID != "" {
			paged[rec.AssetID] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan sweep log: %v", err)
	}
	for _, tc := range []struct {
		label string
		asset c.Asset
		want  bool
	}{
		{"amm_wash", ammWash, false},
		{"amm_broad", ammBroad, true},
		{"sdex_pair", sdexPair, true},
		{"cex_feed", cexFeed, true},
	} {
		if got := paged[tc.asset.String()]; got != tc.want {
			t.Errorf("sweep paged %s (%s) = %v, want %v", tc.label, tc.asset, got, tc.want)
		}
	}
}
