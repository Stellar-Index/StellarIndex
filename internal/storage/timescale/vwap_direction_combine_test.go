// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Regression suite for audit-2026-07-23 R-004 / R-006 / R-007 (MNY-06):
// the served-VWAP direction combine.
//
// SDEX stores the same market in BOTH orientations — the decoder files
// each trade in the venue's observed base/quote ordering rather than
// re-orienting it — so every serving read has to fold two directions
// into the one the caller asked for. Two defects lived here:
//
//	R-004 / R-007: the fold weighted the two directions' prices by TRADE
//	  COUNT. A count-weighted mean of {vwap, 1/vwap_flipped} is provably
//	  not the union's VWAP: it lets N dust trades outvote one whale.
//	R-006: /v1/history and the default /v1/chart didn't fold at all —
//	  they read one stored orientation and silently dropped every trade
//	  recorded the other way round.
//
// The canonical fixture below is one bucket of a two-sided XLM/USDC
// market whose correct union VWAP (0.4) and count-weighted answer (0.23)
// are far apart, so any regression to a count-weighted (or single-
// direction) combine is a loud failure rather than a rounding wobble.

// combineFixture is one bucket of a two-sided market, in the shape the
// CAGG stores it. Requested orientation is XLM/USDC.
//
//	stored XLM/USDC (unflipped): vwap 0.5, volume 100
//	    → 100 XLM changed hands for 50 USDC
//	stored USDC/XLM (flipped)  : vwap 5,   volume 10
//	    → 10 USDC changed hands for 50 XLM (its own vwap is XLM-per-USDC)
//
// Union: 60 USDC / 150 XLM = 0.4 USDC per XLM, exactly.
//
// The pre-fix count-weighted answer, with the flipped side inverted to
// 1/5 = 0.2 and the counts below, is (0.5·1 + 0.2·9) / 10 = 0.23 — off by
// 42%, and driven entirely by how many prints each side happened to make.
var combineFixture = []dirVWAP{
	{vwapText: "0.5", volumeText: "100", flipped: false},
	{vwapText: "5", volumeText: "10", flipped: true},
}

const (
	combineFixtureUnionVWAP = "0.4"  // Σquote / Σbase — the correct answer
	combineFixtureCountVWAP = "0.23" // the pre-fix trade-count-weighted answer
)

// TestCombineDirVWAP_VolumeWeightedUnion pins the exact combined value
// for the cases that matter on the money surface.
func TestCombineDirVWAP_VolumeWeightedUnion(t *testing.T) {
	cases := []struct {
		name string
		rows []dirVWAP
		want string
	}{
		{
			// The headline case — see combineFixture.
			name: "two-sided-market",
			rows: combineFixture,
			want: combineFixtureUnionVWAP,
		},
		{
			// Direction order must not matter: VWAP is a ratio of sums.
			name: "two-sided-market-row-order-swapped",
			rows: []dirVWAP{combineFixture[1], combineFixture[0]},
			want: combineFixtureUnionVWAP,
		},
		{
			// A whale on the flipped side against a dust print on the
			// requested side. Volume weighting puts the answer next to the
			// whale's price (1.0); ANY count-based weighting drags it
			// halfway to the dust print's 2.0. This is the economic shape
			// of the defect.
			name: "whale-flipped-vs-dust-unflipped",
			rows: []dirVWAP{
				{vwapText: "2", volumeText: "1", flipped: false},
				{vwapText: "1", volumeText: "999", flipped: true},
			},
			want: "1.001",
		},
		{
			// R-004's scenario verbatim: ONE large forward fill against
			// MANY tiny flipped fills at a different price.
			//   forward: 999 900 XLM @ 0.1 USDC  →  99 990 USDC / 999 900 XLM
			//   flipped:      25 USDC @ 0.25     →      25 USDC /     100 XLM
			// Union = 100 015 / 1 000 000 = 0.100015 — within 0.015% of the
			// whale's price, as a VWAP must be. The pre-fix count-weighted
			// answer, with 1 forward print against 100 flipped ones, was
			// (0.1·1 + 0.25·100)/101 ≈ 0.2485 — 2.5× the true price, and
			// movable at will by anyone willing to emit dust prints.
			name: "one-whale-forward-vs-many-dust-flipped",
			rows: []dirVWAP{
				{vwapText: "0.1", volumeText: "999900", flipped: false},
				{vwapText: "4", volumeText: "25", flipped: true},
			},
			want: "0.100015",
		},
		{
			// Single flipped direction → the exact inverse of the stored
			// price: 10 quote / 40 base.
			name: "single-flipped-direction",
			rows: []dirVWAP{{vwapText: "4", volumeText: "10", flipped: true}},
			want: "0.25",
		},
		{
			// A row with no volume carries no weight and must not drag the
			// answer — nor blow it up via a division.
			name: "zero-volume-row-ignored",
			rows: []dirVWAP{
				{vwapText: "0.5", volumeText: "100", flipped: false},
				{vwapText: "5", volumeText: "0", flipped: true},
			},
			want: "0.5",
		},
		{
			// Non-terminating ratio: 2 quote / 3 base. Rendered at the
			// package's relative precision (combinedVWAPSigDigits) — the
			// only lossy step in the combine.
			name: "repeating-decimal",
			rows: []dirVWAP{
				{vwapText: "1", volumeText: "1", flipped: false},
				{vwapText: "2", volumeText: "1", flipped: true},
			},
			want: "0.66666666666666666666666667",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := combineDirVWAP(tc.rows)
			if !ok {
				t.Fatalf("combineDirVWAP(%v) returned ok=false", tc.rows)
			}
			if got != tc.want {
				t.Errorf("combineDirVWAP(%v) = %s, want %s", tc.rows, got, tc.want)
			}
		})
	}
}

