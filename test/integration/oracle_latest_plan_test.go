// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// oracleLatestReferenceSQL is the pre-rewrite statement, kept as the
// semantic oracle the served query must agree with row for row.
const oracleLatestReferenceSQL = `
	SELECT DISTINCT ON (source, quote)
	       source, asset, quote, ledger, tx_hash, op_index, ts
	  FROM oracle_updates
	 WHERE asset = ANY($1) AND ($2 = '' OR source = $2)
	 ORDER BY source, quote, ts DESC, ledger DESC`

// TestLatestOracleUpdatesForAssets_NoFullSort pins the /v1/oracle/latest
// read to a plan that does not sort every matching row. The DISTINCT ON
// form sorted all of an asset's history (318,908 rows, a 63 MB external
// merge, 541 ms warm on r1) to emit 7; the served shape
// aggregates max(ts) per (source, asset, quote) — answered from the
// compressed batches' metadata — and fetches one row per stream.
func TestLatestOracleUpdatesForAssets_NoFullSort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	db.SetMaxOpenConns(1)

	seedOracleLatestFixture(t, ctx, db)

	native := c.NativeAsset()
	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset(XLM): %v", err)
	}
	keys := []c.Asset{native, xlm}
	keyStrs := []string{native.String(), xlm.String()}

	got, err := store.LatestOracleUpdatesForAssets(ctx, keys, "")
	if err != nil {
		t.Fatalf("LatestOracleUpdatesForAssets: %v", err)
	}
	stmt := capturePreparedStatement(t, ctx, db, nil, "FROM oracle_updates", "asset = ANY($1)")

	for _, src := range []string{"", "reflector", "dia"} {
		rows, err := store.LatestOracleUpdatesForAssets(ctx, keys, src)
		if err != nil {
			t.Fatalf("LatestOracleUpdatesForAssets(%q): %v", src, err)
		}
		assertOracleLatestMatchesReference(t, ctx, db, rows, keyStrs, src)
	}

	byStream := map[string]c.OracleUpdate{}
	for _, u := range got {
		byStream[u.Source+"|"+u.Quote.String()] = u
	}
	// A stream whose only rows sit in a compressed chunk months back is
	// still "latest": the read is unbounded in age.
	if u, ok := byStream["dia|fiat:USD"]; !ok {
		t.Errorf("stale dia/USD stream (compressed-only) missing from %d rows", len(got))
	} else if u.Ledger != 7777777 {
		t.Errorf("dia/USD ledger = %d at %s, want 7777777 — the late write into the partial compressed chunk", u.Ledger, u.Timestamp)
	}
	// Same ts across two alias keys: the higher ledger wins, as before.
	if u := byStream["reflector|fiat:USD"]; u.Asset.String() != xlm.String() || u.Ledger != 999_999_999 {
		t.Errorf("reflector/USD tie = %s ledger %d, want crypto:XLM ledger 999999999", u.Asset, u.Ledger)
	}

	pc := explainOracleLatest(t, ctx, db, stmt, keyStrs)
	t.Logf("force_custom_plan: %s", pc)
	if pc.maxSortRows > pc.outRowsBound {
		t.Errorf("a Sort node processed %d rows, more than the %d (source, asset, quote) streams "+
			"the answer is drawn from — the plan sorts history again: %v", pc.maxSortRows, pc.outRowsBound, pc.nodes)
	}
	// Rows the compressed tier hands up: one per batch when max(ts) comes
	// from batch metadata, every row when it decompresses. The Sort bound
	// above cannot see the second — a HashAggregate absorbs it.
	if pc.columnarRows*10 > pc.matchingRows {
		t.Errorf("compressed-chunk scans emitted %d of %d matching rows — the aggregate decompresses "+
			"history instead of reading batch metadata: %v", pc.columnarRows, pc.matchingRows, pc.nodes)
	}
	if pc.tempBlocks > 0 {
		t.Errorf("plan spilled %d temp blocks: %v", pc.tempBlocks, pc.nodes)
	}
}

// seedOracleLatestFixture writes 3 sources x 3 assets x 2 quotes at 30-min
// cadence over 120 days (~100k rows across several chunks), plus a stream
// that stopped 100 days ago and a cross-alias (ts) tie, then compresses
// every chunk older than a week as the r1 policy does.
func seedOracleLatestFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Hour)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", strings.TrimSpace(q)[:60], err)
		}
	}
	const insert = `
		INSERT INTO oracle_updates
		       (source, contract_id, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
		SELECT s.source, NULL,
		       (extract(epoch FROM g.ts)::bigint / 5)::int * 4 + array_position($2::text[], a.asset),
		       md5(s.source || a.asset || q.quote || g.ts::text) || md5(g.ts::text || s.source),
		       0, g.ts, a.asset, q.quote, 1000 + (extract(epoch FROM g.ts)::bigint % 97), 7
		  FROM unnest($1::text[]) AS s(source),
		       unnest($2::text[]) AS a(asset),
		       unnest($3::text[]) AS q(quote),
		       generate_series($4::timestamptz, $5::timestamptz, $6::interval) AS g(ts)`
	exec(insert, "{reflector,band,redstone}", "{native,crypto:XLM,crypto:BTC}", "{fiat:USD,fiat:EUR}",
		now.Add(-120*24*time.Hour), now.Add(-time.Hour), "30 minutes")
	exec(insert, "{dia}", "{crypto:XLM}", "{fiat:USD}",
		now.Add(-110*24*time.Hour), now.Add(-100*24*time.Hour), "1 hour")
	// reflector publishes the newest XLM/USD reading under both alias keys at
	// the same ts; the crypto:XLM row carries the higher ledger.
	exec(`INSERT INTO oracle_updates (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
	      VALUES ('reflector', 999999999, repeat('a', 64), 1, $1, 'crypto:XLM', 'fiat:USD', 5, 7)`, now)
	exec(`INSERT INTO oracle_updates (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
	      VALUES ('reflector', 999999998, repeat('b', 64), 1, $1, 'native', 'fiat:USD', 5, 7)`, now)

	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c) FROM show_chunks('oracle_updates', older_than => now() - INTERVAL '7 days') c
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed < 2 {
		t.Fatalf("compressed %d oracle_updates chunks, want >= 2 — the fixture would not exercise the compressed tier", compressed)
	}
	// A late write into a compressed chunk (the ON CONFLICT upsert path on a
	// replay) leaves it partial; it is also dia's newest reading.
	exec(`INSERT INTO oracle_updates (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
	      VALUES ('dia', 7777777, repeat('c', 64), 0, $1, 'crypto:XLM', 'fiat:USD', 5, 7)`,
		now.Add(-99*24*time.Hour))
	exec(`ANALYZE oracle_updates`)
}

