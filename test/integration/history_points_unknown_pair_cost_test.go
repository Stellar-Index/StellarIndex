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

// The since-inception cost fixture. The measured CAGGs' materialisation
// hypertables are re-chunked to 10 days for the fixture (they inherit
// 70 days from `trades`' 7-day interval, migration 0062 — MORE chunks
// is the adverse case for these reads, and 10 days keeps the seed
// small), so costFixtureDays of hourly trades spreads each over ~14
// chunks, and costFixturePairs markets trading every hour put ~9,600
// rows (a multi-level pair index of well over a hundred leaf pages)
// into each of them. That density is the point: an index PROBE and an
// every-row WALK are indistinguishable on a chunk whose whole index is
// one page.
const (
	costFixturePairs  = 40
	costFixtureDays   = 130
	costFixtureIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	// historyMaxPoints in internal/api/v1/history.go — the bucket limit
	// the anonymous series routes actually pass.
	costFixtureLimit = 50_000
	// Ceiling on shared buffers per (chunk, direction) for proving a
	// pair absent: one b-tree descent. Measured at 2.4–3.0 on this
	// fixture; 6 leaves slack for a taller tree or a right-link step
	// without admitting anything that scales with the chunk's rows (one
	// market's rows in one chunk are ~240 heap fetches here, the whole
	// chunk ~9,600).
	maxBuffersPerChunkProbe = 6
)

var costPlanModes = []string{"force_custom_plan", "force_generic_plan"}

// costReader is one anonymous-reachable series read that can be driven
// with NO lower time bound.
type costReader struct {
	name string
	view string // the CAGG it reads; also the capture needle
	call func(ctx context.Context, s *timescale.Store, p c.Pair) (int, error)
}

var costReaders = []costReader{
	{
		name: "HistoryPoints", view: "prices_1h",
		call: func(ctx context.Context, s *timescale.Store, p c.Pair) (int, error) {
			pts, err := s.HistoryPoints(ctx, p, timescale.Granularity1h, costFixtureLimit)
			return len(pts), err
		},
	},
	{
		name: "HistoryPointsInRange(no from/to)", view: "prices_1h",
		call: func(ctx context.Context, s *timescale.Store, p c.Pair) (int, error) {
			pts, err := s.HistoryPointsInRange(ctx, p, timescale.Granularity1h,
				time.Time{}, time.Time{}, costFixtureLimit)
			return len(pts), err
		},
	},
	{
		name: "TWAPPointsInRange(no from/to)", view: "twap_1h",
		call: func(ctx context.Context, s *timescale.Store, p c.Pair) (int, error) {
			pts, err := s.TWAPPointsInRange(ctx, p, timescale.Granularity1h,
				time.Time{}, time.Time{}, costFixtureLimit)
			return len(pts), err
		},
	},
}