// TestCombineDirVWAP_IsNotTradeCountWeighted states the defect directly:
// for the fixture bucket the trade-count-weighted mean is 0.23 and the
// union VWAP is 0.4. Serving the former is R-004/R-007.
func TestCombineDirVWAP_IsNotTradeCountWeighted(t *testing.T) {
	got, ok := combineDirVWAP(combineFixture)
	if !ok {
		t.Fatal("combineDirVWAP returned ok=false for a well-formed two-sided bucket")
	}
	if got == combineFixtureCountVWAP {
		t.Fatalf("combined VWAP = %s — that is the TRADE-COUNT-weighted mean of "+
			"{0.5, 1/5}, not the volume-weighted union of the two directions", got)
	}
	if got != combineFixtureUnionVWAP {
		t.Fatalf("combined VWAP = %s, want the union Σquote/Σbase = 60/150 = %s",
			got, combineFixtureUnionVWAP)
	}
}

// TestCombineDirVWAP_SingleDirectionIsVerbatim — a bucket that traded in
// only the requested orientation IS its stored VWAP, and the Postgres
// NUMERIC text must pass through untouched so the served bytes don't
// shift for the (common) one-sided pair.
func TestCombineDirVWAP_SingleDirectionIsVerbatim(t *testing.T) {
	const stored = "0.1234567890123456789012345678901"
	got, ok := combineDirVWAP([]dirVWAP{{vwapText: stored, volumeText: "42", flipped: false}})
	if !ok {
		t.Fatal("combineDirVWAP returned ok=false for a single unflipped row")
	}
	if got != stored {
		t.Errorf("combineDirVWAP reformatted a single-direction VWAP: got %s, want %s verbatim", got, stored)
	}
}

// TestCombineDirVWAP_NoUsableRows — an empty, unparseable or non-positive
// bucket reports ok=false so callers surface "no data" rather than a
// fabricated price.
func TestCombineDirVWAP_NoUsableRows(t *testing.T) {
	cases := map[string][]dirVWAP{
		"empty":          {},
		"unparseable":    {{vwapText: "NaN", volumeText: "10", flipped: false}},
		"zero-price":     {{vwapText: "0", volumeText: "10", flipped: true}},
		"negative-price": {{vwapText: "-1", volumeText: "10", flipped: true}},
		"no-volume":      {{vwapText: "1", volumeText: "0", flipped: true}},
	}
	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := combineDirVWAP(rows); ok {
				t.Errorf("combineDirVWAP(%v) = (%s, true), want ok=false", rows, got)
			}
		})
	}
}

// TestSumUSDVolume covers the per-bucket USD-volume fold that rides along
// with the direction combine. The two stored directions hold DISJOINT
// trades, so their USD volumes add.
func TestSumUSDVolume(t *testing.T) {
	str := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	null := sql.NullString{}

	if got := sumUSDVolume([]sql.NullString{null, null}); got != nil {
		t.Errorf("all-NULL volume_usd = %q, want nil", *got)
	}
	got := sumUSDVolume([]sql.NullString{str("1000.50"), null})
	if got == nil || *got != "1000.50" {
		t.Errorf("single non-NULL volume_usd = %v, want verbatim 1000.50", got)
	}
	got = sumUSDVolume([]sql.NullString{str("1000.50"), str("2500.25")})
	if got == nil || *got != "3500.75" {
		t.Errorf("summed volume_usd = %v, want 3500.75", got)
	}
}

