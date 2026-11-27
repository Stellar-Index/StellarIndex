//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TradesInMinutes returns, in one read, exactly the rows TradesInRange returns for each listed minute (both stored directions,
// oriented, non-VWAP sources out), keeps the newest `limit` of them in ascending order, and reads a repeated minute once.
func TestTradesInMinutes_MatchesPerMinuteRangeReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	m0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	nonce := 0
	for minute := 0; minute < 6; minute++ {
		for i, src := range []string{"sdex", "soroswap", "upshift", "sdex"} {
			nonce++
			ts := m0.Add(time.Duration(minute)*time.Minute + time.Duration(9*i)*time.Second)
			tr := mkIntegrationTrade(src, nonce, ts, pair, int64(3_000_000_007+nonce*13), int64(700_000_001+nonce*11))
			if nonce%3 == 0 {
				tr = mkIntegrationTrade(src, nonce, ts, pair.Flip(), int64(700_000_001+nonce*11), int64(3_000_000_007+nonce*13))
			}
			if err := store.InsertTrade(ctx, tr); err != nil {
				t.Fatalf("InsertTrade %d: %v", nonce, err)
			}
		}
	}
	minutes := []time.Time{m0.Add(4 * time.Minute), m0.Add(time.Minute), m0.Add(3 * time.Minute), m0.Add(time.Minute)}
	var want []string
	for _, m := range []time.Time{m0.Add(time.Minute), m0.Add(3 * time.Minute), m0.Add(4 * time.Minute)} {
		rows, err := store.TradesInRange(ctx, pair, m, m.Add(time.Minute), 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			want = append(want, r.ID()+"|"+r.BaseAmount.String()+"|"+r.QuoteAmount.String())
		}
	}
	if len(want) == 0 {
		t.Fatal("fixture: no rows in the listed minutes")
	}
	for _, limit := range []int{1000, 5} {
		got, err := store.TradesInMinutes(ctx, pair, minutes, limit)
		if err != nil {
			t.Fatal(err)
		}
		w := want
		if len(w) > limit {
			w = w[len(w)-limit:]
		}
		if len(got) != len(w) {
			t.Fatalf("limit %d: %d rows, want %d", limit, len(got), len(w))
		}
		for i, g := range got {
			if !g.Pair.Equal(pair) {
				t.Errorf("limit %d row %d: pair %s, want oriented to %s", limit, i, g.Pair, pair)
			}
			if k := g.ID() + "|" + g.BaseAmount.String() + "|" + g.QuoteAmount.String(); k != w[i] {
				t.Errorf("limit %d row %d = %s, want %s", limit, i, k, w[i])
			}
		}
	}
	if _, err := store.TradesInMinutes(ctx, pair, []time.Time{m0.Add(time.Second)}, 10); err == nil {
		t.Error("a minute not on the minute: want an error")
	}
}