// TestSeriesReadsUnknownPairCostIsBounded MEASURES the database work an
// anonymous caller buys by asking /v1/history/since-inception (or
// /v1/chart with no window) for a well-formed pair that has never
// traded — audit-2026-09-02 K008, the "no literal lower bound" leg.
//
// These reads have no lower time bound — that is what "since inception"
// means — so TimescaleDB cannot exclude a single chunk and every chunk
// of the CAGG appears in the plan. What K008 asks is whether that is a
// DB-burn lever. It is one only if the per-chunk work scales with the
// chunk's CONTENTS. This test pins that it does not: under the UNION ALL
// of single-direction branches each chunk is answered by an index probe
// that finds no entry — no filtered row, two or three index pages — so
// the whole read costs O(chunks), independent of how much history the
// chunks hold. Measured here: 68–84 shared buffers and under 1 ms for
// 14 chunks x 2 directions, against 5,597 buffers and all 124,800 rows
// fetched-then-discarded for the retired OR disjunction.
//
// What this test does NOT show is a saving from any gate in front of
// the read. There is none, and a gate that itself probes the hypertable
// buys nothing: the probe IS the per-chunk index descent measured here.
// Going below O(chunks) needs a lookup that does not touch the
// hypertable at all (a plain-table pair registry / first-seen bucket),
// which is a migration, not a change to this query.
//
// Two unknown pairs, because "unknown" is not one shape:
//
//   - neither asset exists anywhere (native/fiat:EUR here). The planner
//     can prove this empty from ANY index, so it is the easy case;
//   - both assets are heavily traded but never with EACH OTHER. This is
//     the adversarial one: a plan that drives a single-column
//     (base_asset, bucket) or (quote_asset, bucket) index and tests the
//     other column as a filter walks every row of a real market in
//     every chunk before it can answer "none".
//
// And two plan modes, because pgx prepares the statement and Postgres
// may switch a prepared statement to a GENERIC plan after five
// executions: a property that holds only under the custom plan holds
// for the first five requests per connection. (The retired OR form is
// exactly that case — cheap under a custom plan, a full walk under the
// generic one.)
//
// The statements measured are the PRODUCTION ones, not copies: the pool
// is pinned to a single connection, the Store method runs, and the text
// pgx prepared for it is read back out of pg_prepared_statements on that
// same session. A rewrite of a reader is therefore measured as written,
// and a copy in this file cannot drift from it.
func TestSeriesReadsUnknownPairCostIsBounded(t *testing.T) {
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
	// One connection: pgx's prepared-statement cache,
	// pg_prepared_statements and `SET plan_cache_mode` are all
	// per-session, so everything below must run on the same backend.
	db.SetMaxOpenConns(1)

	seedCostFixture(t, ctx, db)
	pairs := newCostPairs(t)
	assertRetiredORFormWalks(t, ctx, db, pairs)

	var captured []string
	for _, rd := range costReaders {
		chunks := caggChunkCount(t, ctx, db, rd.view)
		if chunks < 10 {
			t.Fatalf("%s materialised into %d chunks, want >= 10 — a bounded-cost claim about "+
				"a multi-chunk walk needs a multi-chunk fixture", rd.view, chunks)
		}
		// The production call, exactly as the route makes it.
		for _, p := range []c.Pair{pairs.neverSeen, pairs.neverPaired} {
			n, err := rd.call(ctx, store, p)
			if err != nil {
				t.Fatalf("%s(%s): %v", rd.name, p, err)
			}
			if n != 0 {
				t.Fatalf("%s(%s) returned %d buckets, want 0 — the fixture trades this pair", rd.name, p, n)
			}
		}
		// Needles name the read, not its current shape: a reader rewritten
		// into some other form must still be captured and MEASURED, not
		// slip past as "no such statement".
		stmt := capturePreparedStatement(t, ctx, db, captured, "FROM "+rd.view, "ORDER BY bucket ASC")
		captured = append(captured, stmt)

		for _, tc := range pairs.unknown() {
			for _, mode := range costPlanModes {
				got := explainPrepared(t, ctx, db, mode, stmt, costArgs(tc.pair))
				t.Logf("%s | %s | %s: %s", rd.name, tc.name, mode, got)
				assertProbeCost(t, rd.name+" / "+tc.name+" / "+mode, got, chunks)
			}
		}

		// Instrument check, buffers: the same statement over a pair that
		// DOES trade must cost far more than the probe ceiling allows.
		// If it does not, the walker is not reading buffers out of the
		// plan or the fixture is too thin to tell a probe from a read of
		// the rows, and the PASSes above mean nothing.
		control := explainPrepared(t, ctx, db, "force_custom_plan", stmt, costArgs(pairs.traded))
		t.Logf("%s | control: traded pair, full series: %s", rd.name, control)
		if ceiling := int64(2*chunks) * maxBuffersPerChunkProbe; control.buffers < 5*ceiling {
			t.Fatalf("%s: instrument check failed — reading a traded pair's whole series cost %d "+
				"buffers, under 5x the %d-buffer probe ceiling; the fixture no longer separates an "+
				"index probe from a read of the rows", rd.name, control.buffers, ceiling)
		}
	}
}

