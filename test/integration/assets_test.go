//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestClickHouseCohortFlowsCarryTheMonthsOwnPrice runs one whole cohort
// cycle against live ClickHouse — the month-price loader, every fold,
// and the EXCHANGE that now swaps stellar.asset_month_usd_prices live
// beside the cohort tables — then reads the cohort back through the
// repo's own reader and pins the read-time join: a flow row carries
// the served tier's price for ITS (asset, month) and nothing else. USDC
// is priced for May and XLM for May only; the cohort moved USDC in May
// and XLM in June, so the May USDC row must read "0.998" and the June
// XLM row must be unpriced although XLM has a row in the table — a join
// on asset alone, or on month alone, fails this.
func TestClickHouseCohortFlowsCarryTheMonthsOwnPrice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		root   = "GTEST_COHORT_THEN_SPONSOR_AAAAAAAAAAAAAAAAAAAAAAAAAA"
		member = "GTEST_COHORT_THEN_MEMBER_AAAAAAAAAAAAAAAAAAAAAAAAAAA"
		usdc   = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		mayLdg = uint32(77_700_001)
		junLdg = uint32(77_700_002)
	)
	may := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
	mayMonth := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// A lake tip the cycle can walk to, in a partition of its own.
	lb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)`)
	if err != nil {
		t.Fatalf("prepare ledgers: %v", err)
	}
	for seq, at := range map[uint32]time.Time{mayLdg: may, junLdg: jun} {
		if err := lb.Append(seq, at, fmt.Sprintf("%064d", seq), "00", uint32(23)); err != nil {
			t.Fatalf("append ledger: %v", err)
		}
	}
	if err := lb.Send(); err != nil {
		t.Fatalf("send ledgers: %v", err)
	}

	// One sponsored member — every sponsor is covered, no floor.
	if err := raw.Exec(ctx, `INSERT INTO stellar.account_sponsor_edges
		(sponsor, sponsored, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
		VALUES (?, ?, 1, ?, ?, ?, ?)`, root, member, mayLdg, mayLdg, may, may); err != nil {
		t.Fatalf("insert sponsor edge: %v", err)
	}

	// The member's movements: 100 USDC in and 25 out in May, 3 XLM in
	// in June.
	mb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.account_movements
		(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction, movement_kind, provenance, asset, counterparty, amount)`)
	if err != nil {
		t.Fatalf("prepare movements: %v", err)
	}
	for i, m := range []struct {
		ledger uint32
		at     time.Time
		dir    chstore.AccountMovementDirection
		asset  string
		amount int64
	}{
		{mayLdg, may, chstore.AccountMovementReceived, usdc, 1_000_000_000},
		{mayLdg, may, chstore.AccountMovementSent, usdc, 250_000_000},
		{junLdg, jun, chstore.AccountMovementReceived, "native", 30_000_000},
	} {
		if err := mb.Append(member, m.ledger, m.at, fmt.Sprintf("%064d", m.ledger), uint32(0), uint32(i),
			string(m.dir), "payment", "classic", m.asset, root, big.NewInt(m.amount)); err != nil {
			t.Fatalf("append movement: %v", err)
		}
	}
	if err := mb.Send(); err != nil {
		t.Fatalf("send movements: %v", err)
	}

	prices := []timescale.MonthlyUSDVWAP{
		{Asset: usdc, Month: mayMonth, VWAPUSD: "0.998", VolumeUSD: 12345.5},
		{Asset: "native", Month: mayMonth, VWAPUSD: "0.1", VolumeUSD: 99.25},
	}
	if err := chstore.RunCohortRollup(ctx, addr, nil, prices, t.Logf); err != nil {
		t.Fatalf("RunCohortRollup: %v", err)
	}

	// The loader's rows were staged and the swap served them.
	var served uint64
	if err := raw.QueryRow(ctx, `SELECT count() FROM stellar.asset_month_usd_prices`).Scan(&served); err != nil {
		t.Fatalf("count served month prices: %v", err)
	}
	if served != uint64(len(prices)) {
		t.Fatalf("stellar.asset_month_usd_prices serves %d rows after the cycle, want %d", served, len(prices))
	}
	var servedPrice string
	var servedVolume float64
	if err := raw.QueryRow(ctx, `SELECT vwap_usd, volume_usd FROM stellar.asset_month_usd_prices WHERE asset = ? AND month = ?`,
		usdc, mayMonth).Scan(&servedPrice, &servedVolume); err != nil {
		t.Fatalf("read served USDC May price: %v", err)
	}
	if servedPrice != "0.998" || servedVolume != 12345.5 {
		t.Errorf("served USDC May = (%q, %v), want (\"0.998\", 12345.5)", servedPrice, servedVolume)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	cohort, ok, err := er.AccountCohort(ctx, root, chstore.CohortRelationSponsored)
	if err != nil {
		t.Fatalf("AccountCohort: %v", err)
	}
	if !ok || !cohort.Covered {
		t.Fatalf("cohort ok=%v covered=%v, want a covered sponsor cohort", ok, cohort.Covered)
	}

	type key struct {
		month string
		asset string
	}
	got := map[key]*string{}
	for _, f := range cohort.Flows {
		got[key{f.Month.UTC().Format("2006-01"), f.Asset}] = f.PriceUSDThen
	}
	show := func(p *string) string {
		if p == nil {
			return "<absent>"
		}
		return *p
	}
	for _, tc := range []struct {
		k    key
		want string // "" = absent
	}{
		{key{"2026-05", usdc}, "0.998"},
		{key{"2026-05", chstore.CohortAllAssets}, ""},
		{key{"2026-06", "native"}, ""}, // XLM is priced for May, not June: the join is on (asset, month)
		{key{"2026-06", chstore.CohortAllAssets}, ""},
	} {
		p, present := got[tc.k]
		if !present {
			t.Errorf("no flow row for %s %s (flows: %+v)", tc.k.month, tc.k.asset, cohort.Flows)
			continue
		}
		if show(p) != tc.want && !(tc.want == "" && p == nil) {
			t.Errorf("%s %s: price_usd_then = %s, want %q", tc.k.month, tc.k.asset, show(p), tc.want)
		}
	}
	if len(got) != 4 {
		t.Errorf("flow rows = %d, want 4 (two months × asset + all-assets): %+v", len(got), cohort.Flows)
	}
	if f := got[key{"2026-05", usdc}]; f != nil {
		// The amount the price applies to is the fold's own.
		for _, fl := range cohort.Flows {
			if fl.Asset == usdc && fl.Inflow.Int64() != 1_000_000_000 {
				t.Errorf("USDC May inflow = %s, want 1000000000", fl.Inflow)
			}
		}
	}
}

