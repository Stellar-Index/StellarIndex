//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestTransitiveUSDPriceCandidates_RankedHops pins that the resolver
// returns EVERY priced hop, ranked by near-leg (asset<->hop) USD volume,
// rather than only the deepest one. The API gates each candidate and
// falls through to the next, which is only possible if the second-best
// hop reaches it.
//
// Fixture (closed buckets inside the trailing 24h):
//
//	XLM/USDC   vwap 0.40, before every hop leg → xlm_usd = 0.40 at each leg's minute
//	HOPA/XLM   vwap 2                 → HOPA = 0.80 USD
//	HOPB/XLM   vwap 0.5               → HOPB = 0.20 USD
//	TGT/HOPA   vwap 1,  $100 volume   → TGT  = 0.80 USD via HOPA
//	HOPB/TGT   vwap 0.25, $1000 volume (inverted) → TGT = 0.80 USD via HOPB
//	TGT/NOPX   NOPX has no USD route  → not a candidate
//
// HOPB outranks HOPA on volume though it sorts after it by id, so the
// order proves the volume ranking rather than the tie-break.
func TestTransitiveUSDPriceCandidates_RankedHops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		hopIssuer  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		tgtIssuer  = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)
	classic := func(code, issuer string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	xlm := c.NativeAsset()
	usdc := classic("USDC", usdcIssuer)
	hopA := classic("HOPA", hopIssuer)
	hopB := classic("HOPB", hopIssuer)
	nopx := classic("NOPX", hopIssuer)
	tgt := classic("TGT", tgtIssuer)

	now := time.Now().UTC().Truncate(time.Minute)
	nonce := 0
	insert := func(ts time.Time, p c.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", nonce, ts, p, base, quote)); err != nil {
			t.Fatalf("InsertTrade %s: %v", p, err)
		}
	}
	insert(now.Add(-30*time.Minute), pair(xlm, usdc), 1_000_000_000, 400_000_000)
	insert(now.Add(-20*time.Minute), pair(hopA, xlm), 100_000_000, 200_000_000)
	insert(now.Add(-20*time.Minute), pair(hopB, xlm), 100_000_000, 50_000_000)
	insert(now.Add(-15*time.Minute), pair(tgt, hopA), 100_000_000, 100_000_000)
	insert(now.Add(-15*time.Minute), pair(hopB, tgt), 400_000_000, 100_000_000)
	insert(now.Add(-15*time.Minute), pair(tgt, nopx), 100_000_000, 100_000_000)

	stamp := func(base, quote c.Asset, usd int) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx,
			`UPDATE trades SET usd_volume = $3 WHERE base_asset = $1 AND quote_asset = $2`,
			base.String(), quote.String(), usd); err != nil {
			t.Fatalf("stamp usd_volume: %v", err)
		}
	}
	stamp(xlm, usdc, 40)
	stamp(tgt, hopA, 100)
	stamp(hopB, tgt, 1000)
	stamp(tgt, nopx, 5000)
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	got, err := store.TransitiveUSDPriceCandidates(ctx, tgt.String())
	if err != nil {
		t.Fatalf("TransitiveUSDPriceCandidates: %v", err)
	}
	want := []struct{ hop, price string }{
		{hopB.String(), "0.80"},
		{hopA.String(), "0.80"},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %+v, want hops %s then %s", got, hopB, hopA)
	}
	for i, w := range want {
		if got[i].Hop != w.hop {
			t.Errorf("candidate %d hop = %s, want %s", i, got[i].Hop, w.hop)
		}
		a, ok1 := new(big.Rat).SetString(got[i].PriceUSD)
		b, _ := new(big.Rat).SetString(w.price)
		if !ok1 || a.Cmp(b) != 0 {
			t.Errorf("candidate %d price = %s, want %s", i, got[i].PriceUSD, w.price)
		}
	}
}

