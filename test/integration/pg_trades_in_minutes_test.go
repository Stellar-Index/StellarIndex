//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	IndexName               string               `json:"Index Name"`
	ActualRows              float64              `json:"Actual Rows"`
	ActualLoops             float64              `json:"Actual Loops"`
	RowsRemovedByFilter     float64              `json:"Rows Removed by Filter"`
	RowsRemovedByJoinFilter float64              `json:"Rows Removed by Join Filter"`
	Plans                   []minutesExplainNode `json:"Plans"`
}

// Over a populated day where the market's assets are the busiest base and the busiest quote (as XLM and USDC are), TradesInMinutes
// reads only the listed minutes' rows of the market whatever the table statistics: before any ANALYZE, after one, and with the
// chunk compressed. Asserted on the plan's row counts, not wall time: rows read from trades chunks (output plus filtered out) never
// exceed the market's rows in the listed minutes, and no join filter discards rows. Then the reader's rows equal a plain reference
// query's, in order, across a compressed and an uncompressed chunk.
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
	db := store.DB()

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	// Chunks inherit this, so the first pass plans with no statistics at all rather than with whatever autovacuum got to.
	if _, err := db.ExecContext(ctx, `ALTER TABLE trades SET (autovacuum_enabled = false)`); err != nil {
		t.Fatal(err)
	}
	// Two rows a second, round-robin over 64 markets: the market both ways, 30 more with its base, 32 more with its quote. The
	// market is 1/32 of a minute's rows while its base and its quote are each about half, so pricing the probe from the assets'
	// own frequencies makes a time-only scan look as cheap as the pair seek and read 30x the rows.
	insertDay := func(day time.Time, seconds, ledger0 int) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount)
			SELECT 'sdex', $5::bigint + g, lpad(to_hex($5::bigint + g), 64, '0'), 0, $1::timestamptz + (g / 2) * INTERVAL '1 second',
			       CASE WHEN g % 64 = 1 THEN $3::text WHEN g % 64 < 32 THEN $2::text ELSE 'N' || (g % 64) || '-' || $4::text END,
			       CASE WHEN g % 64 = 1 THEN $2::text WHEN g % 64 = 0 OR g % 64 >= 32 THEN $3::text
			            ELSE 'Q' || (g % 64) || '-' || $4::text END,
			       1000 + g, 100 + g
			  FROM generate_series(0, $6::bigint - 1) g`,
			day, pair.Base.String(), pair.Quote.String(), issuer, ledger0, 2*seconds); err != nil {
			t.Fatalf("day insert: %v", err)
		}
	}
	d0 := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	insertDay(d0, 86400, 80000000)
	chunkStats := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stats
			 WHERE schemaname = '_timescaledb_internal' AND tablename LIKE '%_hyper_%' AND attname = 'base_asset'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	probe := func(stage string, n, step int) {
		t.Helper()
		minutes := make([]time.Time, n)
		for i := range minutes {
			minutes[i] = d0.Add(time.Duration(i*step) * time.Minute)
		}
		var raw string
		if err := db.QueryRowContext(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+timescale.TradesInMinutesQuery,
			pair.Base.String(), pair.Quote.String(), minutes, minutes[0], minutes[n-1].Add(time.Minute), 10000, "upshift",
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
		var read, batches, joinRemoved float64
		scans := map[string]int{}
		var walk func(p minutesExplainNode)
		walk = func(p minutesExplainNode) {
			if strings.HasPrefix(p.IndexName, "compress_hyper_") && !strings.Contains(p.IndexName, "base_asset_quote_asset") {
				t.Errorf("%s, %d minutes: compressed chunk read through %q, want the pair-segment index", stage, n, p.IndexName)
			}
			switch {
			case strings.HasPrefix(p.RelationName, "compress_hyper_"):
				// A compressed chunk decompresses whole batches, so it is bounded in batches read from the pair's segment.
				batches += p.ActualRows * p.ActualLoops
				scans[p.NodeType+" "+p.IndexName]++
				if p.NodeType == "Seq Scan" {
					t.Errorf("%s, %d minutes: compressed chunk read by a Seq Scan, want the pair-segment index", stage, n)
				}
			case strings.HasPrefix(p.RelationName, "_hyper_") && p.NodeType != "Custom Scan":
				// The chunk scans; a ChunkAppend parent repeats their rows. Both counts are per-loop averages.
				read += (p.ActualRows + p.RowsRemovedByFilter) * p.ActualLoops
				scans[p.NodeType+" "+p.IndexName]++
			}
			joinRemoved += p.RowsRemovedByJoinFilter
			for _, ch := range p.Plans {
				walk(ch)
			}
		}
		walk(doc[0].Plan)
		var inMinutes float64
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM trades
			 WHERE ((base_asset = $1 AND quote_asset = $2) OR (base_asset = $2 AND quote_asset = $1))
			   AND date_trunc('minute', ts) = ANY($3::timestamptz[])`,
			pair.Base.String(), pair.Quote.String(), minutes).Scan(&inMinutes); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s, %d minutes: %.0f trades rows and %.0f compressed batches read, %.0f removed by join filter, %.0f in the listed "+
			"minutes, %.1f ms, scans %v", stage, n, read, batches, joinRemoved, inMinutes, doc[0].ExecutionTime, scans)
		// A minute's rows of one stored direction straddle at most two batches.
		if batches > float64(2*2*n) {
			t.Errorf("%s, %d minutes: plan reads %.0f compressed batches; want at most %d:\n%s", stage, n, batches, 4*n, raw)
		}
		// 1.25x absorbs EXPLAIN's per-loop rounding of Actual Rows; a time-only scan reads 30x the rows.
		if inMinutes == 0 || read > 1.25*inMinutes || joinRemoved > 0 {
			t.Errorf("%s, %d minutes: plan reads %.0f trades rows (join filter drops %.0f); want at most the %0.f in the listed minutes:\n%s",
				stage, n, read, joinRemoved, inMinutes, raw)
		}
	}
	cases := []struct{ n, step int }{{5, 240}, {400, 3}}

	if got := chunkStats(); got != 0 {
		t.Fatalf("fixture: the chunk already has %d statistics rows; the no-statistics pass would not be one", got)
	}
	for _, tc := range cases {
		probe("no statistics", tc.n, tc.step)
	}
	if got := chunkStats(); got != 0 {
		t.Fatalf("fixture: statistics appeared during the no-statistics pass (%d rows)", got)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE trades`); err != nil {
		t.Fatal(err)
	}
	if chunkStats() == 0 {
		t.Fatal("fixture: ANALYZE left the chunk without statistics")
	}
	for _, tc := range cases {
		probe("analyzed", tc.n, tc.step)
	}

	// A later chunk stays uncompressed; the day's chunk is compressed (whole-chunk compress_chunk, never a merge).
	d1 := d0.AddDate(0, 0, 30)
	insertDay(d1, 3600, 90000000)
	if _, err := db.ExecContext(ctx, `SELECT compress_chunk(ch) FROM show_chunks('trades', older_than => $1::timestamptz) ch`,
		d0.AddDate(0, 0, 14)); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'trades' AND is_compressed`).Scan(&compressed); err != nil || compressed != 1 {
		t.Fatalf("fixture: %d compressed trades chunks (%v), want 1", compressed, err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE trades`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		probe("compressed", tc.n, tc.step)
	}

	var minutes []time.Time
	for i := 0; i < 200; i++ {
		minutes = append(minutes, d0.Add(time.Duration(i*7)*time.Minute))
	}
	for i := 0; i < 30; i++ {
		minutes = append(minutes, d1.Add(time.Duration(i*2)*time.Minute))
	}
	for _, limit := range []int{10000, 50} {
		rows, err := db.QueryContext(ctx, `
			SELECT source, ledger, tx_hash, op_index, ts,
			       CASE WHEN base_asset = $1 THEN base_amount ELSE quote_amount END::text,
			       CASE WHEN base_asset = $1 THEN quote_amount ELSE base_amount END::text
			  FROM (SELECT * FROM trades
			         WHERE ((base_asset = $1 AND quote_asset = $2) OR (base_asset = $2 AND quote_asset = $1))
			           AND date_trunc('minute', ts) = ANY($3::timestamptz[])
			         ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
			         LIMIT $4) r
			 ORDER BY ts, ledger, tx_hash, op_index, source`,
			pair.Base.String(), pair.Quote.String(), minutes, limit)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for rows.Next() {
			var src, tx, base, quote string
			var ledger, op int64
			var ts time.Time
			if err := rows.Scan(&src, &ledger, &tx, &op, &ts, &base, &quote); err != nil {
				t.Fatal(err)
			}
			want = append(want, fmt.Sprintf("%s:%d:%s:%d:%d|%s|%s", src, ledger, tx, op, ts.UTC().UnixNano(), base, quote))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		got, err := store.TradesInMinutes(ctx, pair, minutes, limit)
		if err != nil {
			t.Fatal(err)
		}
		var fromCompressed int
		for _, g := range got {
			if g.Timestamp.Before(d1) {
				fromCompressed++
			}
		}
		if len(want) < limit && (fromCompressed == 0 || fromCompressed == len(got)) {
			t.Fatalf("limit %d: %d of %d rows from the compressed chunk, want rows from both chunks", limit, fromCompressed, len(got))
		}
		if len(got) != len(want) {
			t.Fatalf("limit %d: %d rows, the reference query has %d", limit, len(got), len(want))
		}
		for i, g := range got {
			if k := g.ID() + "|" + g.BaseAmount.String() + "|" + g.QuoteAmount.String(); k != want[i] {
				t.Fatalf("limit %d row %d = %s, the reference query has %s", limit, i, k, want[i])
			}
		}
		t.Logf("limit %d: %d rows equal the reference query's, in order", limit, len(got))
	}
}