// assertRetiredORFormWalks is the instrument check for the
// rows-removed-by-filter detector, run on the known-bad case before any
// PASS is believed: the pre-split `(A AND B) OR (B AND A)` disjunction
// these readers carried until audit-2026-09-02 F169. Under the generic
// plan a prepared statement settles into, it must show the walk this
// test exists to rule out — every row of the CAGG fetched and discarded
// — and [assertProbeCost]'s own bounds must reject it. The custom-plan
// runs are logged beside it for the record and not asserted: the
// planner rescues them with a BitmapOr, which is why the defect hid.
func assertRetiredORFormWalks(t *testing.T, ctx context.Context, db *sql.DB, pairs costPairs) {
	t.Helper()
	const orForm = `
		SELECT bucket, base_asset, vwap::text, COALESCE(volume, 0)::text, volume_usd::text
		  FROM prices_1h
		 WHERE ((base_asset = $1 AND quote_asset = $2)
		     OR (base_asset = $2 AND quote_asset = $1))
		   AND bucket <= now() - INTERVAL '1 hour'
		 ORDER BY bucket ASC
		 LIMIT $3`
	var fixtureRows int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM prices_1h`).Scan(&fixtureRows); err != nil {
		t.Fatalf("count prices_1h: %v", err)
	}
	chunks := caggChunkCount(t, ctx, db, "prices_1h")
	t.Logf("fixture: %d prices_1h rows over %d chunks", fixtureRows, chunks)
	for _, tc := range pairs.unknown() {
		for _, mode := range costPlanModes {
			got := explainPrepared(t, ctx, db, mode, orForm, costArgs(tc.pair))
			t.Logf("retired OR disjunction | %s | %s: %s", tc.name, mode, got)
			if mode != "force_generic_plan" {
				continue
			}
			ceiling := int64(got.chunkScans) * maxBuffersPerChunkProbe
			if got.rowsRemovedByFilter < fixtureRows || got.buffers <= ceiling {
				t.Fatalf("instrument check failed: the retired OR disjunction (%s, generic plan) "+
					"filtered %d of %d rows over %d buffers (probe ceiling %d) — it no longer shows "+
					"the every-row walk, so this fixture cannot tell a bounded read from an unbounded one",
					tc.name, got.rowsRemovedByFilter, fixtureRows, got.buffers, ceiling)
			}
		}
	}
}

// assertProbeCost pins the bounded-cost property for one measured run.
func assertProbeCost(t *testing.T, what string, got planCost, chunks int) {
	t.Helper()
	if got.chunkScans == 0 {
		t.Fatalf("%s: EXPLAIN shows no executed chunk scan — the plan walker is not seeing the plan", what)
	}
	if got.chunkScans > 2*chunks {
		t.Errorf("%s: %d chunk scans for %d chunks x 2 directions — a chunk is being scanned twice",
			what, got.chunkScans, chunks)
	}
	if got.seqScans != 0 {
		t.Errorf("%s: %d sequential chunk scan(s) (%v) — proving a pair absent must be an index "+
			"probe per chunk", what, got.seqScans, got.scanTypes)
	}
	if got.rowsRemovedByFilter != 0 {
		t.Errorf("%s: %d rows were fetched and then discarded by a filter — the read is walking "+
			"another market's rows, so its cost scales with the chunks' contents",
			what, got.rowsRemovedByFilter)
	}
	if ceiling := int64(got.chunkScans) * maxBuffersPerChunkProbe; got.buffers > ceiling {
		t.Errorf("%s: %d shared buffers over %d chunk scans, want <= %d (%d per probe: one "+
			"b-tree descent) — the per-chunk cost is no longer O(1)",
			what, got.buffers, got.chunkScans, ceiling, maxBuffersPerChunkProbe)
	}
}

type costPairs struct {
	neverSeen   c.Pair // neither asset appears in the fixture
	neverPaired c.Pair // both assets heavily traded, never against each other
	traded      c.Pair // a real fixture market
}

type namedCostPair struct {
	name string
	pair c.Pair
}

func (p costPairs) unknown() []namedCostPair {
	return []namedCostPair{
		{"neither asset exists", p.neverSeen},
		{"both assets traded, never together", p.neverPaired},
	}
}

func newCostPairs(t *testing.T) costPairs {
	t.Helper()
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	// FILL01 is the base of a USDC-quoted market; native is the quote of
	// markets FILL21..FILL40. Both are everywhere; FILL01/native is not.
	fill01, err := c.NewClassicAsset("FILL01", costFixtureIssuer)
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := c.NewClassicAsset("USDC", costFixtureIssuer)
	if err != nil {
		t.Fatal(err)
	}
	var out costPairs
	if out.neverSeen, err = c.NewPair(c.NativeAsset(), eur); err != nil {
		t.Fatal(err)
	}
	if out.neverPaired, err = c.NewPair(fill01, c.NativeAsset()); err != nil {
		t.Fatal(err)
	}
	if out.traded, err = c.NewPair(fill01, usdc); err != nil {
		t.Fatal(err)
	}
	return out
}

// costArgs is the argument list every measured reader binds: the pair,
// then the 2n+1 row cap its bucket limit becomes (bucketRowCap).
func costArgs(p c.Pair) []any {
	return []any{p.Base.String(), p.Quote.String(), 2*costFixtureLimit + 1}
}

// seedCostFixture writes one trade per hour per market for
// costFixtureDays, ending 10 days back so every bucket is closed, and
// materialises the measured CAGGs over it. Markets FILL01..FILL20 quote
// in USDC and FILL21..FILL40 in native, so every asset the adversarial
// unknown pair names is heavily traded — just never against the other.
func seedCostFixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, view := range []string{"prices_1h", "twap_1h"} {
		var matTable string
		if err := db.QueryRowContext(ctx, `
			SELECT format('%I.%I', materialization_hypertable_schema, materialization_hypertable_name)
			  FROM timescaledb_information.continuous_aggregates
			 WHERE view_name = $1`, view).Scan(&matTable); err != nil {
			t.Fatalf("resolve %s materialisation hypertable: %v", view, err)
		}
		if _, err := db.ExecContext(ctx,
			`SELECT set_chunk_time_interval($1::regclass, INTERVAL '10 days')`, matTable); err != nil {
			t.Fatalf("re-chunk %s: %v", matTable, err)
		}
	}
	end := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		SELECT 'sdex',
		       70000000 + h,
		       lpad(to_hex(h::bigint * 1000 + p), 64, '0'),
		       0,
		       $1::timestamptz - make_interval(hours => h),
		       'FILL' || lpad(p::text, 2, '0') || '-' || $2::text,
		       CASE WHEN p <= $4::int / 2 THEN 'USDC-' || $2::text ELSE 'native' END,
		       1::numeric, 2::numeric, 2::numeric
		  FROM generate_series(1, $3::int) AS h,
		       generate_series(1, $4::int) AS p`,
		end, costFixtureIssuer, costFixtureDays*24, costFixturePairs,
	); err != nil {
		t.Fatalf("seed cost fixture: %v", err)
	}
	// twap_1h is hierarchical over prices_1m, so that refreshes first.
	for _, view := range []string{"prices_1m", "prices_1h", "twap_1h"} {
		if _, err := db.ExecContext(ctx,
			`CALL refresh_continuous_aggregate($1::regclass, NULL, NULL)`, view); err != nil {
			t.Fatalf("refresh %s: %v", view, err)
		}
	}
	// Planner statistics for the freshly materialised chunks: without
	// them the plans below are chosen on default estimates, which is
	// not the state any served database is in.
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func caggChunkCount(t *testing.T, ctx context.Context, db *sql.DB, view string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*)
		  FROM timescaledb_information.chunks ch
		  JOIN timescaledb_information.continuous_aggregates ca
		    ON ca.materialization_hypertable_schema = ch.hypertable_schema
		   AND ca.materialization_hypertable_name   = ch.hypertable_name
		 WHERE ca.view_name = $1`, view).Scan(&n); err != nil {
		t.Fatalf("count %s chunks: %v", view, err)
	}
	return n
}

// capturePreparedStatement returns the text of the one statement this
// session has prepared that mentions every needle and is not already in
// `seen`. database/sql via pgx prepares each distinct query once per
// connection, and pg_prepared_statements keeps the full text
// (pg_stat_activity would truncate it at track_activity_query_size).
func capturePreparedStatement(t *testing.T, ctx context.Context, db *sql.DB, seen []string, needles ...string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT statement FROM pg_prepared_statements`)
	if err != nil {
		t.Fatalf("read pg_prepared_statements: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var found []string
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			t.Fatalf("scan pg_prepared_statements: %v", err)
		}
		// Never this file's own probes: a `PREPARE … AS <stmt>` or an
		// EXPLAIN wrapper carries the production text inside it.
		head := strings.ToUpper(strings.TrimSpace(stmt))
		match := !strings.HasPrefix(head, "PREPARE") && !strings.HasPrefix(head, "EXPLAIN") &&
			!strings.Contains(stmt, "pg_prepared_statements") && !slices.Contains(seen, stmt)
		for _, n := range needles {
			match = match && strings.Contains(stmt, n)
		}
		if match {
			found = append(found, stmt)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_prepared_statements rows: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("captured %d new prepared statements matching %v, want exactly 1 — the production "+
			"statement was not prepared on this session, so there is nothing honest to measure: %q",
			len(found), needles, found)
	}
	return found[0]
}