// TestBucketRowCap pins the row budget a both-directions read needs to
// still deliver `limit` COMPLETE buckets: a bucket holds at most two
// rows, so 2n+1 always contains the first n whole.
func TestBucketRowCap(t *testing.T) {
	for limit, want := range map[int]int{0: 0, -1: 0, 1: 3, 2: 5, 50_000: 100_001} {
		if got := bucketRowCap(limit); got != want {
			t.Errorf("bucketRowCap(%d) = %d, want %d", limit, got, want)
		}
	}
}

// -----------------------------------------------------------------
// End-to-end reads over a canned driver.
//
// These drive the real Store methods (query text, scan, fold) and assert
// the VALUE they serve, so a regression anywhere along the path — not
// just in combineDirVWAP — fails.
// -----------------------------------------------------------------

// TestHistoryPoints_CombinesBothStoredDirections is R-006: /v1/history
// (and the default /v1/chart, via HistoryPointsInRange) must read BOTH
// stored orientations and serve their volume-weighted union. Pre-fix this
// read filtered `base_asset = $1 AND quote_asset = $2` and served 0.5 —
// the requested-orientation rows alone, with the 50 XLM traded the other
// way round silently discarded.
func TestHistoryPoints_CombinesBothStoredDirections(t *testing.T) {
	pair := testXLMUSDCPair(t)
	bucket := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	conn := &cannedConn{plan: []cannedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "volume_usd"},
		rows: [][]driver.Value{
			{bucket, pair.Base.String(), "0.5", "100", "1000.50"},
			{bucket, pair.Quote.String(), "5", "10", "2500.25"},
		},
	}}}
	store := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}
	defer func() { _ = store.db.Close() }()

	pts, err := store.HistoryPoints(context.Background(), pair, Granularity1m, 10)
	if err != nil {
		t.Fatalf("HistoryPoints: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("HistoryPoints returned %d points, want 1 (both directions fold into ONE bucket)", len(pts))
	}
	if pts[0].VWAP != combineFixtureUnionVWAP {
		t.Errorf("served history VWAP = %s, want %s (union of both stored directions; "+
			"0.5 means the flipped orientation was dropped, 0.23 means it was trade-count-weighted)",
			pts[0].VWAP, combineFixtureUnionVWAP)
	}
	if pts[0].VolumeUSD == nil || *pts[0].VolumeUSD != "3500.75" {
		t.Errorf("served volume_usd = %v, want 3500.75 (both directions summed)", pts[0].VolumeUSD)
	}
	// The read must actually ASK for both orientations.
	q := conn.queries[0]
	if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") ||
		!strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
		t.Errorf("HistoryPoints query reads only one stored orientation:\n%s", q)
	}
}

// TestHistoryPoints_FlippedOnlyBucketIsServed is the other half of
// R-006: a bucket that traded ONLY in the reverse orientation used to
// vanish from /v1/history and /v1/chart entirely — the chart showed a
// gap where a real market had traded. It must now be served, at the
// exact inverse of the stored price.
func TestHistoryPoints_FlippedOnlyBucketIsServed(t *testing.T) {
	pair := testXLMUSDCPair(t)
	bucket := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	conn := &cannedConn{plan: []cannedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "volume_usd"},
		rows: [][]driver.Value{
			// Only the USDC/XLM orientation traded this minute.
			{bucket, pair.Quote.String(), "4", "10", "500.00"},
		},
	}}}
	store := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}
	defer func() { _ = store.db.Close() }()

	pts, err := store.HistoryPoints(context.Background(), pair, Granularity1m, 10)
	if err != nil {
		t.Fatalf("HistoryPoints: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("flipped-only bucket returned %d points, want 1 (it must not be dropped)", len(pts))
	}
	if pts[0].VWAP != "0.25" {
		t.Errorf("flipped-only VWAP = %s, want 0.25 (exact inverse of the stored 4)", pts[0].VWAP)
	}
	if pts[0].VolumeUSD == nil || *pts[0].VolumeUSD != "500.00" {
		t.Errorf("flipped-only volume_usd = %v, want 500.00", pts[0].VolumeUSD)
	}
}

