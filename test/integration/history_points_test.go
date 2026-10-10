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

// TestHistoryPointsDirectionUnion executes the two CAGG series reads
// behind /v1/history/since-inception and /v1/chart against a real
// TimescaleDB after they were rewritten from a both-directions OR
// disjunction into a UNION ALL of two single-direction branches with a
// sargable closed-bucket bound.
//
// The shape itself is pinned without a database by the scanning guard in
// internal/storage/timescale/prices_1m_direction_union_test.go. What
// only a live database can prove is that the rewritten statements still
// PARSE, still bind their parameters in the right order once the
// optional from/to/limit clauses move inside the branches, and still
// serve the same numbers: both stored orientations folded into the
// requested one, chronological order, the bucket LIMIT counting BUCKETS
// (not rows) across the two branches, the in-progress bucket excluded,
// and an unknown pair answered with an empty series rather than an
// error.
func TestHistoryPointsDirectionUnion(t *testing.T) {
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
	xlmUSDC, _ := c.NewPair(c.NativeAsset(), usdc) // requested orientation
	usdcXLM, _ := c.NewPair(usdc, c.NativeAsset()) // the flipped storage direction

	// Three closed 1-minute buckets ~2h back. The middle one is stored
	// ONLY in the flipped orientation — a one-direction read drops it,
	// and a UNION ALL that lost a branch would drop it too.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	trades := []c.Trade{
		mkAPITrade(41, t0, xlmUSDC, 1_000_000, 500_000),                      // 0.5
		mkAPITrade(42, t0.Add(2*time.Minute), usdcXLM, 1_000_000, 2_000_000), // 2.0 → 0.5 inverted
		mkAPITrade(43, t0.Add(4*time.Minute), xlmUSDC, 1_000_000, 500_000),   // 0.5
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
	} {
		if _, err := store.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg: %v", err)
		}
	}

	wantBuckets := []time.Time{t0, t0.Add(2 * time.Minute), t0.Add(4 * time.Minute)}

	assertSeries := func(t *testing.T, what string, pts []timescale.HistoryPoint, want []time.Time) {
		t.Helper()
		if len(pts) != len(want) {
			t.Fatalf("%s returned %d buckets, want %d: %+v", what, len(pts), len(want), pts)
		}
		for i, p := range pts {
			if !p.Bucket.UTC().Equal(want[i]) {
				t.Errorf("%s[%d].Bucket = %s, want %s (chronological, oldest first)",
					what, i, p.Bucket.UTC(), want[i])
			}
			// Every bucket is the same market at 0.5 USDC per XLM; the
			// flipped-only bucket must arrive INVERTED from its stored 2.0.
			if px := mustFloat(t, p.VWAP); px < 0.49 || px > 0.51 {
				t.Errorf("%s[%d].VWAP = %s, want ~0.5 (both directions folded into the requested orientation)",
					what, i, p.VWAP)
			}
		}
	}

	// ── HistoryPoints: the since-inception read (no lower bound) ────
	pts, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints: %v", err)
	}
	assertSeries(t, "HistoryPoints", pts, wantBuckets)

	// The bucket LIMIT counts BUCKETS across both branches: with the cap
	// now applied per branch AND on the union, the first n buckets of the
	// merged series must still be complete and in order.
	for n := 1; n <= 3; n++ {
		capped, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1m, n)
		if err != nil {
			t.Fatalf("HistoryPoints(limit=%d): %v", n, err)
		}
		assertSeries(t, "HistoryPoints(limit)", capped, wantBuckets[:n])
	}

	// Requesting the market the other way round serves the reciprocal —
	// the same rows, folded into the flipped orientation (1/0.5 = 2.0).
	rev, err := store.HistoryPoints(ctx, usdcXLM, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(reversed): %v", err)
	}
	if len(rev) != 3 {
		t.Fatalf("HistoryPoints(reversed) returned %d buckets, want 3", len(rev))
	}
	for i, p := range rev {
		if px := mustFloat(t, p.VWAP); px < 1.99 || px > 2.01 {
			t.Errorf("HistoryPoints(reversed)[%d].VWAP = %s, want ~2.0", i, p.VWAP)
		}
	}

	// An unknown-but-well-formed pair is the DoS scenario this rewrite
	// exists for: it must answer an empty series, not an error.
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	emptyPair, _ := c.NewPair(c.NativeAsset(), eur)
	empty, err := store.HistoryPoints(ctx, emptyPair, timescale.Granularity1m, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(empty pair): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("HistoryPoints(empty pair) returned %d buckets, want 0", len(empty))
	}

	// ── Closed-bucket guard, in its rewritten sargable spelling ─────
	// A 1-day bucket is served only once it has CLOSED (ADR-0015:
	// `bucket <= now() - INTERVAL '1 day'`). The seed sits ~2h back, so
	// which side of that line it falls on depends on the clock: for the
	// first ~2h of a UTC day the trades land in YESTERDAY's bucket, which
	// is closed and must be served; for the rest of the day they land in
	// today's, which is open and must not be. Assert whichever is true —
	// a fixed "want 0" fails every run between 00:00 and ~02:05 UTC.
	var dayRows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM prices_1d
		  WHERE (base_asset = $1 AND quote_asset = $2)
		     OR (base_asset = $2 AND quote_asset = $1)`,
		xlmUSDC.Base.String(), xlmUSDC.Quote.String(),
	).Scan(&dayRows); err != nil {
		t.Fatalf("count prices_1d: %v", err)
	}
	if dayRows == 0 {
		t.Fatal("prices_1d holds no row for the seeded pair — the closed-bucket " +
			"assertion below would pass vacuously")
	}
	closedDays := func(now time.Time) map[time.Time]bool {
		out := map[time.Time]bool{}
		for _, tr := range trades {
			day := tr.Timestamp.UTC().Truncate(24 * time.Hour)
			if !day.Add(24 * time.Hour).After(now) {
				out[day] = true
			}
		}
		return out
	}
	closedBefore := closedDays(time.Now().UTC())
	dayPts, err := store.HistoryPoints(ctx, xlmUSDC, timescale.Granularity1d, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(1d): %v", err)
	}
	// Read the clock on both sides of the query: a run that straddles
	// midnight may legitimately see either answer.
	closedAfter := closedDays(time.Now().UTC())
	if len(dayPts) != len(closedBefore) && len(dayPts) != len(closedAfter) {
		t.Errorf("HistoryPoints(1d) returned %d buckets, want %d — only a CLOSED day "+
			"bucket is served (ADR-0015, `bucket <= now() - INTERVAL '1 day'`): %+v",
			len(dayPts), len(closedAfter), dayPts)
	}
	for _, p := range dayPts {
		if !closedAfter[p.Bucket.UTC()] {
			t.Errorf("HistoryPoints(1d) served %s, a day bucket that is still open",
				p.Bucket.UTC().Format(time.RFC3339))
		}
	}

	// ── HistoryPointsInRange: /v1/chart's windowed read ─────────────
	from := t0.Add(-time.Minute)
	to := t0.Add(5 * time.Minute)
	ranged, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m, from, to, 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange: %v", err)
	}
	assertSeries(t, "HistoryPointsInRange", ranged, wantBuckets)

	// The window must still bite once the bounds live inside the
	// branches: [t0+1m, t0+3m) keeps only the flipped-only bucket.
	narrow, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		t0.Add(time.Minute), t0.Add(3*time.Minute), 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(narrow): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(narrow)", narrow, wantBuckets[1:2])

	// Optional bounds: zero `from` means since-inception, zero `to`
	// means open-ended — both branches must still carry the rest of the
	// predicate list and the placeholders must stay in step.
	noFrom, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		time.Time{}, to, 0)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(no from): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(no from)", noFrom, wantBuckets)

	noTo, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		from, time.Time{}, 1)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(no to, limit 1): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(no to, limit 1)", noTo, wantBuckets[:1])

	// Every optional clause at once — the maximal-placeholder path
	// ($1..$5), which is what /v1/chart issues. The bounds now live
	// inside both branches and the LIMIT is bound last, so an
	// off-by-one in the placeholder arithmetic surfaces here as a
	// bind error or as the wrong window.
	all, err := store.HistoryPointsInRange(ctx, xlmUSDC, timescale.Granularity1m,
		from, to, 2)
	if err != nil {
		t.Fatalf("HistoryPointsInRange(from, to, limit 2): %v", err)
	}
	assertSeries(t, "HistoryPointsInRange(from, to, limit 2)", all, wantBuckets[:2])
}

// TestHistoryPointsUnboundedPopulatedReadStreams measures the one shape
// the unknown-pair cost test does not: /v1/history/since-inception with
// limit=0 over a pair that HAS a long history. That statement carries no
// LIMIT and an outer `ORDER BY bucket ASC, base_asset` over a UNION ALL
// whose branches are index-ordered on bucket alone, so the question is
// whether the outer sort streams or materialises the pair's whole series.
//
// Under the serving pool's plan_cache_mode = force_custom_plan (see
// OpenServing) it streams: a Merge Append on bucket feeding an
// Incremental Sort whose groups are one bucket (at most two rows). No
// full Sort node may appear. Measured on this fixture (3,120 hourly
// buckets over ~14 chunks): 3.2 ms, 29 kB peak sort memory. Adding
// base_asset to each branch's ORDER BY changes nothing — within a branch
// base_asset is bound to a parameter, so the planner drops it as a
// redundant sort key. A GENERIC plan (not used by the serving pool) is
// logged for the record: it chose Parallel Append + a full Sort, 17 ms.
func TestHistoryPointsUnboundedPopulatedReadStreams(t *testing.T) {
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
	// Prepared statements and SET are per-session: one backend throughout.
	db.SetMaxOpenConns(1)
	seedCostFixture(t, ctx, db)
	pairs := newCostPairs(t)

	pts, err := store.HistoryPoints(ctx, pairs.traded, timescale.Granularity1h, 0)
	if err != nil {
		t.Fatalf("HistoryPoints(limit=0): %v", err)
	}
	if want := costFixtureDays * 24; len(pts) != want {
		t.Fatalf("HistoryPoints(limit=0) returned %d buckets, want %d — not the populated since-inception read", len(pts), want)
	}
	stmt := capturePreparedStatement(t, ctx, db, nil, "FROM prices_1h", "ORDER BY bucket ASC")
	args := []string{pairs.traded.Base.String(), pairs.traded.Quote.String()}

	// Instrument check first: with incremental sort disabled the same
	// statement has no way to stream, so the walker must see a full Sort.
	mustExecPlan(t, ctx, db, `SET enable_incremental_sort = off`)
	control := planNodeTypes(t, ctx, db, "force_custom_plan", stmt, args)
	mustExecPlan(t, ctx, db, `RESET enable_incremental_sort`)
	if !slices.Contains(control, "Sort") {
		t.Fatalf("instrument check failed: with incremental sort off the plan %v has no Sort node — "+
			"the walker is not reading the plan", control)
	}

	served := planNodeTypes(t, ctx, db, "force_custom_plan", stmt, args)
	t.Logf("force_custom_plan: %v", served)
	if slices.Contains(served, "Sort") {
		t.Errorf("serving-mode plan materialises the whole series in a full Sort: %v", served)
	}
	if !slices.Contains(served, "Merge Append") {
		t.Errorf("serving-mode plan does not merge the two index-ordered branches: %v", served)
	}
	t.Logf("force_generic_plan (not the serving pool's mode): %v",
		planNodeTypes(t, ctx, db, "force_generic_plan", stmt, args))
}

func mustExecPlan(t *testing.T, ctx context.Context, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// planNodeTypes prepares stmt under planMode and returns every node type
// of its EXPLAIN ANALYZE plan, depth-first. args are string literals.
func planNodeTypes(t *testing.T, ctx context.Context, db *sql.DB, planMode, stmt string, args []string) []string {
	t.Helper()
	const name = "unbounded_plan_probe"
	mustExecPlan(t, ctx, db, `SET plan_cache_mode = `+planMode)
	mustExecPlan(t, ctx, db, `PREPARE `+name+` AS `+stmt)
	defer mustExecPlan(t, ctx, db, `DEALLOCATE `+name)
	lits := ""
	for i, a := range args {
		if i > 0 {
			lits += ", "
		}
		lits += "'" + a + "'"
	}
	var raw string
	if err := db.QueryRowContext(ctx,
		`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE `+name+`(`+lits+`)`).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var doc []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse EXPLAIN json (%v): %s", err, raw)
	}
	var out []string
	var walk func(n explainNode)
	walk = func(n explainNode) {
		out = append(out, n.NodeType)
		for _, ch := range n.Plans {
			walk(ch)
		}
	}
	walk(doc[0].Plan)
	return out
}

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
// traded (the "no literal lower bound" case).
//
// These reads have no lower time bound — that is what "since inception"
// means — so TimescaleDB cannot exclude a single chunk and every chunk
// of the CAGG appears in the plan. The question is whether that is a
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
// PASS is believed: the retired `(A AND B) OR (B AND A)` disjunction
// form of these readers. Under the generic
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
