//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TradeSums1mLive computes from raw trades exactly the rows the materialised trade_sums_1m serves for the same minutes: same
// per-source sums, counts and NUMERIC price bounds, both directions, non-VWAP sources out. Its plan reads only the window's
// chunk through the pair index.
func TestTradeSums1mLive_MatchesMaterialisedView(t *testing.T) {
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
	eurc, err := c.NewClassicAsset("EURC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	m0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	nonce := 0
	for minute := 0; minute < 4; minute++ {
		for i, src := range []string{"sdex", "soroswap", "upshift", "sdex", "soroswap"} {
			nonce++
			ts := m0.Add(time.Duration(minute)*time.Minute + time.Duration(7*i+minute)*time.Second)
			base, quote := int64(3_000_000_007+nonce*13), int64(700_000_001+nonce*11) // repeating-decimal prices
			tr := mkIntegrationTrade(src, nonce, ts, pair, base, quote)
			if nonce%3 == 0 {
				tr = mkIntegrationTrade(src, nonce, ts, pair.Flip(), quote, base)
			}
			if err := store.InsertTrade(ctx, tr); err != nil {
				t.Fatalf("InsertTrade %d: %v", nonce, err)
			}
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('trade_sums_1m', $1::timestamptz, $2::timestamptz)`,
		m0, m0.Add(5*time.Minute)); err != nil {
		t.Fatalf("refresh trade_sums_1m: %v", err)
	}
	to := m0.Add(4 * time.Minute)
	view, watermark, err := store.TradeSums1mInRange(ctx, pair, m0, to)
	if err != nil {
		t.Fatal(err)
	}
	if watermark.Before(to) {
		t.Fatalf("watermark %v, want >= %v after the refresh", watermark, to)
	}
	live, err := store.TradeSums1mLive(ctx, pair, m0, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) == 0 {
		t.Fatal("the view served no rows")
	}
	if a, b := minuteSumKeys(view), minuteSumKeys(live); strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("live rows differ from the materialised view:\nview:\n%s\nlive:\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	for _, r := range live {
		if r.Source == "upshift" {
			t.Errorf("non-VWAP source served live: %+v", r)
		}
	}
	if _, err := store.TradeSums1mLive(ctx, pair, m0, m0.Add(timescale.TradeSums1mLiveMaxSpan+time.Minute)); err == nil {
		t.Error("a live span past TradeSums1mLiveMaxSpan was accepted")
	}

	// Noise: another market in the same minutes, and this market two days earlier in another chunk.
	for _, q := range []struct {
		at         time.Time
		base, quot string
	}{{m0, c.NativeAsset().String(), eurc.String()}, {m0.Add(-48 * time.Hour), pair.Base.String(), pair.Quote.String()}} {
		if _, err := store.DB().ExecContext(ctx, `
			INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount)
			SELECT 'sdex', 60000000 + g, lpad(to_hex(g), 64, '0'), 1, $1::timestamptz + (g % 240) * INTERVAL '1 second',
			       $2, $3, 1000 + g, 100 + g
			  FROM generate_series(1, 20000) g`, q.at, q.base, q.quot); err != nil {
			t.Fatalf("noise insert: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `ANALYZE trades`); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := store.DB().QueryRowContext(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+timescale.TradeSums1mLiveQuery(m0, to),
		pair.Base.String(), pair.Quote.String(), "upshift").Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var doc []struct {
		Plan liveExplainNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
	}
	chunks := map[string]bool{}
	var read float64
	var walk func(n liveExplainNode)
	walk = func(n liveExplainNode) {
		if strings.HasPrefix(n.RelationName, "_hyper_") {
			chunks[n.RelationName] = true
			read += n.ActualRows*n.ActualLoops + n.RowsRemovedByFilter
			if n.NodeType == "Seq Scan" {
				t.Errorf("seq scan of trades chunk %s", n.RelationName)
			}
		}
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(doc[0].Plan)
	if len(chunks) != 1 {
		t.Errorf("plan reads %d trades chunks %v, want only the window's", len(chunks), chunks)
	}
	if read > float64(nonce) {
		t.Errorf("plan reads %.0f trades rows, want at most the market's %d in the window:\n%s", read, nonce, raw)
	}
}

type liveExplainNode struct {
	NodeType            string            `json:"Node Type"`
	RelationName        string            `json:"Relation Name"`
	ActualRows          float64           `json:"Actual Rows"`
	ActualLoops         float64           `json:"Actual Loops"`
	RowsRemovedByFilter float64           `json:"Rows Removed by Filter"`
	Plans               []liveExplainNode `json:"Plans"`
}

func minuteSumKeys(rows []timescale.TradeMinuteSum) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s %s n=%d base=%s quote=%s lo=%s hi=%s", r.Source, r.Bucket.UTC().Format(time.RFC3339),
			r.Count, r.BaseSum, r.QuoteSum, r.MinPrice.RatString(), r.MaxPrice.RatString()))
	}
	sort.Strings(out)
	return out
}