// TestLatestClosedVWAP1mForPair_VolumeWeightedUnion is R-004: the value
// /v1/price serves. Pre-fix the SQL folded the bucket's two directions by
// trade count and served 0.23 for this bucket.
func TestLatestClosedVWAP1mForPair_VolumeWeightedUnion(t *testing.T) {
	pair := testXLMUSDCPair(t)
	bucket := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	conn := &cannedConn{plan: []cannedResult{
		// 1. the cheap recent-existence gate
		{cols: []string{"?column?"}, rows: [][]driver.Value{{int64(1)}}},
		// 2. the latest closed bucket's raw per-direction rows
		{
			cols: []string{"bucket", "base_asset", "vwap", "volume", "tc", "sources"},
			rows: [][]driver.Value{
				{bucket, pair.Base.String(), "0.5", "100", int64(1), []byte("{sdex}")},
				{bucket, pair.Quote.String(), "5", "10", int64(9), []byte("{soroswap,sdex}")},
			},
		},
	}}
	store := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}
	defer func() { _ = store.db.Close() }()

	row, err := store.LatestClosedVWAP1mForPair(context.Background(), pair)
	if err != nil {
		t.Fatalf("LatestClosedVWAP1mForPair: %v", err)
	}
	if row.VWAP != combineFixtureUnionVWAP {
		t.Errorf("served /v1/price VWAP = %s, want %s (volume-weighted union; "+
			"%s is the trade-count-weighted mean this replaced)",
			row.VWAP, combineFixtureUnionVWAP, combineFixtureCountVWAP)
	}
	if !row.Bucket.Equal(bucket) {
		t.Errorf("bucket = %v, want %v", row.Bucket, bucket)
	}
	// Metadata still aggregates across both directions.
	if row.TradeCount != 10 {
		t.Errorf("trade_count = %d, want 10 (1 + 9 across both directions)", row.TradeCount)
	}
	if strings.Join(row.Sources, ",") != "sdex,soroswap" {
		t.Errorf("sources = %v, want the de-duplicated sorted union [sdex soroswap]", row.Sources)
	}
	if row.BaseAsset != pair.Base.String() || row.QuoteAsset != pair.Quote.String() {
		t.Errorf("row orientation = %s/%s, want the REQUESTED %s", row.BaseAsset, row.QuoteAsset, pair)
	}
}

// TestClosedVWAPAtOrBefore_VolumeWeightedUnion is R-007 on the
// point-in-time engine behind /v1/price/at and /v1/price/changes.
func TestClosedVWAPAtOrBefore_VolumeWeightedUnion(t *testing.T) {
	pair := testXLMUSDCPair(t)
	ts := time.Now().UTC().Add(-30 * time.Minute)
	bucket := ts.Truncate(time.Minute).Add(-time.Minute)

	conn := &cannedConn{plan: []cannedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume"},
		rows: [][]driver.Value{
			{bucket, pair.Base.String(), "0.5", "100"},
			{bucket, pair.Quote.String(), "5", "10"},
		},
	}}}
	store := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}
	defer func() { _ = store.db.Close() }()

	got, err := store.ClosedVWAPAtOrBefore(context.Background(), pair, ts, time.Hour)
	if err != nil {
		t.Fatalf("ClosedVWAPAtOrBefore: %v", err)
	}
	if got.VWAP != combineFixtureUnionVWAP {
		t.Errorf("point-in-time VWAP = %s, want %s (volume-weighted union)",
			got.VWAP, combineFixtureUnionVWAP)
	}
	if got.Resolution != Granularity1m {
		t.Errorf("resolution = %s, want the finest rung 1m", got.Resolution)
	}
}

