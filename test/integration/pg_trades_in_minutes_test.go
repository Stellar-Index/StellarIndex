//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
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

type minutesExplainNode struct {
	NodeType                string               `json:"Node Type"`
	RelationName            string               `json:"Relation Name"`
	ActualRows              float64              `json:"Actual Rows"`
	ActualLoops             float64              `json:"Actual Loops"`
	RowsRemovedByFilter     float64              `json:"Rows Removed by Filter"`
	RowsRemovedByJoinFilter float64              `json:"Rows Removed by Join Filter"`
	Plans                   []minutesExplainNode `json:"Plans"`
}

// Over a populated day of four markets, TradesInMinutes reads only the listed minutes' rows of the market: each minute is one
// index range probe. Asserted on the plan's row counts, not wall time: rows read from trades chunks (output plus filtered out)
// never exceed the market's rows in the listed minutes, and no join filter discards rows.
func TestTradesInMinutes_PlanProbesEachMinute(t *testing.T) {
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
	usdc, err := c.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	d0 := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	// Two rows a second for a day, round-robin over the market's two stored directions and fourteen other markets: 15 rows a
	// minute in the market, 172,800 in all, so a probe that filters on the pair instead of seeking it reads 8x the rows.
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount)
		SELECT 'sdex', 80000000 + g, lpad(to_hex(g), 64, '0'), 0, $1::timestamptz + (g / 2) * INTERVAL '1 second',
		       CASE g % 16 WHEN 0 THEN $2::text WHEN 1 THEN $3::text ELSE 'N' || (g % 16) || '-' || $4::text END,
		       CASE g % 16 WHEN 1 THEN $2::text ELSE $3::text END,
		       1000 + g, 100 + g
		  FROM generate_series(0, 172799) g`,
		d0, pair.Base.String(), pair.Quote.String(), issuer); err != nil {
		t.Fatalf("day insert: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `ANALYZE trades`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ n, step int }{{5, 240}, {400, 3}} {
		minutes := make([]time.Time, tc.n)
		for i := range minutes {
			minutes[i] = d0.Add(time.Duration(i*tc.step) * time.Minute)
		}
		const limit = 10000
		var raw string
		if err := store.DB().QueryRowContext(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+timescale.TradesInMinutesQuery,
			pair.Base.String(), pair.Quote.String(), minutes, minutes[0], minutes[tc.n-1].Add(time.Minute), limit, "upshift",
			"0.01").Scan(&raw); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		var doc []struct {
			Plan          minutesExplainNode `json:"Plan"`
			ExecutionTime float64            `json:"Execution Time"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
			t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
		}
		var read, joinRemoved float64
		var walk func(n minutesExplainNode)
		walk = func(n minutesExplainNode) {
			if strings.HasPrefix(n.RelationName, "_hyper_") { // the chunk scans; a ChunkAppend parent repeats their rows
				read += (n.ActualRows + n.RowsRemovedByFilter) * n.ActualLoops // both are per-loop averages
			}
			joinRemoved += n.RowsRemovedByJoinFilter
			for _, ch := range n.Plans {
				walk(ch)
			}
		}
		walk(doc[0].Plan)
		var inMinutes float64
		if err := store.DB().QueryRowContext(ctx, `
			SELECT count(*) FROM trades
			 WHERE ((base_asset = $1 AND quote_asset = $2) OR (base_asset = $2 AND quote_asset = $1))
			   AND date_trunc('minute', ts) = ANY($3::timestamptz[])`,
			pair.Base.String(), pair.Quote.String(), minutes).Scan(&inMinutes); err != nil {
			t.Fatal(err)
		}
		t.Logf("%d minutes: %.0f trades rows read, %.0f removed by join filter, %.0f in the listed minutes, %.1f ms",
			tc.n, read, joinRemoved, inMinutes, doc[0].ExecutionTime)
		// 1.25x absorbs EXPLAIN's per-loop rounding of Actual Rows; a scan of the whole span reads several times the rows.
		if read > 1.25*inMinutes || joinRemoved > 0 {
			t.Errorf("%d minutes: plan reads %.0f trades rows (join filter drops %.0f); want at most the %0.f in the listed minutes:\n%s",
				tc.n, read, joinRemoved, inMinutes, raw)
		}
	}
}