const (
	cohortPosRoot  = "GTEST_COHORT_POSITIONS_SPONSOR_AAAAAAAAAAAAAAAAAAAAAA"
	cohortPosOne   = "GTEST_COHORT_POSITIONS_MEMBER_ONE_AAAAAAAAAAAAAAAAAAA"
	cohortPosTwo   = "GTEST_COHORT_POSITIONS_MEMBER_TWO_AAAAAAAAAAAAAAAAAAA"
	cohortPosVenue = "CTEST_COHORT_POSITIONS_POOL_2POW53"
	cohortPosVault = "CTEST_COHORT_POSITIONS_VAULT_18DEC"
)

func seedCohortPositionEdges(t *testing.T, ctx context.Context) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	// account_sponsor_edges is a plain MergeTree: a second seed would duplicate
	// the edges and double every membership sum, whichever test runs second.
	var seeded uint64
	if err := raw.QueryRow(ctx, `SELECT count() FROM stellar.account_sponsor_edges WHERE sponsor = ?`,
		cohortPosRoot).Scan(&seeded); err != nil {
		t.Fatalf("count seeded edges: %v", err)
	}
	if seeded > 0 {
		return
	}
	at := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	// A lake tip for the cycle to walk to; the cycle refuses a tip of 0.
	if err := raw.Exec(ctx, `INSERT INTO stellar.ledgers
		(ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)
		VALUES (77900001, ?, ?, '00', 23)`, at, fmt.Sprintf("%064d", 77900001)); err != nil {
		t.Fatalf("insert ledger: %v", err)
	}
	for _, m := range []string{cohortPosOne, cohortPosTwo} {
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_sponsor_edges
			(sponsor, sponsored, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
			VALUES (?, ?, 1, 77900001, 77900001, ?, ?)`, cohortPosRoot, m, at, at); err != nil {
			t.Fatalf("insert sponsor edge: %v", err)
		}
	}
}

func cohortPositionHolder(user, venue, amount string) timescale.DeFiPositionHolder {
	return timescale.DeFiPositionHolder{
		Protocol: "blend", PositionKind: "lending_supply", Venue: venue, Asset: "",
		User: user, Amount: amount, LastLedger: 77_900_001,
	}
}

// TestClickHouseCohortPositionsSumExactly runs a cohort cycle over DeFi
// position amounts a float64 cannot hold and requires the exact integer
// sum back, both from the table and through the reader. 2^53+1 plus 1 is
// 2^53+2, which a Float64 sum rounds to 2^53; an 18-decimal total loses
// its low digits.
func TestClickHouseCohortPositionsSumExactly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	seedCohortPositionEdges(t, ctx)

	holders := []timescale.DeFiPositionHolder{
		cohortPositionHolder(cohortPosOne, cohortPosVenue, "9007199254740993"),
		cohortPositionHolder(cohortPosTwo, cohortPosVenue, "1"),
		cohortPositionHolder(cohortPosOne, cohortPosVault, "8760000000000000000000000001"),
		cohortPositionHolder(cohortPosTwo, cohortPosVault, "-2"),
	}
	want := map[string]string{
		cohortPosVenue: "9007199254740994",
		cohortPosVault: "8759999999999999999999999999",
	}
	if err := chstore.RunCohortRollup(ctx, addr, holders, nil, t.Logf); err != nil {
		t.Fatalf("RunCohortRollup: %v", err)
	}

	raw := dialClickHouse(t, ctx, "stellar")
	rows, err := raw.Query(ctx, `SELECT venue, toString(amount) FROM stellar.account_cohort_positions
		WHERE rel = ? AND root = ?`, chstore.CohortRelationSponsored, cohortPosRoot)
	if err != nil {
		t.Fatalf("read positions: %v", err)
	}
	stored := map[string]string{}
	for rows.Next() {
		var venue, amount string
		if err := rows.Scan(&venue, &amount); err != nil {
			t.Fatalf("scan position: %v", err)
		}
		stored[venue] = amount
	}
	_ = rows.Close()
	for venue, w := range want {
		if stored[venue] != w {
			t.Errorf("stored amount for %s = %q, want %q (summed through a float)", venue, stored[venue], w)
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	cohort, ok, err := er.AccountCohort(ctx, cohortPosRoot, chstore.CohortRelationSponsored)
	if err != nil || !ok {
		t.Fatalf("AccountCohort ok=%v err=%v", ok, err)
	}
	seen := 0
	for _, p := range cohort.Positions {
		if w, ok := want[p.Venue]; ok {
			seen++
			if got := fmt.Sprint(p.Amount); got != w {
				t.Errorf("read amount for %s = %s, want %s", p.Venue, got, w)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("reader returned %d of %d seeded positions: %+v", seen, len(want), cohort.Positions)
	}
}

// TestClickHouseCohortPositionsRefuseANonIntegerAmount pins that an amount
// the exact sum cannot take fails the cycle loudly — never read as zero,
// never rounded into a total that is served as exact.
func TestClickHouseCohortPositionsRefuseANonIntegerAmount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	seedCohortPositionEdges(t, ctx)

	for _, bad := range []string{"1.5", "", "1e18"} {
		holders := []timescale.DeFiPositionHolder{
			cohortPositionHolder(cohortPosOne, cohortPosVenue, "7"),
			cohortPositionHolder(cohortPosTwo, cohortPosVenue, bad),
		}
		err := chstore.RunCohortRollup(ctx, addr, holders, nil, t.Logf)
		if !errors.Is(err, canonical.ErrInvalidAmount) {
			t.Errorf("amount %q: RunCohortRollup err = %v, want canonical.ErrInvalidAmount", bad, err)
		}
	}
}

var cohortPosStellarRef = regexp.MustCompile(`\bstellar\.`)

// TestClickHouseCohortPositionsRetypeMigratesAFloatTable applies the
// operator file's ALTERs to a pre-retype (Float64) pair of tables and
// requires both halves of the EXCHANGE pair to come out Int256: a staging
// twin left Float64 would round every later cycle's exact sum on insert.
func TestClickHouseCohortPositionsRetypeMigratesAFloatTable(t *testing.T) {
	ctx := context.Background()
	const db = "stellar_cohort_retype"
	conn := dialClickHouse(t, ctx, "default")
	exec := func(q string) {
		t.Helper()
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%.96q: %v", q, err)
		}
	}
	exec("DROP DATABASE IF EXISTS " + db + " SYNC")
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" SYNC") })
	exec("CREATE DATABASE " + db)
	exec(`CREATE TABLE ` + db + `.account_cohort_positions
		(rel LowCardinality(String), root String, protocol LowCardinality(String),
		 position_kind LowCardinality(String), venue String, asset String,
		 holders UInt64, amount Float64)
		ENGINE = MergeTree ORDER BY (rel, root, protocol, venue, asset, position_kind)`)
	exec(`CREATE TABLE ` + db + `.account_cohort_positions_staging AS ` + db + `.account_cohort_positions`)
	exec(`INSERT INTO ` + db + `.account_cohort_positions VALUES ('sponsored','G','blend','lending_supply','C','',2,1234)`)

	stmts, err := clickHouseDeployStatements("account_cohort_rollup.sql")
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, s := range stmts {
		if strings.HasPrefix(strings.ToUpper(s), "ALTER TABLE STELLAR.ACCOUNT_COHORT_POSITIONS") {
			exec(cohortPosStellarRef.ReplaceAllString(s, db+"."))
			applied++
		}
	}
	t.Logf("applied %d account_cohort_positions ALTERs from account_cohort_rollup.sql", applied)

	for _, table := range []string{"account_cohort_positions", "account_cohort_positions_staging"} {
		var typ string
		if err := conn.QueryRow(ctx, `SELECT type FROM system.columns
			WHERE database = ? AND table = ? AND name = 'amount'`, db, table).Scan(&typ); err != nil {
			t.Fatalf("%s: read column type: %v", table, err)
		}
		if typ != "Int256" {
			t.Errorf("%s.amount is %s after the operator file's ALTERs, want Int256", table, typ)
		}
	}
	var amount string
	if err := conn.QueryRow(ctx, `SELECT toString(amount) FROM `+db+`.account_cohort_positions`).Scan(&amount); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if amount != "1234" {
		t.Errorf("migrated amount = %q, want 1234", amount)
	}
}

// TestIssuersAndAssetsRegistryReads covers the read paths backing
// /v1/issuers (list + detail), /v1/coins (with and without the
// ?issuer= filter), and the issuer→assets join used by the
// showcase /coins/[slug] issuer tab.
//
// The classic_assets + issuers tables are populated by ingest-side
// observers in production. This test inserts directly so we can
// exercise the storage queries against a known shape without
// stitching together an end-to-end ingest harness.
func TestIssuersAndAssetsRegistryReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Three issuers — one with home_domain set, two without.
	// Observation counts vary so we can test the ranking.
	const (
		issuerA = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" // USDC-shape
		issuerB = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		issuerC = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: issuerA, homeDomain: "centre.io"},
		{g: issuerB, homeDomain: ""}, // null
		{g: issuerC, homeDomain: "example.org"},
	})
	seedClassicAssets(t, ctx, store, []seedAsset{
		{assetID: "USDC-" + issuerA, code: "USDC", issuer: issuerA, slug: "USDC", obs: 41_000_000},
		{assetID: "AQUA-" + issuerB, code: "AQUA", issuer: issuerB, slug: "AQUA", obs: 14_000_000},
		// Two assets for issuerC — verifies the JOIN aggregates
		// observation_count across multiple assets per issuer.
		{assetID: "FOO-" + issuerC, code: "FOO", issuer: issuerC, slug: "FOO", obs: 5_000_000},
		{assetID: "BAR-" + issuerC, code: "BAR", issuer: issuerC, slug: "BAR", obs: 2_000_000},
	})

	t.Run("ListIssuers", func(t *testing.T) {
		got, err := store.ListIssuers(ctx, 10)
		if err != nil {
			t.Fatalf("ListIssuers: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d issuers, want 3", len(got))
		}
		// Ranking: A (41M) > B (14M) > C (7M total).
		if got[0].GStrkey != issuerA {
			t.Errorf("rank 1 = %s, want %s", got[0].GStrkey, issuerA)
		}
		if got[1].GStrkey != issuerB {
			t.Errorf("rank 2 = %s, want %s", got[1].GStrkey, issuerB)
		}
		if got[2].GStrkey != issuerC {
			t.Errorf("rank 3 = %s, want %s", got[2].GStrkey, issuerC)
		}
		// home_domain flows through.
		if got[0].HomeDomain != "centre.io" {
			t.Errorf("home_domain = %q, want %q", got[0].HomeDomain, "centre.io")
		}
		// NULL home_domain comes back as empty string.
		if got[1].HomeDomain != "" {
			t.Errorf("null home_domain = %q, want empty", got[1].HomeDomain)
		}
		// Per-issuer asset count + total observations aggregate
		// correctly across rows.
		if got[2].AssetCount != 2 {
			t.Errorf("issuerC asset_count = %d, want 2", got[2].AssetCount)
		}
		if got[2].TotalObservationCount != 7_000_000 {
			t.Errorf("issuerC total_obs = %d, want 7_000_000", got[2].TotalObservationCount)
		}
	})

	t.Run("ListIssuers limit clamp", func(t *testing.T) {
		// Out-of-range limit clamps to default 100, not to itself.
		got, err := store.ListIssuers(ctx, 0)
		if err != nil {
			t.Fatalf("ListIssuers(0): %v", err)
		}
		if len(got) != 3 {
			t.Errorf("limit=0 returned %d rows, want 3 (default 100 clamp)", len(got))
		}
		gotHigh, err := store.ListIssuers(ctx, 99999)
		if err != nil {
			t.Fatalf("ListIssuers(99999): %v", err)
		}
		if len(gotHigh) != 3 {
			t.Errorf("limit=99999 returned %d rows, want 3 (table only has 3)", len(gotHigh))
		}
	})

	t.Run("ListAssets no filter", func(t *testing.T) {
		got, err := store.ListAssets(ctx, 10, "", "")
		if err != nil {
			t.Fatalf("ListAssets: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("got %d coins, want 4", len(got))
		}
		// Ordered by observation_count desc — USDC at top.
		if got[0].Code != "USDC" {
			t.Errorf("rank 1 = %s, want USDC", got[0].Code)
		}
	})

	t.Run("ListAssets issuer filter", func(t *testing.T) {
		got, err := store.ListAssets(ctx, 10, issuerC, "")
		if err != nil {
			t.Fatalf("ListAssets by issuer: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("issuerC filter returned %d rows, want 2", len(got))
		}
		// Both rows should be issuerC.
		for _, r := range got {
			if r.IssuerGStrkey != issuerC {
				t.Errorf("row %s leaked through filter — issuer %s, want %s",
					r.Code, r.IssuerGStrkey, issuerC)
			}
		}
		// Top-of-list within the filter is FOO (5M > 2M).
		if got[0].Code != "FOO" {
			t.Errorf("filter rank 1 = %s, want FOO", got[0].Code)
		}
	})

	t.Run("ListAssets issuer filter — no match", func(t *testing.T) {
		got, err := store.ListAssets(ctx, 10, "GUNKNOWN0000000000000000000000000000000000000000000000XX", "")
		if err != nil {
			t.Fatalf("ListAssets unknown issuer: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("unknown issuer returned %d rows, want 0", len(got))
		}
	})

	t.Run("GetIssuer + ListIssuerAssets", func(t *testing.T) {
		row, err := store.GetIssuer(ctx, issuerA)
		if err != nil {
			t.Fatalf("GetIssuer: %v", err)
		}
		if row.GStrkey != issuerA {
			t.Errorf("g_strkey = %s, want %s", row.GStrkey, issuerA)
		}
		if row.HomeDomain != "centre.io" {
			t.Errorf("home_domain = %q, want centre.io", row.HomeDomain)
		}

		assets, err := store.ListIssuerAssets(ctx, issuerC)
		if err != nil {
			t.Fatalf("ListIssuerAssets: %v", err)
		}
		if len(assets) != 2 {
			t.Fatalf("issuerC assets = %d, want 2", len(assets))
		}
		// Assets returned ordered by observation_count desc.
		if assets[0].Code != "FOO" {
			t.Errorf("first asset = %s, want FOO", assets[0].Code)
		}
	})

	t.Run("GetIssuer not found", func(t *testing.T) {
		_, err := store.GetIssuer(ctx, "GUNKNOWN0000000000000000000000000000000000000000000000XX")
		if err == nil {
			t.Fatal("expected error for unknown issuer, got nil")
		}
		// Caller in handleIssuer relies on errors.Is(err, sql.ErrNoRows).
		if err != sql.ErrNoRows {
			t.Errorf("error = %v, want sql.ErrNoRows", err)
		}
	})
}

type seedIssuer struct {
	g          string
	homeDomain string
}

type seedAsset struct {
	assetID string
	code    string
	issuer  string
	slug    string
	obs     int64
}

// seedIssuers + seedClassicAssets insert directly into the registry
// tables. In production these are populated by the accounts decoder
// + classic_assets observer; bypassing them here keeps the test
// focused on the read-path behaviour.
func seedIssuers(t *testing.T, ctx context.Context, store *timescale.Store, rows []seedIssuer) {
	t.Helper()
	for _, r := range rows {
		var hd any
		if r.homeDomain != "" {
			hd = r.homeDomain
		}
		_, err := store.DB().ExecContext(ctx,
			`INSERT INTO issuers (g_strkey, home_domain) VALUES ($1, $2)`,
			r.g, hd,
		)
		if err != nil {
			t.Fatalf("seed issuer %s: %v", r.g, err)
		}
	}
}

func seedClassicAssets(t *testing.T, ctx context.Context, store *timescale.Store, rows []seedAsset) {
	t.Helper()
	now := time.Now().UTC()
	for _, r := range rows {
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO classic_assets
			    (asset_id, code, issuer_g_strkey, slug,
			     first_seen_at, first_seen_ledger,
			     last_seen_at,  last_seen_ledger,
			     observation_count)
			VALUES ($1, $2, $3, $4, $5, 1, $6, 100, $7)
		`,
			r.assetID, r.code, r.issuer, r.slug,
			now.Add(-24*time.Hour), now, r.obs,
		)
		if err != nil {
			t.Fatalf("seed asset %s: %v", r.assetID, err)
		}
	}
}