// TestRecentClosedVWAP1mCombined_PerBucketUnion is R-007 on the
// trailing baseline behind the /v1/price serving-sanity guard. Buckets
// must stay separate (one row per bucket, newest first) AND each must be
// the volume-weighted union — the guard compares this series against the
// served candidate, so a different combine here false-positives.
func TestRecentClosedVWAP1mCombined_PerBucketUnion(t *testing.T) {
	pair := testXLMUSDCPair(t)
	newer := time.Date(2026, 7, 23, 12, 1, 0, 0, time.UTC)
	older := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	conn := &cannedConn{plan: []cannedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "trade_count", "sources"},
		rows: [][]driver.Value{
			// newest bucket: two-sided → 0.4
			{newer, pair.Base.String(), "0.5", "100", int64(1), []byte("{sdex}")},
			{newer, pair.Quote.String(), "5", "10", int64(9), []byte("{sdex}")},
			// older bucket: flipped-only → exact inverse, 0.25
			{older, pair.Quote.String(), "4", "10", int64(3), []byte("{soroswap}")},
		},
	}}}
	store := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}
	defer func() { _ = store.db.Close() }()

	rows, err := store.RecentClosedVWAP1mCombined(context.Background(), pair, 5)
	if err != nil {
		t.Fatalf("RecentClosedVWAP1mCombined: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d trailing buckets, want 2 (rows of one bucket fold together)", len(rows))
	}
	if !rows[0].Bucket.Equal(newer) {
		t.Errorf("trailing[0] bucket = %v, want the newest %v", rows[0].Bucket, newer)
	}
	if rows[0].VWAP != combineFixtureUnionVWAP {
		t.Errorf("trailing[0] VWAP = %s, want %s (must match the served candidate's combine)",
			rows[0].VWAP, combineFixtureUnionVWAP)
	}
	if rows[1].VWAP != "0.25" {
		t.Errorf("trailing[1] VWAP = %s, want 0.25 (flipped-only bucket, exact inverse)", rows[1].VWAP)
	}
	if rows[0].TradeCount != 10 {
		t.Errorf("trailing[0] trade_count = %d, want 10", rows[0].TradeCount)
	}
}

// TestHistoryPoints_BucketLimitCountsBuckets — `limit` is a BUCKET limit,
// so a two-sided series must not return half as many points as asked for
// just because each bucket now costs two rows.
func TestHistoryPoints_BucketLimitCountsBuckets(t *testing.T) {
	pair := testXLMUSDCPair(t)
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	var rows [][]driver.Value
	for i := 0; i < 4; i++ {
		b := base.Add(time.Duration(i) * time.Minute)
		rows = append(rows,
			[]driver.Value{b, pair.Base.String(), "0.5", "100", nil},
			[]driver.Value{b, pair.Quote.String(), "5", "10", nil},
		)
	}
	conn := &cannedConn{plan: []cannedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "volume_usd"},
		rows: rows,
	}}}
	store := &Store{db: sql.OpenDB(&cannedConnector{conn: conn})}
	defer func() { _ = store.db.Close() }()

	pts, err := store.HistoryPoints(context.Background(), pair, Granularity1m, 3)
	if err != nil {
		t.Fatalf("HistoryPoints: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("limit=3 returned %d buckets, want 3", len(pts))
	}
	for i, p := range pts {
		if p.VWAP != combineFixtureUnionVWAP {
			t.Errorf("bucket %d VWAP = %s, want %s", i, p.VWAP, combineFixtureUnionVWAP)
		}
		if p.VolumeUSD != nil {
			t.Errorf("bucket %d volume_usd = %q, want nil (both directions NULL)", i, *p.VolumeUSD)
		}
	}
}

// testXLMUSDCPair builds the XLM/USDC pair the fixtures use.
func testXLMUSDCPair(t *testing.T) canonical.Pair {
	t.Helper()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset USDC: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

// -----------------------------------------------------------------
// Canned driver — replays a fixed result per query, in order, and
// records the SQL it was asked to run. Enough of database/sql's driver
// surface to exercise a Store read end-to-end without a database (the
// same "money math must be testable without a DB" discipline as
// bridgeRate / xlmLegRate in usd_fx_resolver.go).
// -----------------------------------------------------------------

type cannedResult struct {
	cols []string
	rows [][]driver.Value
}

type cannedRows struct {
	res cannedResult
	i   int
}

func (r *cannedRows) Columns() []string { return r.res.cols }
func (r *cannedRows) Close() error      { return nil }
func (r *cannedRows) Next(dest []driver.Value) error {
	if r.i >= len(r.res.rows) {
		return io.EOF
	}
	copy(dest, r.res.rows[r.i])
	r.i++
	return nil
}

type cannedConn struct {
	plan    []cannedResult
	n       int
	queries []string
}

func (c *cannedConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (c *cannedConn) Close() error                        { return nil }
func (c *cannedConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }

func (c *cannedConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.queries = append(c.queries, q)
	if c.n >= len(c.plan) {
		return nil, errors.New("cannedConn: unexpected extra query: " + q)
	}
	res := c.plan[c.n]
	c.n++
	return &cannedRows{res: res}, nil
}

type cannedDriver struct{}

func (cannedDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not implemented") }

type cannedConnector struct{ conn *cannedConn }

func (c *cannedConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *cannedConnector) Driver() driver.Driver                        { return cannedDriver{} }