// TestTransitiveUSDPriceCandidates_LatestBucketAcrossDirections pins that
// both the asset->hop leg and the hop's XLM price take the LATEST bucket
// across the two stored directions, the as-is direction winning a tie. A
// preferred direction that went quiet 20h ago must not outrank the other
// direction's fresh bucket.
//
// Fixture (xlm_usd = 0.40 from -30m, before every hop's XLM leg):
//
//	HOPA/XLM 2 (-20m)                       → HOPA = 0.80 USD
//	T1/HOPA 1 (-20h), HOPA/T1 0.5 (-2m)     → T1 = 2 HOPA   = 1.60
//	T3/HOPA 1, HOPA/T3 0.5 (both -15m)      → T3 = 1 HOPA   = 0.80
//	HOPC/XLM 2 (-20h), XLM/HOPC 0.25 (-5m)  → HOPC = 4 XLM  = 1.60; T2/HOPC 1 → 1.60
//	HOPD/XLM 2, XLM/HOPD 0.25 (both -15m)   → HOPD = 2 XLM  = 0.80; T4/HOPD 1 → 0.80
func TestTransitiveUSDPriceCandidates_LatestBucketAcrossDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		hopIssuer  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		tgtIssuer  = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)
	classic := func(code, issuer string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	xlm := c.NativeAsset()
	usdc := classic("USDC", usdcIssuer)
	hopA := classic("HOPA", hopIssuer)
	hopC := classic("HOPC", hopIssuer)
	hopD := classic("HOPD", hopIssuer)
	t1 := classic("T1", tgtIssuer)
	t2 := classic("T2", tgtIssuer)
	t3 := classic("T3", tgtIssuer)
	t4 := classic("T4", tgtIssuer)

	now := time.Now().UTC().Truncate(time.Minute)
	nonce := 0
	insert := func(ago time.Duration, p c.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", nonce, now.Add(-ago), p, base, quote)); err != nil {
			t.Fatalf("InsertTrade %s: %v", p, err)
		}
	}
	insert(30*time.Minute, pair(xlm, usdc), 1_000_000_000, 400_000_000)
	insert(20*time.Minute, pair(hopA, xlm), 100_000_000, 200_000_000)
	insert(20*time.Hour, pair(t1, hopA), 100_000_000, 100_000_000)
	insert(2*time.Minute, pair(hopA, t1), 200_000_000, 100_000_000)
	insert(15*time.Minute, pair(t3, hopA), 100_000_000, 100_000_000)
	insert(15*time.Minute, pair(hopA, t3), 200_000_000, 100_000_000)
	insert(20*time.Hour, pair(hopC, xlm), 100_000_000, 200_000_000)
	insert(5*time.Minute, pair(xlm, hopC), 400_000_000, 100_000_000)
	insert(15*time.Minute, pair(t2, hopC), 100_000_000, 100_000_000)
	insert(15*time.Minute, pair(hopD, xlm), 100_000_000, 200_000_000)
	insert(15*time.Minute, pair(xlm, hopD), 400_000_000, 100_000_000)
	insert(15*time.Minute, pair(t4, hopD), 100_000_000, 100_000_000)
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 40 WHERE base_asset = 'native' AND quote_asset = $1`, usdc.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	cases := []struct {
		name       string
		asset, hop c.Asset
		price      string
	}{
		{"fresh inverted leg beats stale direct leg", t1, hopA, "1.60"},
		{"same-bucket leg tie takes the direct row", t3, hopA, "0.80"},
		{"fresh XLM/hop beats stale hop/XLM", t2, hopC, "1.60"},
		{"same-bucket hop tie takes hop/XLM", t4, hopD, "0.80"},
	}
	for _, tc := range cases {
		got, err := store.TransitiveUSDPriceCandidates(ctx, tc.asset.String())
		if err != nil {
			t.Fatalf("%s: TransitiveUSDPriceCandidates: %v", tc.name, err)
		}
		if len(got) != 1 || got[0].Hop != tc.hop.String() {
			t.Errorf("%s: candidates = %+v, want one via %s", tc.name, got, tc.hop)
			continue
		}
		a, ok := new(big.Rat).SetString(got[0].PriceUSD)
		b, _ := new(big.Rat).SetString(tc.price)
		if !ok || a.Cmp(b) != 0 {
			t.Errorf("%s: price = %s, want %s", tc.name, got[0].PriceUSD, tc.price)
		}
	}
}