// planCost is what one EXPLAIN (ANALYZE, BUFFERS) run cost, reduced to
// the properties the bounded-cost claim is made of.
type planCost struct {
	buffers             int64 // shared hit + read, whole statement (the root node is inclusive)
	chunkScans          int   // executed scan nodes over a _hyper_*_chunk relation
	seqScans            int   // … of which are sequential
	rowsRemovedByFilter int64
	planningMS          float64
	executionMS         float64
	scanTypes           map[string]int
	indexes             map[string]int // index name with the per-chunk prefix stripped
}

func (p planCost) String() string {
	b, _ := json.Marshal(map[string]any{
		"shared_buffers": p.buffers, "chunk_scans": p.chunkScans, "seq_scans": p.seqScans,
		"rows_removed_by_filter": p.rowsRemovedByFilter, "scan_types": p.scanTypes,
		"indexes": p.indexes, "planning_ms": p.planningMS, "execution_ms": p.executionMS,
	})
	return string(b)
}

type explainNode struct {
	NodeType            string        `json:"Node Type"`
	RelationName        string        `json:"Relation Name"`
	IndexName           string        `json:"Index Name"`
	ActualLoops         float64       `json:"Actual Loops"`
	SharedHitBlocks     int64         `json:"Shared Hit Blocks"`
	SharedReadBlocks    int64         `json:"Shared Read Blocks"`
	RowsRemovedByFilter int64         `json:"Rows Removed by Filter"`
	Plans               []explainNode `json:"Plans"`
}

