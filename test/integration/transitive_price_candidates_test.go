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
//	XLM/USDC   vwap 0.40              → xlm_usd = 0.40
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
	insert(now.Add(-10*time.Minute), pair(xlm, usdc), 1_000_000_000, 400_000_000)
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