func assertOracleLatestMatchesReference(t *testing.T, ctx context.Context, db *sql.DB, got []c.OracleUpdate, keys []string, src string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, oracleLatestReferenceSQL, keys, src)
	if err != nil {
		t.Fatalf("reference query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var want []string
	for rows.Next() {
		var source, asset, quote, txHash string
		var ledger, opIndex int
		var ts time.Time
		if err := rows.Scan(&source, &asset, &quote, &ledger, &txHash, &opIndex, &ts); err != nil {
			t.Fatalf("reference scan: %v", err)
		}
		want = append(want, fmt.Sprintf("%s|%s|%s|%d|%s|%d|%s", source, asset, quote, ledger, txHash, opIndex, ts.UTC().Format(time.RFC3339Nano)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reference rows: %v", err)
	}
	have := make([]string, 0, len(got))
	for _, u := range got {
		have = append(have, fmt.Sprintf("%s|%s|%s|%d|%s|%d|%s", u.Source, u.Asset, u.Quote, u.Ledger, u.TxHash, u.OpIndex, u.Timestamp.UTC().Format(time.RFC3339Nano)))
	}
	slices.Sort(want)
	slices.Sort(have)
	if len(want) == 0 {
		t.Fatalf("reference returned no rows for source %q — the comparison would be vacuous", src)
	}
	if !slices.Equal(have, want) {
		t.Errorf("source %q: served rows differ from the DISTINCT ON reference\n got: %v\nwant: %v", src, have, want)
	}
}

type oracleLatestPlan struct {
	maxSortRows  int64
	outRowsBound int64
	tempBlocks   int64
	columnarRows int64
	matchingRows int64
	executionMS  float64
	nodes        []string
}

func (p oracleLatestPlan) String() string {
	return fmt.Sprintf("max_sort_rows=%d streams=%d columnar_rows=%d matching_rows=%d temp_blocks=%d execution_ms=%.1f",
		p.maxSortRows, p.outRowsBound, p.columnarRows, p.matchingRows, p.tempBlocks, p.executionMS)
}

func explainOracleLatest(t *testing.T, ctx context.Context, db *sql.DB, stmt string, keys []string) oracleLatestPlan {
	t.Helper()
	var streams, matching int64
	if err := db.QueryRowContext(ctx,
		`SELECT count(DISTINCT (source, asset, quote)), count(*) FROM oracle_updates WHERE asset = ANY($1)`,
		keys).Scan(&streams, &matching); err != nil {
		t.Fatalf("count streams: %v", err)
	}
	// The serving pool's plan mode and r1's random_page_cost (postgresql.conf.j2).
	mustExecPlan(t, ctx, db, `SET plan_cache_mode = force_custom_plan`)
	mustExecPlan(t, ctx, db, `SET random_page_cost = 1.1`)
	defer mustExecPlan(t, ctx, db, `RESET random_page_cost`)
	mustExecPlan(t, ctx, db, `PREPARE oracle_latest_probe AS `+stmt)
	defer mustExecPlan(t, ctx, db, `DEALLOCATE oracle_latest_probe`)
	defer mustExecPlan(t, ctx, db, `RESET plan_cache_mode`)
	q := `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE oracle_latest_probe('{` + strings.Join(keys, ",") + `}', '')`
	var raw string
	for range 2 {
		if err := db.QueryRowContext(ctx, q).Scan(&raw); err != nil {
			t.Fatalf("EXPLAIN ANALYZE: %v", err)
		}
	}
	type node struct {
		NodeType          string  `json:"Node Type"`
		Provider          string  `json:"Custom Plan Provider"`
		ActualRows        float64 `json:"Actual Rows"`
		ActualLoops       float64 `json:"Actual Loops"`
		TempWrittenBlocks int64   `json:"Temp Written Blocks"`
		Plans             []node  `json:"Plans"`
	}
	var doc []struct {
		Plan          node    `json:"Plan"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
	}
	out := oracleLatestPlan{outRowsBound: streams, matchingRows: matching, executionMS: doc[0].ExecutionTime, tempBlocks: doc[0].Plan.TempWrittenBlocks}
	var walk func(n node)
	walk = func(n node) {
		out.nodes = append(out.nodes, strings.TrimSpace(n.NodeType+" "+n.Provider))
		if strings.HasPrefix(n.Provider, "Columnar") || n.Provider == "DecompressChunk" {
			out.columnarRows += int64(n.ActualRows * n.ActualLoops)
		}
		if n.NodeType == "Sort" && int64(n.ActualRows*n.ActualLoops) > out.maxSortRows {
			out.maxSortRows = int64(n.ActualRows * n.ActualLoops)
		}
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(doc[0].Plan)
	return out
}