// TestCatalogCorrectionsAndScaffoldDrop pins what migrations 0151 and 0152
// actually put in (and take out of) the DATABASE — which is the only place
// this class of defect lives.
//
// Why it needs a real database. A `COMMENT ON` string is not repo state: it
// is a row in pg_description written once, by whichever migration issued it.
// Editing the original migration's text therefore changes what a FRESH
// database gets and changes NOTHING about an applied one, so a file-level
// assertion would pass while every deployed environment still served the
// wrong string through `\d+`. The same is true in reverse for 0152: the
// defect was six tables that EXIST in the catalog with no writer, and only
// information_schema can answer whether they still do.
//
// Every assertion below is on the CORRECTED VALUE, not on
// defined/non-empty:
//
//   - trades.usd_volume must no longer claim the aggregator fills it
//     post-insert (it is valued at INSERT; NULL means "no route" and never
//     becomes a value on its own — a consumer that polls waits forever).
//   - aquarius_rewards_events must name twelve kinds, matching its own
//     event_kind CHECK, and must name config_rewards — the one a stale
//     "11 kinds" list would omit.
//   - oracle_updates.contract_id must not name coinmarketcap or the retired
//     chainlink-http spelling.
//   - aquarius_protocol_fee.recipient must point at the `token` column
//     (migration 0139) instead of telling the reader to join a trade.
//   - soroban_events.topics_xdr must not offer the "ClickHouse-lake
//     re-project" recovery, which does not exist for that column.
//   - customer_webhooks.secret_hash must carry the MISNAMED warning: the
//     column holds the raw HMAC signing key, not a hash of one.
//   - the six unwired scaffold tables must be GONE, as must the four
//     orphaned Stripe columns and the two partial indexes over them.
//
// Runs under `-tags=integration` only, against the TimescaleDB version r1
// runs.
func TestCatalogCorrectionsAndScaffoldDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// ─── 0151: stored comments say the true thing ───────────────

	colComment := func(table, column string) string {
		t.Helper()
		var c sql.NullString
		err := db.QueryRowContext(ctx, `
			SELECT col_description(c.oid, a.attnum)
			  FROM pg_class c
			  JOIN pg_attribute a ON a.attrelid = c.oid
			 WHERE c.relname = $1 AND a.attname = $2`, table, column).Scan(&c)
		if err != nil {
			t.Fatalf("read comment on %s.%s: %v", table, column, err)
		}
		if !c.Valid {
			// Errorf, not Fatalf: one missing comment must not hide the
			// state of the other eleven. A test that stops at the first
			// failure reports one regression when there are several.
			t.Errorf("%s.%s has NO catalog comment — migration 0151 should have set one", table, column)
			return ""
		}
		return c.String
	}

	tableComment := func(table string) string {
		t.Helper()
		var c sql.NullString
		err := db.QueryRowContext(ctx,
			`SELECT obj_description($1::regclass, 'pg_class')`, table).Scan(&c)
		if err != nil {
			t.Fatalf("read comment on table %s: %v", table, err)
		}
		if !c.Valid {
			t.Errorf("table %s has NO catalog comment — migration 0151 should have set one", table)
			return ""
		}
		return c.String
	}

	type commentCase struct {
		what     string
		got      string
		mustHave []string
		mustNot  []string
		why      string
	}

	cases := []commentCase{
		{
			what:     "trades.usd_volume",
			got:      colComment("trades", "usd_volume"),
			mustHave: []string{"Valued AT INSERT", "no route"},
			mustNot:  []string{"Derived by the aggregator post-insert"},
			why: "no aggregator pass has ever valued this column; NULL means the tiered " +
				"USD router found no route, so a consumer waiting for the value to appear waits forever",
		},
		{
			what:     "oracle_updates.contract_id",
			got:      colComment("oracle_updates", "contract_id"),
			mustHave: []string{"chainlink", "coingecko", "ecb"},
			mustNot:  []string{"coinmarketcap", "chainlink-http"},
			why: "measured on r1 the off-chain (NULL contract_id) sources are chainlink, " +
				"coingecko and ecb; no coinmarketcap ingest writes here and `chainlink-http` is retired",
		},
		{
			what:     "aquarius_protocol_fee.recipient",
			got:      colComment("aquarius_protocol_fee", "recipient"),
			mustHave: []string{"`token` column", "0139"},
			mustNot:  []string{"token is positional"},
			why: "0139 added the token column from topic[1]; joining a trade instead " +
				"mis-attributes an op that sweeps two tokens to the same recipient",
		},
		{
			what:     "soroban_events.topics_xdr",
			got:      colComment("soroban_events", "topics_xdr"),
			mustHave: []string{"NOT repaired by projector-replay"},
			mustNot:  []string{"Recover pre-0114 rows via a ClickHouse-lake re-project"},
			why: "the projector reads this table and writes the per-source tables; " +
				"nothing writes topics_xdr back here, so the recovery it named cannot restore the column",
		},
		{
			what:     "customer_webhooks.secret_hash",
			got:      colComment("customer_webhooks", "secret_hash"),
			mustHave: []string{"MISNAMED", "RAW HMAC-SHA-256"},
			why: "the column stores the shared signing key, not a hash of it — `\\d+` is " +
				"where an operator forms the belief that it is already hashed",
		},
		{
			what:     "aquarius_rewards_events",
			got:      tableComment("aquarius_rewards_events"),
			mustHave: []string{"12 kinds", "config_rewards"},
			mustNot:  []string{"11 kinds"},
			why:      "the event_kind CHECK admits twelve values; the old list named eleven",
		},
		{
			what:     "decoder_stats_5m",
			got:      tableComment("decoder_stats_5m"),
			mustHave: []string{"statsflush", "WRITE-ONLY"},
			mustNot:  []string{"so /v1/diagnostics/decoders can query"},
			why: "the writer is the indexer's statsflush worker, not the aggregator, and " +
				"the /v1/diagnostics/decoders route it cited is not in the OpenAPI spec",
		},
		{
			what:    "completeness_snapshots",
			got:     tableComment("completeness_snapshots"),
			mustNot: []string{"notes/DECISION-genesis-complete-verdict"},
			why:     "notes/ is gitignored, so that pointer resolves to nothing for any reader",
		},
		{
			what:    "change_summary_5m",
			got:     tableComment("change_summary_5m"),
			mustNot: []string{"showcase-site-data-inventory.md"},
			why:     "the doc was renamed to explorer-data-inventory.md",
		},
		{
			what:    "soroswap_skim_events",
			got:     tableComment("soroswap_skim_events"),
			mustNot: []string{"docs/discovery/dexes-amms"},
			why:     "the docs/discovery/ tree was removed from the repo",
		},
	}

	for _, tc := range cases {
		if tc.got == "" {
			continue // already reported as missing by the reader above
		}
		for _, want := range tc.mustHave {
			if !strings.Contains(tc.got, want) {
				t.Errorf("catalog comment on %s does not contain %q (%s).\n  got: %s",
					tc.what, want, tc.why, tc.got)
			}
		}
		for _, bad := range tc.mustNot {
			if strings.Contains(tc.got, bad) {
				t.Errorf("catalog comment on %s STILL contains %q (%s).\n  got: %s",
					tc.what, bad, tc.why, tc.got)
			}
		}
	}

	// The corrected aquarius_rewards_events comment must enumerate exactly
	// the values its own CHECK admits — the "12 kinds" claim is only worth
	// anything if the list behind it is the real set.
	assertRewardsKindsMatchCheck(t, db, ctx, tableComment("aquarius_rewards_events"))

	// ─── 0152: the unwired scaffolds are gone ───────────────────

	for _, gone := range []string{
		"wasm_versions", "contract_wasm_history", "tvl_observations",
		"anchors", "classic_asset_stats_5m", "aggregator_exposures",
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name = $1`, gone).Scan(&n); err != nil {
			t.Fatalf("look up table %s: %v", gone, err)
		}
		if n != 0 {
			t.Errorf("table %q still exists after migration 0152. It has no Go writer or "+
				"reader in any released binary, so a `\\dt` listing it is a claim that a "+
				"capability exists when the data is merely absent (#358).", gone)
		}
	}

	for _, col := range []string{
		"dead_lettered_at", "dead_letter_reason", "dead_letter_resolved_at", "claimed_at",
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'stripe_event_log' AND column_name = $1`, col).Scan(&n); err != nil {
			t.Fatalf("look up stripe_event_log.%s: %v", col, err)
		}
		if n != 0 {
			t.Errorf("stripe_event_log.%s still exists after migration 0152 — its writer was "+
				"deleted in d2185560, so the column describes a reconciliation protocol no "+
				"code implements (#357 F8).", col)
		}
	}

	for _, idx := range []string{
		"stripe_event_log_open_dead_letters_idx", "stripe_event_log_claimed_idx",
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_indexes WHERE indexname = $1`, idx).Scan(&n); err != nil {
			t.Fatalf("look up index %s: %v", idx, err)
		}
		if n != 0 {
			t.Errorf("index %q still exists after migration 0152 (its columns are gone)", idx)
		}
	}

	// stripe_event_log itself SURVIVES — 0027 creates it and 0152 must not
	// break 0027's rollback symmetry — but it is tombstoned so the next
	// reader does not go looking for a worker that was deleted.
	assertTableExists(t, db, ctx, "stripe_event_log")
	if c := tableComment("stripe_event_log"); !strings.Contains(c, "INERT") ||
		!strings.Contains(c, "d2185560") {
		t.Errorf("stripe_event_log's tombstone comment does not record that its writers "+
			"were deleted in d2185560.\n  got: %s", c)
	}
}

// assertRewardsKindsMatchCheck proves the corrected "12 kinds" comment lists
// exactly the values the event_kind CHECK constraint admits — so a future
// widening of the CHECK that forgets the comment fails here rather than
// re-creating catalogue drift.
func assertRewardsKindsMatchCheck(t *testing.T, db *sql.DB, ctx context.Context, comment string) {
	t.Helper()

	var def string
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(con.oid)
		  FROM pg_constraint con
		  JOIN pg_class c ON c.oid = con.conrelid
		 WHERE c.relname = 'aquarius_rewards_events'
		   AND con.contype = 'c'
		   AND pg_get_constraintdef(con.oid) LIKE '%event_kind%'`).Scan(&def)
	if err != nil {
		t.Fatalf("read aquarius_rewards_events event_kind CHECK: %v", err)
	}

	if comment == "" {
		return // the missing comment is already an error; nothing to compare
	}

	kinds := extractQuotedLiterals(def)
	if len(kinds) == 0 {
		t.Fatalf("parsed 0 kinds out of the event_kind CHECK %q — the parse has gone "+
			"vacuous; fix it, do not delete the assertion", def)
	}
	t.Logf("event_kind CHECK admits %d kinds", len(kinds))

	for _, k := range kinds {
		if !strings.Contains(comment, k) {
			t.Errorf("aquarius_rewards_events' catalog comment omits event kind %q, which "+
				"its own CHECK admits. The comment is what an operator reads through "+
				"`\\d+`; a kind missing from it is a kind they will not know exists (#357 F4).\n"+
				"  comment: %s", k, comment)
		}
	}
}

// extractQuotedLiterals pulls the 'single-quoted' literals out of a
// constraint definition, de-duplicated, preserving order.
func extractQuotedLiterals(s string) []string {
	var out []string
	seen := map[string]bool{}
	for {
		i := strings.Index(s, "'")
		if i < 0 {
			return out
		}
		s = s[i+1:]
		j := strings.Index(s, "'")
		if j < 0 {
			return out
		}
		lit := s[:j]
		s = s[j+1:]
		if lit != "" && !seen[lit] {
			seen[lit] = true
			out = append(out, lit)
		}
	}
}