// explainPrepared runs stmt through PREPARE / EXPLAIN (ANALYZE, BUFFERS)
// EXECUTE under the given plan_cache_mode, twice — the first run pays
// for loading the chunks' catalog and relcache entries, which is not
// the statement's own cost — and returns the second.
//
// PREPARE + EXECUTE rather than a parameterised EXPLAIN because only a
// prepared statement has a generic plan to force. EXECUTE is a utility
// statement and takes no bind parameters, so the arguments are
// rendered as literals; they are this test's own asset ids and ints.
func explainPrepared(t *testing.T, ctx context.Context, db *sql.DB, planMode, stmt string, args []any) planCost {
	t.Helper()
	const name = "k008_cost_probe"
	mustExec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`SET plan_cache_mode = ` + planMode)
	mustExec(`PREPARE ` + name + ` AS ` + stmt)
	defer mustExec(`DEALLOCATE ` + name)

	lits := make([]string, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case string:
			lits[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		case int:
			lits[i] = fmt.Sprintf("%d", v)
		default:
			t.Fatalf("explainPrepared: unsupported arg type %T", a)
		}
	}
	q := `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE ` + name + `(` + strings.Join(lits, ", ") + `)`
	var raw string
	for range 2 {
		if err := db.QueryRowContext(ctx, q).Scan(&raw); err != nil {
			t.Fatalf("EXPLAIN ANALYZE: %v", err)
		}
	}
	return parsePlanCost(t, raw)
}

func parsePlanCost(t *testing.T, raw string) planCost {
	t.Helper()
	var doc []struct {
		Plan          explainNode `json:"Plan"`
		PlanningTime  float64     `json:"Planning Time"`
		ExecutionTime float64     `json:"Execution Time"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
	}
	out := planCost{
		buffers:     doc[0].Plan.SharedHitBlocks + doc[0].Plan.SharedReadBlocks,
		planningMS:  doc[0].PlanningTime,
		executionMS: doc[0].ExecutionTime,
		scanTypes:   map[string]int{},
		indexes:     map[string]int{},
	}
	var walk func(n explainNode)
	walk = func(n explainNode) {
		if strings.Contains(n.RelationName, "_hyper_") && n.ActualLoops > 0 {
			out.chunkScans++
			out.scanTypes[n.NodeType]++
			if n.NodeType == "Seq Scan" {
				out.seqScans++
			}
		}
		if n.IndexName != "" && n.ActualLoops > 0 {
			// _hyper_<h>_<c>_chunk_<index> → <index>, so the per-chunk
			// copies of one index count together.
			name := n.IndexName
			if i := strings.Index(name, "_chunk_"); i >= 0 {
				name = name[i+len("_chunk_"):]
			}
			out.indexes[name]++
		}
		out.rowsRemovedByFilter += n.RowsRemovedByFilter
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(doc[0].Plan)
	return out
}
