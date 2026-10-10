//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestClickHouseCohortHoldingsSumPastInt64 runs a whole cohort cycle over
// two members each holding math.MaxInt64 of one trustline asset and reads
// the holding back through the repo's reader. ledger_entries_current.balance
// is Int64, and ClickHouse's sum() over Int64 returns Int64 and wraps, so a
// holdings step that widens the sum's result instead of its argument serves
// -2 here instead of 2*(2^63-1).
func TestClickHouseCohortHoldingsSumPastInt64(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		root   = "GTEST_COHORT_INT128_SPONSOR_AAAAAAAAAAAAAAAAAAAAAAAAA"
		asset  = "BIGSUP-GTEST_COHORT_INT128_ISSUER_AAAAAAAAAAAAAAAAAAAAAAA"
		ledger = uint32(77_800_001)
	)
	members := []string{
		"GTEST_COHORT_INT128_MEMBER_ONE_AAAAAAAAAAAAAAAAAAAAAAA",
		"GTEST_COHORT_INT128_MEMBER_TWO_AAAAAAAAAAAAAAAAAAAAAAA",
	}
	at := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

	for _, m := range members {
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_sponsor_edges
			(sponsor, sponsored, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
			VALUES (?, ?, 1, ?, ?, ?, ?)`, root, m, ledger, ledger, at, at); err != nil {
			t.Fatalf("insert sponsor edge: %v", err)
		}
	}

	lb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare ledger_entries_current: %v", err)
	}
	for i, m := range members {
		if err := lb.Append("trustline", fmt.Sprintf("cohort-int128-tl-%d", i), m, asset,
			int64(math.MaxInt64), "updated", ledger, at, ""); err != nil {
			t.Fatalf("append trustline: %v", err)
		}
	}
	if err := lb.Send(); err != nil {
		t.Fatalf("send trustlines: %v", err)
	}

	if err := chstore.RunCohortRollup(ctx, addr, nil, nil, t.Logf); err != nil {
		t.Fatalf("RunCohortRollup: %v", err)
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

	want := new(big.Int).Mul(big.NewInt(math.MaxInt64), big.NewInt(2))
	for _, h := range cohort.Holdings {
		if h.Asset != asset {
			continue
		}
		if h.Holders != uint64(len(members)) {
			t.Errorf("holders = %d, want %d", h.Holders, len(members))
		}
		if h.Balance == nil || h.Balance.Cmp(want) != 0 {
			t.Fatalf("cohort balance of %s = %v, want %s (Int64 sum wrapped before widening)", asset, h.Balance, want)
		}
		return
	}
	t.Fatalf("no holding row for %s: %+v", asset, cohort.Holdings)
}

// TestClickHouseAccountGraphFundedSumPastInt64 executes the outbound
// graph query's widened funded sum over two edges of 2^62 stroops each.
func TestClickHouseAccountGraphFundedSumPastInt64(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const creator = "GTEST_GRAPH_WIDE_CREATOR_AAAAAAAAAAAAAAAAAAAAAAAAAAA"
	at := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	half := new(big.Int).Lsh(big.NewInt(1), 62)
	for i := range 2 {
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_creator_edges
			(creator, created, creations, funded_stroops, first_ledger, last_ledger, first_at, last_at)
			VALUES (?, ?, 1, ?, 1, 1, ?, ?)`,
			creator, fmt.Sprintf("GTEST_GRAPH_WIDE_CREATED%d_AAAAAAAAAAAAAAAAAAAAAAAAAA", i), half, at, at); err != nil {
			t.Fatalf("insert creator edge: %v", err)
		}
	}
	// The reader serves nothing until both edge tables hold a row.
	if err := raw.Exec(ctx, `INSERT INTO stellar.account_sponsor_edges
		(sponsor, sponsored, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
		VALUES ('GTEST_GRAPH_WIDE_OTHER_SPONSOR', 'GTEST_GRAPH_WIDE_OTHER_SPONSORED', 1, 1, 1, ?, ?)`, at, at); err != nil {
		t.Fatalf("insert sponsor edge: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	g, ok, err := er.AccountGraph(ctx, creator, "", 10, "")
	if err != nil || !ok {
		t.Fatalf("AccountGraph: ok=%v err=%v", ok, err)
	}
	want := new(big.Int).Lsh(big.NewInt(1), 63)
	if g.Created.Accounts != 2 || g.Created.FundedStroops == nil || g.Created.FundedStroops.Cmp(want) != 0 {
		t.Fatalf("created side = (%d accounts, %v funded), want (2, %s)", g.Created.Accounts, g.Created.FundedStroops, want)
	}
}

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

// TestHoldersRollupFreshness_ExecutesAgainstServer runs the real
// ch-holders-rollup cycle, then ages its stamp, against a real ClickHouse
// server. It proves: the writer's UTC-pinned cycle stamp round-trips
// as the true instant; a fresh cycle is served from the rollup, including
// the live-table stamp read for an asset absent from it; and once the cycle
// is older than the reader's max age, AssetHolders stops serving the rollup
// and answers from the live per-request scans.
func TestHoldersRollupFreshness_ExecutesAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	// Other tests in this package read AssetHolders expecting the legacy
	// path; leave the rollup as unpopulated as this test found it.
	t.Cleanup(func() {
		for _, table := range []string{
			"asset_holders_rollup", "asset_holders_counts", "accounts_stats",
			"accounts_wealth_histogram", "accounts_trustline_histogram",
			"asset_stats_daily", "asset_stats_daily_staging",
		} {
			_ = conn.Exec(context.Background(), "TRUNCATE TABLE stellar."+table)
		}
	})

	const listedAsset, lateAsset = "T390L-GISSUERT390", "T390N-GISSUERT390"
	seedEntry := func(entryType, asset, holder string, ledger uint32) {
		t.Helper()
		row := chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC),
			TxHash: "t390", IntraLedgerSeq: 1, ChangeType: "created", EntryType: entryType,
			KeyXDR: "t390-" + entryType + "-" + holder + asset, EntryXDR: "t390", AccountID: holder,
			Asset: asset, Balance: 42,
		}
		if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
			t.Fatalf("InsertEntryChanges(%s %s): %v", entryType, asset, err)
		}
	}

	// The cycle's accounts_stats arm needs at least one funded account
	// (avg() over none is NaN, which toInt64 rejects).
	seedEntry("account", "", "GHOLDERT390L", 72_000_001)
	seedEntry("trustline", listedAsset, "GHOLDERT390L", 72_000_001)
	if err := chstore.RunHoldersRollup(ctx, addr, t.Logf); err != nil {
		t.Fatalf("RunHoldersRollup: %v", err)
	}
	// Issued after the cycle: absent from the rollup, present in the lake.
	seedEntry("trustline", lateAsset, "GHOLDERT390N", 72_000_002)

	var stamp time.Time
	if err := conn.QueryRow(ctx, `SELECT max(computed_at) FROM stellar.asset_holders_rollup`).Scan(&stamp); err != nil {
		t.Fatalf("read cycle stamp: %v", err)
	}
	if age := time.Since(stamp); age < -time.Minute || age > 5*time.Minute {
		t.Fatalf("cycle stamp %s is %s from now — the writer's stamp did not round-trip as the true instant", stamp, age)
	}

	holders := func(asset string) int64 {
		t.Helper()
		r, err := chstore.NewExplorerReader(ctx, addr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		_, total, err := r.AssetHolders(ctx, asset, 5)
		if err != nil {
			t.Fatalf("AssetHolders(%s): %v", asset, err)
		}
		return total
	}

	if got := holders(listedAsset); got != 1 {
		t.Errorf("fresh cycle, listed asset: total = %d, want 1", got)
	}
	// Fresh cycle: the rollup is authoritative, so the late asset reads as
	// zero holders — the live scan would have found one.
	if got := holders(lateAsset); got != 0 {
		t.Errorf("fresh cycle, late asset: total = %d, want 0 served from the rollup", got)
	}

	for _, table := range []string{"asset_holders_rollup", "asset_holders_counts"} {
		if err := conn.Exec(ctx, "ALTER TABLE stellar."+table+
			" UPDATE computed_at = computed_at - INTERVAL 3 HOUR WHERE 1 SETTINGS mutations_sync = 1"); err != nil {
			t.Fatalf("age %s: %v", table, err)
		}
	}
	// Stale cycle: the rollup must no longer answer; the live scan finds the
	// late asset's holder.
	if got := holders(lateAsset); got != 1 {
		t.Errorf("stale cycle, late asset: total = %d, want 1 from the live scan — a 3h-old rollup was served as current", got)
	}
}

// TestCreatorsRollup_BoundaryIsTheNetworks is the test nets' empty-`created`
// cohort proof, run through the real cycle on a real
// ClickHouse. A creation on a post-P23-only chain is recorded ONE way: a
// CAP-67 `transfer` movement paired with a CreateAccount operation. The
// cycle's post-P23 arm reads exactly that pair — but only for ledgers at or
// above its boundary, which must be the network's, not pubnet's constant baked
// into the SQL. Every test-net ledger sits below 58,762,517, so with the pubnet
// constant the classic arm owns all of them and looks for `create_account`
// movements that chain never writes: zero account_creator_edges rows, zero
// `created` cohorts, however full the archive.
//
// Fixture: one CreateAccount operation and its funding transfer at a low
// ledger. The same fixture is rolled up twice — at the pubnet boundary
// (the wrong boundary on a test net: no edge) and at the chain's start
// (the network's boundary: one edge) — so the test pins the substitution,
// not merely that the SQL runs.
func TestCreatorsRollup_BoundaryIsTheNetworks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// A low ledger, as every ledger on a reset test net is. Distinct from
	// the first-run watermark test's [2, 6] so neither disturbs the other's
	// contiguity or lake-min expectations.
	const (
		ledger = uint32(40)
		txHash = "creatorsboundaryaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	creator := gAccountFromSeed(t, 0x31)
	created := gAccountFromSeed(t, 0x32)
	closeTime := time.Date(2027, 9, 1, 0, 0, 40, 0, time.UTC)

	if err := chstore.EnsureAccountMovementsTable(ctx, addr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}

	// The operation, as the indexer's extract lands it: the ledger row (the
	// per-ledger commit marker the walk's tip is read from) plus the
	// CreateAccount operation. The operations side contributes the join
	// key only, so no body is needed.
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	if err := sink.Add(ctx, chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime,
			LedgerHash: "aa03", PrevHash: "bb03", ProtocolVersion: 23, BucketListHash: "cc03",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
		Ops: []chstore.OperationRow{{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, TxIndex: 0, OpIndex: 0,
			OpType: "OperationTypeCreateAccount", SourceAccount: creator,
		}},
	}); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	// The funding leg, as ch-cap67-movements derives it from the CAP-67
	// transfer event: a `transfer` movement from the creator to the new
	// account at the same (ledger, tx_hash, op_index).
	if _, err := chstore.InsertAccountMovements(ctx, addr, []chstore.AccountMovement{{
		MovementKind:    "transfer",
		Provenance:      chstore.ProvenanceCAP67Derived,
		Ledger:          ledger,
		LedgerCloseTime: closeTime,
		TxHash:          txHash,
		OpIndex:         0,
		LegIndex:        0,
		Asset:           "native",
		Amount:          big.NewInt(100_000_000),
		FromAddress:     creator,
		ToAddress:       created,
	}}); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// edgesAt runs one full cycle at the given boundary and reads back the
	// served edge for this creator → created pair: (rows, creations).
	edgesAt := func(boundary uint32) (uint64, uint64) {
		t.Helper()
		if err := chstore.RunCreatorsRollup(ctx, addr, boundary, t.Logf); err != nil {
			t.Fatalf("RunCreatorsRollup(boundary=%d): %v", boundary, err)
		}
		var rows, creations uint64
		const q = `SELECT toUInt64(count()), toUInt64(sum(creations))
			FROM stellar.account_creator_edges
			WHERE creator = ? AND created = ?`
		if err := conn.QueryRow(ctx, q, creator, created).Scan(&rows, &creations); err != nil {
			t.Fatalf("read account_creator_edges (boundary=%d): %v", boundary, err)
		}
		return rows, creations
	}

	// Pubnet's boundary on a chain whose every ledger sits below it: the
	// classic arm owns ledger 40 and finds no create_account movement —
	// the test-net symptom, pinned so the substitution below is
	// shown to be what changes the outcome.
	if rows, _ := edgesAt(chstore.P23BoundaryLedger); rows != 0 {
		t.Fatalf("boundary %d: %d edge rows for a post-P23 creation below the boundary, want 0 "+
			"(the classic arm cannot see a CAP-67 transfer; if it now does, the arms overlap)",
			chstore.P23BoundaryLedger, rows)
	}

	// The network's boundary — the chain's start: the post-P23 arm owns
	// ledger 40, pairs the transfer with the CreateAccount operation, and
	// the creation reaches the served edge table exactly once.
	rows, creations := edgesAt(1)
	if rows != 1 || creations != 1 {
		t.Fatalf("boundary 1: account_creator_edges has %d row(s) / %d creation(s) for the pair, want 1 / 1 — "+
			"the post-P23 arm must own every ledger of a post-P23-only chain", rows, creations)
	}
}

// TestCreatorsRollup_CrossCreatorRecycleCreditsLatestCreator pins that an
// address recycled by DIFFERENT creators (A creates X, X merges, B
// re-creates X) is live only under the creator of its current incarnation.
// Crediting every creator that ever created X counts one account once per
// creator in live_accounts_total and sums its balance into a row whose
// creation no longer backs it.
func TestCreatorsRollup_CrossCreatorRecycleCreditsLatestCreator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		firstLedger  = uint32(51)
		secondLedger = uint32(52)
		txHashA      = "xrecycleaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
		txHashB      = "xrecycleaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2"
		balance      = int64(1_000_000_000)
	)
	creatorA := gAccountFromSeed(t, 0x51)
	creatorB := gAccountFromSeed(t, 0x52)
	created := gAccountFromSeed(t, 0x53)
	firstClose := time.Date(2027, 9, 3, 0, 0, 51, 0, time.UTC)
	secondClose := firstClose.Add(5 * time.Second)

	if err := chstore.EnsureAccountMovementsTable(ctx, addr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}
	// B's creation is the later one, so B's is the incarnation alive now.
	seedCrossCreatorRecycle(ctx, t, addr, []creationFixture{
		{ledger: firstLedger, closeTime: firstClose, txHash: txHashA, creator: creatorA, hashTag: "51"},
		{ledger: secondLedger, closeTime: secondClose, txHash: txHashB, creator: creatorB, hashTag: "52"},
	}, created)

	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: secondLedger, CloseTime: secondClose, TxHash: txHashB, OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "created", EntryType: "account",
			KeyXDR: "xrecycle-account-key-" + created, EntryXDR: "xrecycle-account-entry",
			AccountID: created, Balance: balance,
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	if err := chstore.RunCreatorsRollup(ctx, addr, 1, t.Logf); err != nil {
		t.Fatalf("RunCreatorsRollup: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	assertCreatorLive(ctx, t, conn, creatorA, 0, 0)
	assertCreatorLive(ctx, t, conn, creatorB, 1, balance)

	// Conservation over the whole board, whatever other fixtures share the
	// container: live_accounts_total is the number of DISTINCT created
	// addresses alive now, never one per creator that ever created them.
	var liveTotal int64
	if err := conn.QueryRow(ctx, `SELECT value FROM stellar.account_creators_stats
		WHERE metric = 'live_accounts_total'`).Scan(&liveTotal); err != nil {
		t.Fatalf("read live_accounts_total: %v", err)
	}
	var distinctLive uint64
	if err := conn.QueryRow(ctx, `SELECT uniqExact(created) FROM stellar.account_creators_ops
		WHERE created IN (SELECT account_id FROM stellar.ledger_entries_current FINAL
		                  WHERE entry_type = 'account' AND change_type != 'removed')`).Scan(&distinctLive); err != nil {
		t.Fatalf("read distinct live created: %v", err)
	}
	if liveTotal < 0 || uint64(liveTotal) != distinctLive {
		t.Errorf("live_accounts_total = %d, want %d (distinct created addresses alive now)", liveTotal, distinctLive)
	}
}

type creationFixture struct {
	ledger    uint32
	closeTime time.Time
	txHash    string
	creator   string
	hashTag   string
}

// seedCrossCreatorRecycle writes one CreateAccount op and its funding leg
// per fixture, each in its own ledger, all creating the same address.
func seedCrossCreatorRecycle(ctx context.Context, t *testing.T, addr string, fx []creationFixture, created string) {
	t.Helper()
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	moves := make([]chstore.AccountMovement, 0, len(fx))
	for _, f := range fx {
		if err := sink.Add(ctx, chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{
				LedgerSeq: f.ledger, CloseTime: f.closeTime,
				LedgerHash: "aa" + f.hashTag, PrevHash: "bb" + f.hashTag, ProtocolVersion: 23,
				BucketListHash: "cc" + f.hashTag,
				TotalCoins:     1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
			},
			Ops: []chstore.OperationRow{{
				LedgerSeq: f.ledger, CloseTime: f.closeTime, TxHash: f.txHash, TxIndex: 0, OpIndex: 0,
				OpType: "OperationTypeCreateAccount", SourceAccount: f.creator,
			}},
		}); err != nil {
			t.Fatalf("sink add: %v", err)
		}
		moves = append(moves, chstore.AccountMovement{
			MovementKind:    "transfer",
			Provenance:      chstore.ProvenanceCAP67Derived,
			Ledger:          f.ledger,
			LedgerCloseTime: f.closeTime,
			TxHash:          f.txHash,
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(50_000_000),
			FromAddress:     f.creator,
			ToAddress:       created,
		})
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}
	if _, err := chstore.InsertAccountMovements(ctx, addr, moves); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}
}

func assertCreatorLive(ctx context.Context, t *testing.T, conn clickhouse.Conn, creator string, wantLive uint64, wantStroops int64) {
	t.Helper()
	var (
		accountsCreated uint64
		liveAccounts    uint64
		liveStroops     big.Int
	)
	if err := conn.QueryRow(ctx, `SELECT accounts_created, live_accounts, live_stroops
		FROM stellar.account_creators_rollup WHERE creator = ?`, creator).
		Scan(&accountsCreated, &liveAccounts, &liveStroops); err != nil {
		t.Fatalf("read board row for %s: %v", creator, err)
	}
	if accountsCreated != 1 {
		t.Errorf("%s accounts_created = %d, want 1 (immutable history keeps each creation)", creator, accountsCreated)
	}
	if liveAccounts != wantLive {
		t.Errorf("%s live_accounts = %d, want %d (only the current incarnation's creator is live)", creator, liveAccounts, wantLive)
	}
	if liveStroops.Cmp(big.NewInt(wantStroops)) != 0 {
		t.Errorf("%s live_stroops = %s, want %d", creator, liveStroops.String(), wantStroops)
	}
}

// TestCreatorsRollup_RecycledAddressCountsOnce is the recycled-address proof
// against real ClickHouse. stellar.account_creators_ops is one row
// per creation OPERATION, so one creator recycling one address (create ->
// merge -> create) produces two rows sharing the same `created`. Joining
// the board's live_accounts/live_stroops live-entry set onto that per-event
// table directly counts the still-live address TWICE and sums its balance
// TWICE, so the board aggregates over the distinct
// (creator, created) pair before the outer sum.
//
// Fixture: one creator, one address created by two distinct operations,
// live with a single known balance. The served board row must show
// live_accounts = 1 and live_stroops = that one balance, not 2x.
func TestCreatorsRollup_RecycledAddressCountsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger    = uint32(41)
		txHashOne = "recycledaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
		txHashTwo = "recycledaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2"
		balance   = int64(100_000_000) // one XLM's worth of stroops, the survivor's true balance
	)
	creator := gAccountFromSeed(t, 0x41)
	created := gAccountFromSeed(t, 0x42)
	closeTime := time.Date(2027, 9, 2, 0, 0, 41, 0, time.UTC)

	if err := chstore.EnsureAccountMovementsTable(ctx, addr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	if err := sink.Add(ctx, chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime,
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
		Ops: []chstore.OperationRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashOne, TxIndex: 0, OpIndex: 0,
				OpType: "OperationTypeCreateAccount", SourceAccount: creator,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashTwo, TxIndex: 1, OpIndex: 0,
				OpType: "OperationTypeCreateAccount", SourceAccount: creator,
			},
		},
	}); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	// Two funding legs for the SAME (creator, created) pair — the recycle:
	// created, presumably merged away, created again by the same funder.
	if _, err := chstore.InsertAccountMovements(ctx, addr, []chstore.AccountMovement{
		{
			MovementKind:    "transfer",
			Provenance:      chstore.ProvenanceCAP67Derived,
			Ledger:          ledger,
			LedgerCloseTime: closeTime,
			TxHash:          txHashOne,
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(50_000_000),
			FromAddress:     creator,
			ToAddress:       created,
		},
		{
			MovementKind:    "transfer",
			Provenance:      chstore.ProvenanceCAP67Derived,
			Ledger:          ledger,
			LedgerCloseTime: closeTime,
			TxHash:          txHashTwo,
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(50_000_000),
			FromAddress:     creator,
			ToAddress:       created,
		},
	}); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	// The address's current live state: one account entry, one balance.
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashTwo, OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "account",
			KeyXDR: "recycled-account-key-" + created, EntryXDR: "recycled-account-entry",
			AccountID: created, Balance: balance,
		},
	}, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	if err := chstore.RunCreatorsRollup(ctx, addr, 1, t.Logf); err != nil {
		t.Fatalf("RunCreatorsRollup: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var (
		accountsCreated uint64
		liveAccounts    uint64
		liveStroops     big.Int
	)
	const q = `SELECT accounts_created, live_accounts, live_stroops
		FROM stellar.account_creators_rollup
		WHERE creator = ?`
	if err := conn.QueryRow(ctx, q, creator).Scan(&accountsCreated, &liveAccounts, &liveStroops); err != nil {
		t.Fatalf("read account_creators_rollup: %v", err)
	}

	// accounts_created is immutable history and stays per-event: two
	// creation operations, so two.
	if accountsCreated != 2 {
		t.Errorf("accounts_created = %d, want 2 (immutable per-event history)", accountsCreated)
	}
	// live_accounts/live_stroops describe the SURVIVING set: one address,
	// its one true balance — not doubled by the two creation events that
	// produced it.
	if liveAccounts != 1 {
		t.Errorf("live_accounts = %d, want 1 (one surviving address recycled twice, not 2)", liveAccounts)
	}
	if liveStroops.Cmp(big.NewInt(balance)) != 0 {
		t.Errorf("live_stroops = %s, want %d (the address's one true balance, not summed once per creation event)", liveStroops.String(), balance)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	hit, ok, err := reader.AccountCreators(ctx, 10, creator)
	if err != nil || !ok || len(hit.Board) != 1 {
		t.Fatalf("AccountCreators(keyed hit) = %d rows, ok %v, err %v", len(hit.Board), ok, err)
	}
	// A keyed miss still reports the cycle's time, never the zero time.
	miss, ok, err := reader.AccountCreators(ctx, 10, gAccountFromSeed(t, 0x7e))
	if err != nil || !ok {
		t.Fatalf("AccountCreators(keyed miss) = ok %v, err %v", ok, err)
	}
	if len(miss.Board) != 0 {
		t.Fatalf("keyed miss Board = %+v, want empty", miss.Board)
	}
	assertCycleTime(t, miss.ComputedAt, hit.ComputedAt)
}

// TestTrustlineAssetsAfter_PoolPrefixExcludesOnlyRealPoolShares is the
// executing proof of CA2-A14-correct-5: TrustlineAssetsAfter's pool-share
// exclusion must match only the two spellings TrustLineAssetID emits
// ("pool:<hex>" and the bare "pool"), never a real credit-asset string that
// merely starts with the substring "pool" — asset codes are case-sensitive
// and "poolX" etc. are valid Stellar asset codes (internal/canonical/asset.go
// validateClassicAssetCode).
//
// `NOT startsWith(asset, 'pool')` would drop "poolX-GISSUER..." silently
// alongside the real pool-share rows; this test goes RED on that predicate.
func TestTrustlineAssetsAfter_PoolPrefixExcludesOnlyRealPoolShares(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger    = uint32(70_100_000)
		realAsset = "poolX-GCA14POOLPREFIXTESTISSUERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-1", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-real", Asset: realAsset,
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-2", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 2, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-poolhex", Asset: "pool:c24deadbeef",
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-3", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-poolbare", Asset: "pool",
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	scanner, err := chstore.NewHoldingsScanner(ctx, addr)
	if err != nil {
		t.Fatalf("NewHoldingsScanner: %v", err)
	}
	defer func() { _ = scanner.Close() }()

	// "ooo" sorts after every uppercase-coded asset other tests seed
	// (ASCII uppercase < lowercase) but strictly before "pool", "pool:..."
	// and "poolX...", so this page captures all three fixture rows without
	// depending on how many unrelated rows the shared container holds.
	seeds, err := scanner.TrustlineAssetsAfter(ctx, "ooo", 50)
	if err != nil {
		t.Fatalf("TrustlineAssetsAfter: %v", err)
	}

	var gotReal, gotPoolHex, gotPoolBare bool
	for _, s := range seeds {
		switch s.Asset {
		case realAsset:
			gotReal = true
		case "pool:c24deadbeef":
			gotPoolHex = true
		case "pool":
			gotPoolBare = true
		}
	}

	if !gotReal {
		t.Errorf("TrustlineAssetsAfter dropped real asset %q — the pool-prefix exclusion over-matched a credit asset code starting with %q", realAsset, "pool")
	}
	if gotPoolHex {
		t.Errorf("TrustlineAssetsAfter returned the pool:<hex> share row %q — pool-share exclusion regressed", "pool:c24deadbeef")
	}
	if gotPoolBare {
		t.Errorf("TrustlineAssetsAfter returned the bare pool fallback row — pool-share exclusion regressed")
	}
}

// TestClickHouseAssetEntryChanges pins the asset entry-history read: a
// re-derived row collapses to its newest version before RMT merges, keyset
// pages never repeat or skip a row, nothing above the derive watermark is
// served, and an Int128 balance above 2^64 reaches the wire exact.
func TestClickHouseAssetEntryChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		asset  = "AECTEST-" + issuer
		base   = uint32(170_000_000)
		wm     = base + 10
	)
	t.Cleanup(func() {
		bg := context.Background()
		_ = raw.Exec(bg, `DELETE FROM stellar.asset_entry_changes WHERE asset = ? SETTINGS mutations_sync = 2`, asset)
		_ = raw.Exec(bg, `DELETE FROM stellar.entry_history_watermark WHERE name IN ('entry_history', ?) SETTINGS mutations_sync = 2`,
			chstore.EntryHistoryBackfillMarker)
	})
	if err := raw.Exec(ctx, `SYSTEM STOP MERGES stellar.asset_entry_changes`); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), `SYSTEM START MERGES stellar.asset_entry_changes`) })

	big128 := new(big.Int).Lsh(big.NewInt(1), 100)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	old, newer := at, at.Add(time.Hour)
	insert := func(rows ...[]any) {
		t.Helper()
		b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.asset_entry_changes
			(asset, ledger, close_time, tx_hash, op_index, change_index, role, intra_ledger_seq,
			 entry_type, change_type, changed, account, balance, fields, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		for _, r := range rows {
			if err := b.Append(r...); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		if err := b.Send(); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	row := func(ledger uint32, op int32, role, entryType string, bal *big.Int, fields string, ing time.Time) []any {
		return []any{
			asset, ledger, at, "aec-tx", op, uint32(0), role, uint32(0),
			entryType, "updated",
			[]string{"balance"},
			"GHOLDER", bal, fields, ing,
		}
	}
	insert(
		row(base+1, -1, "holder", "trustline", big128, `{"balance":"big"}`, old),
		row(base+2, 0, "claimable", "claimable_balance", big.NewInt(7), `{"v":"old"}`, old),
		row(base+3, 1, "selling", "offer", big.NewInt(9), `{}`, old),
		row(base+20, 0, "holder", "trustline", big.NewInt(1), `{}`, old),
	)
	// Later derives of the claimable row, in their own part so they are not yet merged.
	// Enough stale copies that no window proves its page, forcing the floored exact read.
	rederives := [][]any{row(base+2, 0, "claimable", "claimable_balance", big.NewInt(8), `{"v":"new"}`, newer)}
	for i := 0; i < 12; i++ {
		rederives = append(rederives, row(base+2, 0, "claimable", "claimable_balance", big.NewInt(7), `{"v":"old"}`, old.Add(time.Duration(i)*time.Second)))
	}
	insert(rederives...)

	er, err := chstore.NewExplorerReader(ctx, clickhouseAddr(t))
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	var seen []uint32
	var cur chstore.AssetEntryChangeCursor
	for page := 0; page < 5; page++ {
		got, err := er.AssetEntryChanges(ctx, asset, 1, cur, wm)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(got) == 0 {
			break
		}
		r := got[0]
		if r.Ledger == base+2 && (r.Balance.Int64() != 8 || r.Fields != `{"v":"new"}`) {
			t.Fatalf("claimable row = %+v, want the newest derive", r)
		}
		if r.Ledger == base+1 && r.Balance.Cmp(big128) != 0 {
			t.Fatalf("holder balance = %s, want 2^100", r.Balance)
		}
		seen = append(seen, r.Ledger)
		cur = chstore.AssetEntryChangeCursor{Ledger: r.Ledger, TxHash: r.TxHash, OpIndex: r.OpIndex, ChangeIndex: r.ChangeIndex, Role: r.Role}
	}
	if len(seen) != 3 || seen[0] != base+3 || seen[1] != base+2 || seen[2] != base+1 {
		t.Fatalf("paged ledgers = %v, want [%d %d %d] (deduped, ceilinged at %d)", seen, base+3, base+2, base+1, wm)
	}

	if err := raw.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES ('entry_history', ?), (?, ?)`,
		wm, chstore.EntryHistoryBackfillMarker, wm); err != nil {
		t.Fatalf("watermark insert: %v", err)
	}
	if got, thru, err := er.EntryHistoryCoverage(ctx); err != nil || got < wm || thru < wm {
		t.Fatalf("coverage = %d, %d, %v; want both at least %d", got, thru, err, wm)
	}

	ts := httptest.NewServer(v1.New(v1.Options{Explorer: er}).Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/assets/AECTEST:" + issuer + "/entry-changes?limit=5")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data struct {
			Asset         string `json:"asset"`
			ThroughLedger uint32 `json:"through_ledger"`
			LowerBound    bool   `json:"lower_bound"`
			Changes       []struct {
				Ledger  uint32          `json:"ledger"`
				OpIndex int32           `json:"op_index"`
				Amount  string          `json:"amount"`
				Entry   json.RawMessage `json:"entry"`
			} `json:"changes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d decode %v", resp.StatusCode, err)
	}
	d := body.Data
	if d.Asset != asset || d.LowerBound || d.ThroughLedger < wm || len(d.Changes) != 3 {
		t.Fatalf("HTTP view = %+v, want 3 changes of %s through %d, not a lower bound", d, asset, wm)
	}
	if last := d.Changes[2]; last.Amount != big128.String() || last.OpIndex != -1 || string(last.Entry) != `{"balance":"big"}` {
		t.Fatalf("oldest change = %+v, want the exact 2^100 tx-level holder row", last)
	}
}

// TestAssetStatsDaily_ExecutesAgainstServer runs the real ch-holders-rollup
// cycle twice against a real ClickHouse server and reads the day's snapshot
// back. It proves the trustline count includes zero-balance lines while
// holders do not, the balance sums stay exact past 2^63, the Gini matches a
// hand-computed value, and a second cycle on the same day replaces the day's
// rows rather than adding to them.
func TestAssetStatsDaily_ExecutesAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	t.Cleanup(func() {
		for _, table := range []string{
			"asset_holders_rollup", "asset_holders_counts", "accounts_stats",
			"accounts_wealth_histogram", "accounts_trustline_histogram",
			"asset_stats_daily", "asset_stats_daily_staging",
		} {
			_ = conn.Exec(context.Background(), "TRUNCATE TABLE stellar."+table)
		}
	})

	const spreadAsset, wideAsset = "A3AS-GISSUERA3A", "A3AW-GISSUERA3A"
	seed := func(entryType, asset, holder string, balance int64) {
		t.Helper()
		row := chstore.LedgerEntryChangeRow{
			LedgerSeq: 73_000_001, CloseTime: time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC),
			TxHash: "a3a", IntraLedgerSeq: 1, ChangeType: "created", EntryType: entryType,
			KeyXDR: "a3a-" + entryType + "-" + holder + asset, EntryXDR: "a3a", AccountID: holder,
			Asset: asset, Balance: balance,
		}
		if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
			t.Fatalf("InsertEntryChanges(%s %s %s): %v", entryType, asset, holder, err)
		}
	}
	// The cycle's accounts_stats arm needs at least one funded account.
	seed("account", "", "GHOLDERA3A1", 42)
	seed("trustline", spreadAsset, "GHOLDERA3A1", 10)
	seed("trustline", spreadAsset, "GHOLDERA3A2", 20)
	seed("trustline", spreadAsset, "GHOLDERA3A3", 30)
	seed("trustline", spreadAsset, "GHOLDERA3A4", 0)
	seed("trustline", wideAsset, "GHOLDERA3A1", math.MaxInt64)
	seed("trustline", wideAsset, "GHOLDERA3A2", math.MaxInt64)

	for range 2 {
		if err := chstore.RunHoldersRollup(ctx, addr, t.Logf); err != nil {
			t.Fatalf("RunHoldersRollup: %v", err)
		}
	}

	type snapshot struct {
		day                  time.Time
		holders, trustlines  int64
		total, top10, top100 string
		gini                 *float64
	}
	read := func(asset string) snapshot {
		t.Helper()
		rows, err := conn.Query(ctx, `
			SELECT day, holders, trustlines, toString(balance_total), toString(top10_balance),
			       toString(top100_balance), gini
			FROM stellar.asset_stats_daily WHERE asset = ?`, asset)
		if err != nil {
			t.Fatalf("read snapshot %s: %v", asset, err)
		}
		defer func() { _ = rows.Close() }()
		var out []snapshot
		for rows.Next() {
			var s snapshot
			if err := rows.Scan(&s.day, &s.holders, &s.trustlines, &s.total, &s.top10, &s.top100, &s.gini); err != nil {
				t.Fatalf("scan snapshot %s: %v", asset, err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot rows %s: %v", asset, err)
		}
		if len(out) != 1 {
			t.Fatalf("%s has %d snapshot row(s) after two same-day cycles, want exactly 1", asset, len(out))
		}
		return out[0]
	}

	today := time.Now().UTC().Format("2006-01-02")
	spread := read(spreadAsset)
	if got := spread.day.Format("2006-01-02"); got != today {
		t.Errorf("snapshot day = %s, want the cycle's UTC day %s", got, today)
	}
	if spread.holders != 3 || spread.trustlines != 4 {
		t.Errorf("holders/trustlines = %d/%d, want 3/4 (the zero-balance line counts as a trustline only)", spread.holders, spread.trustlines)
	}
	if spread.total != "60" || spread.top10 != "60" || spread.top100 != "60" {
		t.Errorf("total/top10/top100 = %s/%s/%s, want 60/60/60", spread.total, spread.top10, spread.top100)
	}
	// Mean absolute difference over 10, 20, 30: 80 / (2 · 3² · 20) = 2/9.
	if spread.gini == nil || math.Abs(*spread.gini-2.0/9.0) > 1e-12 {
		t.Errorf("gini = %s, want 2/9", giniString(spread.gini))
	}

	wide := read(wideAsset)
	if wide.total != "18446744073709551614" || wide.top10 != "18446744073709551614" {
		t.Errorf("total/top10 = %s/%s, want 18446744073709551614 (2·(2^63−1), exact past Int64)", wide.total, wide.top10)
	}
	if wide.gini == nil || *wide.gini != 0 {
		t.Errorf("gini of two equal holders = %s, want 0", giniString(wide.gini))
	}

	native := read("native")
	if native.holders < 1 || native.trustlines < native.holders {
		t.Errorf("native holders/trustlines = %d/%d, want ≥1 and trustlines ≥ holders", native.holders, native.trustlines)
	}
}

func giniString(g *float64) string {
	if g == nil {
		return "NULL"
	}
	return strconv.FormatFloat(*g, 'g', -1, 64)
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

// legacyBlendReservesSQL is the BlendPoolReserves lookup as it stood
// before the current-state rewrite, frozen here as the differential oracle: a
// 250,000-ledger windowed fold over stellar.ledger_entry_changes, resolving
// the latest entry per key itself. The rewrite must agree with it row for row
// on every reserve the window could see, while no longer reading one granule
// per WRITE the pool made inside that window.
func legacyBlendReservesSQL() string {
	return `SELECT key_xdr, argMax(entry_xdr, (ledger_seq, intra_ledger_seq)) AS latest_xdr
		FROM stellar.ledger_entry_changes
		WHERE entry_type = 'contract_data'
		  AND ledger_seq > (SELECT max(ledger_seq) FROM stellar.ledger_entry_changes) - ?
		  AND key_xdr IN (?)
		GROUP BY key_xdr
		HAVING argMax(change_type, (ledger_seq, intra_ledger_seq)) != 'removed'`
}

// legacyBlendWindowLedgers is the frozen oracle's window width.
const legacyBlendWindowLedgers = uint32(250_000)

// TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead is the
// live-ClickHouse proof for (`/v1/lending/pools/{pool}/reserves` →
// 503 lending-timeout at 12.1s on the largest Blend pool; 9.31s — 78% of the
// same 12s budget — on a SMALL one).
//
// Pathology: the reserve lookup folded the latest entry per key out of
// stellar.ledger_entry_changes across a 250,000-ledger (~14-day) window. A
// Blend ResData entry is REWRITTEN on nearly every pool interaction, so one
// reserve's key alone matches tens of thousands of rows scattered through
// that window's granules (tens of thousands of rows for the busiest mainnet pool's USDC
// reserve). The legacy read's cost was therefore a function of pool
// WRITE ACTIVITY, not of how many reserves were asked for — a multi-second
// floor under EVERY pool.
//
// The fix reads stellar.ledger_entries_current, whose sort key IS
// (entry_type, key_xdr): ~one row per requested key, the same shape the three
// sibling pool-state readers (Soroswap / Phoenix / Comet) have always used.
//
// Removing that window, however, also removed a bound it was serving by
// ACCIDENT. An archived (TTL-lapsed) Soroban entry has had no writes since it
// lapsed, so the narrow window dropped it as a side effect of being narrow,
// while ledger_entries_current keeps its last-known value forever. Reading the
// projection without an explicit staleness bound hands a DEAD pool's final
// reserves to the handler, which prices them at today's USD rate into
// `tvl_usd` and stamps the current watermark with flags.stale=false — a
// fabricated TVL where a naive read returned an empty reserve list. So the
// read is paired with the same archived-entry drop the three sibling readers
// carry, and this test pins BOTH halves: quiet-but-live must answer, archived
// must not. Conflating those two is the bug this fixture exists to prevent.
//
// Fixture — one pool, five reserves chosen to cover every axis:
//
//   - assetHot: ResData rewritten across `churn` ledgers inside the window,
//     the LAST write carrying a distinct b_rate. This is the pathology: the
//     legacy fold must read every one of those writes to find the winner.
//   - assetGone: written, then REMOVED as its final change. Must be absent
//     from BOTH paths — the removed-key drop is the semantics the old
//     `HAVING argMax(change_type, ...) != 'removed'` carried, and that the
//     empty-entry_xdr filter over FINAL must preserve.
//   - assetTied: two writes in the SAME ledger, the stale one sorting FIRST
//     in the base table. Pins the intra-ledger tie-break through the new path:
//     the projection's version is (ledger_seq << 32) | intra_ledger_seq, so
//     FINAL keeps the LAST intra-ledger change, exactly as the old composite
//     argMax did.
//   - assetQuiet: a single write far BELOW the legacy window, with a TTL
//     entry that is still LIVE. Quiet is not dead: the old shape reported it
//     absent ("consistent with captured window") and the projection answers.
//     The genuine coverage win — and it must SURVIVE the archived drop.
//   - assetArchived: written in the same old ledger as assetQuiet, but with a
//     LAPSED TTL. As visible to the projection as assetQuiet is; the ONLY
//     thing separating the two is the TTL verdict. Must be ABSENT, or the
//     route publishes a dead pool's reserves as current liquidity.
//
// The test then:
//
//  1. DIFFERENTIAL: the frozen legacy SQL and the reader must agree on the
//     winning entry for every reserve the window could see (hot, tied), agree
//     on the removal (gone absent from both), and differ on the two OLD
//     reserves — quiet ADMITTED where the legacy window could not see it,
//     archived still absent but now for a stated reason rather than as a
//     lucky side effect of the scan bound.
//  2. READ-ROWS: both paths measured via system.query_log. The legacy shape
//     reads at least the pool's whole in-window write history; the new one
//     must read a small multiple of the key count. Red-proof: with the old
//     query text in the reader the two figures are equal and the bound
//     below fails.
func TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		poolSeed     = byte(0xC0)
		hotSeed      = byte(0xC1)
		goneSeed     = byte(0xC2)
		tiedSeed     = byte(0xC3)
		quietSeed    = byte(0xC4)
		archivedSeed = byte(0xC5)

		// Far above every other fixture, so this test's own top (base+churn)
		// is the lake's max(ledger_seq) and the frozen oracle's 250k window
		// therefore covers the fixture — in any shard and any test order.
		// It does not lean on another test: the one other fixture up here
		// (4_000_000_000, lake_test.go) is within
		// 250k above base, so the window holds with it present too, and that
		// test removes its rows when it finishes. Asserted below rather than
		// assumed. This test removes its own rows the same way (see the
		// purgeLakeFixtureLedgers call below).
		base  = uint32(3_999_900_000)
		churn = 3_000
		// Far below the window: the legacy shape cannot see these two.
		quietLedger = uint32(1_000_000)

		// TTL verdicts are judged against max(ledger_seq) at compute time,
		// which the suite shares. These two sit far either side of any tip
		// this test can run under (its own top, base+churn, or 4e9 while
		// the argmax fixture is live), so the verdicts hold in any order.
		liveUntilLive     = uint32(4_294_000_000)
		liveUntilArchived = uint32(1_500_000_000)

		staleBRate    = uint64(1_000_000_000_000) // 1.0 at 12 decimals
		finalBRate    = uint64(2_000_000_000_000) // 2.0 — the winning write
		quietBRate    = uint64(3_000_000_000_000) // 3.0
		archivedBRate = uint64(4_000_000_000_000) // 4.0 — must never be served
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Everything seeded at [base, base+churn] is removed again when this test
	// finishes. Rows this high are the table's max(ledger_seq), which bounds
	// every whole-lake walker in the process: left behind, they cost each
	// later claimable-balance / SAC full-history seed walk ~16,000 empty
	// 250k-ledger windows. The quiet + archived rows at quietLedger
	// stay — they are below any realistic tip and bound nothing.
	purgeLakeFixtureLedgers(t, addr, base, base+churn)

	pool := contractIDFromSeed(poolSeed)
	poolStr := mustContractStrkey(t, poolSeed)
	hot, gone, tied := contractIDFromSeed(hotSeed), contractIDFromSeed(goneSeed), contractIDFromSeed(tiedSeed)
	quiet, archived := contractIDFromSeed(quietSeed), contractIDFromSeed(archivedSeed)
	hotStr := mustContractStrkey(t, hotSeed)
	goneStr := mustContractStrkey(t, goneSeed)
	tiedStr := mustContractStrkey(t, tiedSeed)
	quietStr := mustContractStrkey(t, quietSeed)
	archivedStr := mustContractStrkey(t, archivedSeed)

	hotKey := resDataKeyB64(t, pool, hot)
	goneKey := resDataKeyB64(t, pool, gone)
	tiedKey := resDataKeyB64(t, pool, tied)
	quietKey := resDataKeyB64(t, pool, quiet)
	archivedKey := resDataKeyB64(t, pool, archived)

	hotFinalEntry := resDataEntryB64(t, pool, hot, base+churn, finalBRate)
	tiedFinalEntry := resDataEntryB64(t, pool, tied, base+churn, finalBRate)
	quietEntry := resDataEntryB64(t, pool, quiet, quietLedger, quietBRate)
	archivedEntry := resDataEntryB64(t, pool, archived, quietLedger, archivedBRate)

	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.90q: %v", q, err)
		}
	}

	// The hot reserve's churn: `churn` writes of the STALE state, one per
	// ledger, in ONE insert so they land together. This is what the legacy
	// fold has to read in full to find the winner.
	mustExec(fmt.Sprintf(`INSERT INTO stellar.ledger_entry_changes
		(ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, intra_ledger_seq)
		SELECT toUInt32(%d + number), toDateTime('2024-01-01 00:00:00', 'UTC'), lpad(toString(number), 64, '0'), 0, 0,
		       'updated', 'contract_data', '%s', '%s', 1
		FROM numbers(%d)`, base, hotKey, resDataEntryB64(t, pool, hot, base, staleBRate), churn))

	rows := []chstore.LedgerEntryChangeRow{
		// assetHot: the winning write, one ledger past the churn.
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-hot", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: hotKey, EntryXDR: hotFinalEntry,
		},
		// assetGone: a live write, then a REMOVAL as the final change.
		{
			LedgerSeq: base + 10, CloseTime: closeTime, TxHash: "blend504-gone", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: goneKey, EntryXDR: resDataEntryB64(t, pool, gone, base+10, staleBRate),
		},
		{
			LedgerSeq: base + 11, CloseTime: closeTime, TxHash: "blend504-gone", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "removed", EntryType: "contract_data",
			KeyXDR: goneKey, EntryXDR: "",
		},
		// assetTied: the LATER same-ledger change (intra 9), written first so
		// it sorts LAST in the base table's ORDER BY — a ledger_seq-only
		// tie-break keeps the stale row below instead.
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-tied", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: tiedKey, EntryXDR: tiedFinalEntry,
		},
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-tied", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: tiedKey, EntryXDR: resDataEntryB64(t, pool, tied, base+churn, staleBRate),
		},
		// assetQuiet + assetArchived: one write each, far below the legacy
		// window, IDENTICAL in every respect the projection can see. Only
		// their TTL entries (seeded below) differ.
		{
			LedgerSeq: quietLedger, CloseTime: closeTime, TxHash: "blend504-quiet", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: quietKey, EntryXDR: quietEntry,
		},
		{
			LedgerSeq: quietLedger, CloseTime: closeTime, TxHash: "blend504-archived", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: archivedKey, EntryXDR: archivedEntry,
		},
		// The TTL entries that separate them. ttlChangeRow (seed_test.go)
		// renders the lake's TTL change for a governed key; the
		// ttl_live_until_mv materialized view turns it into the slim projection
		// ClassifyTTLLiveness reads.
		ttlChangeRow(quietKey, base+1, 1, liveUntilLive, 48),
		ttlChangeRow(archivedKey, base+1, 1, liveUntilArchived, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// Collapse ledger_entries_current to its steady state. In production
	// background merges keep the projection at ~one row per live key, which is
	// what makes the PK-prefix probe cheap; a container that was seeded
	// seconds ago has every version still sitting in unmerged parts, so
	// without this the read_rows figures below would measure merge lag rather
	// than query shape. FINAL returns the same ANSWER either way — this only
	// removes the fixture's own artefact.
	mustExec(`OPTIMIZE TABLE stellar.ledger_entries_current FINAL`)

	// The oracle derives its window from the table's global max ledger, which
	// the whole suite shares. Prove the fixture is actually inside it, or the
	// differential below would be comparing against an empty legacy answer.
	var maxLedger uint32
	if err := raw.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.ledger_entry_changes`).Scan(&maxLedger); err != nil {
		t.Fatalf("read max ledger: %v", err)
	}
	if maxLedger < base+churn || maxLedger-legacyBlendWindowLedgers >= base {
		t.Fatalf("fixture outside the frozen oracle's window: max(ledger_seq)=%d, window starts at %d, fixture spans [%d, %d] — the legacy answer would be empty and every comparison below vacuous",
			maxLedger, maxLedger-legacyBlendWindowLedgers, base, base+churn)
	}
	if quietLedger > maxLedger-legacyBlendWindowLedgers {
		t.Fatalf("the quiet + archived reserves (ledger %d) landed INSIDE the legacy window (starts %d) — they exist to separate the retired capture-window caveat from the staleness bound, so they must sit below it",
			quietLedger, maxLedger-legacyBlendWindowLedgers)
	}
	// The TTL verdicts are judged at the lake tip; assert the fixture's two
	// live_until values still straddle it, or the archived assertion below
	// would pass for the wrong reason (or the quiet one fail spuriously).
	if liveUntilArchived >= maxLedger {
		t.Fatalf("assetArchived's live_until (%d) is at/above the lake tip (%d) — it would classify LIVE and the archived-drop assertion would be vacuous", liveUntilArchived, maxLedger)
	}
	if liveUntilLive < maxLedger {
		t.Fatalf("assetQuiet's live_until (%d) is below the lake tip (%d) — it would classify ARCHIVED and the quiet-survives assertion would be testing the opposite property", liveUntilLive, maxLedger)
	}

	keys := []string{hotKey, goneKey, tiedKey, quietKey, archivedKey}
	legacyWinners := func(qctx context.Context) map[string]string {
		t.Helper()
		out := map[string]string{}
		rs, err := raw.Query(qctx, legacyBlendReservesSQL(), legacyBlendWindowLedgers, keys)
		if err != nil {
			t.Fatalf("legacy blend reserves: %v", err)
		}
		defer func() { _ = rs.Close() }()
		for rs.Next() {
			var k, entry string
			if err := rs.Scan(&k, &entry); err != nil {
				t.Fatalf("legacy scan: %v", err)
			}
			out[k] = entry
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("legacy rows: %v", err)
		}
		return out
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	assets := []string{hotStr, goneStr, tiedStr, quietStr, archivedStr}

	// (1) Differential.
	legacy := legacyWinners(ctx)
	states, err := reader.BlendPoolReserves(ctx, poolStr, blend.PoolV2, assets, nil)
	if err != nil {
		t.Fatalf("BlendPoolReserves: %v", err)
	}
	byAsset := make(map[string]chstore.BlendReserveState, len(states))
	for _, s := range states {
		byAsset[s.Asset] = s
	}

	// The fixture must actually reproduce the pathology the oracle models:
	// the legacy path has to see the hot + tied reserves, and must NOT see
	// the removed one.
	for _, want := range []struct {
		key, label, entry string
	}{
		{hotKey, "assetHot", hotFinalEntry},
		{tiedKey, "assetTied", tiedFinalEntry},
	} {
		got, ok := legacy[want.key]
		if !ok {
			t.Fatalf("legacy oracle did not resolve %s — the fixture no longer reproduces the pre-fix behaviour, so the agreement below is vacuous", want.label)
		}
		if got != want.entry {
			t.Fatalf("legacy oracle resolved %s to an unexpected write — the fixture's winner is not the one seeded as final", want.label)
		}
	}
	if _, present := legacy[goneKey]; present {
		t.Fatal("legacy oracle resolved assetGone — its final change is a removal; the fixture is wrong")
	}
	for _, tc := range []struct{ key, label string }{{quietKey, "assetQuiet"}, {archivedKey, "assetArchived"}} {
		if _, present := legacy[tc.key]; present {
			t.Fatalf("legacy oracle resolved %s from ledger %d — both old reserves must sit below the window, or the difference assertions mean nothing", tc.label, quietLedger)
		}
	}

	// Reader vs oracle: same answer for everything the window could see.
	for _, tc := range []struct {
		asset, label string
		wantBRate    uint64
	}{
		{hotStr, "assetHot", finalBRate},
		{tiedStr, "assetTied", finalBRate},
	} {
		got, ok := byAsset[tc.asset]
		if !ok {
			t.Errorf("%s absent from the reader's result; the legacy path resolved it", tc.label)
			continue
		}
		want := new(big.Int).SetUint64(tc.wantBRate)
		if got.Data.BRate == nil || got.Data.BRate.Cmp(want) != 0 {
			t.Errorf("%s b_rate = %v, want %d — the projection resolved a different write than the frozen fold did", tc.label, got.Data.BRate, tc.wantBRate)
		}
	}
	if _, present := byAsset[goneStr]; present {
		t.Error("assetGone present in the reader's result; want ABSENT — its final change was a removal, and `entry_xdr != ''` over FINAL must drop it exactly as the old HAVING did")
	}

	// The two OLD reserves — identical to the projection, opposite verdicts.
	// This pair is the whole point: dropping the 250k-ledger window retired a
	// capture-window caveat AND removed an accidental staleness bound, and the
	// fix must land on the right side of both.
	//
	// Quiet-but-live: ADMITTED. This is the coverage win — a reserve nobody has
	// touched in months is not a dead one, and the old shape reported it absent.
	q, ok := byAsset[quietStr]
	if !ok {
		t.Error("assetQuiet absent from the reader's result — a QUIET reserve is not a dead one; reading the current-state projection is supposed to retire the 250k-ledger capture-window caveat, and the archived-entry drop must not over-reach into live-but-old entries")
	} else if want := new(big.Int).SetUint64(quietBRate); q.Data.BRate == nil || q.Data.BRate.Cmp(want) != 0 {
		t.Errorf("assetQuiet b_rate = %v, want %d", q.Data.BRate, quietBRate)
	}

	// Positively archived: ABSENT. The projection still holds this entry's
	// last-known value and the query returns it — nothing in the SQL can tell
	// it apart from assetQuiet. Only the TTL verdict can, and if it is not
	// consulted the handler prices a dead pool's reserves at today's USD rate
	// into tvl_usd and publishes it with flags.stale=false.
	if got, present := byAsset[archivedStr]; present {
		t.Errorf("assetArchived present in the reader's result (b_rate=%v) — its TTL lapsed at ledger %d against a lake tip of %d, so its last-known reserves are NOT current liquidity. The pre-#504 250k-ledger window dropped it as a side effect of being narrow; removing that window without an explicit archived-entry drop publishes a fabricated TVL for a dead pool",
			got.Data.BRate, liveUntilArchived, maxLedger)
	}

	// (2) read_rows, one measured call down each path.
	t0 := time.Now().Add(-time.Second)
	legacyID := uuid.NewString()
	_ = legacyWinners(clickhouse.Context(ctx, clickhouse.WithQueryID(legacyID)))
	readerID := uuid.NewString()
	if _, err := reader.BlendPoolReserves(clickhouse.Context(ctx, clickhouse.WithQueryID(readerID)), poolStr, blend.PoolV2, assets, nil); err != nil {
		t.Fatalf("BlendPoolReserves (measured): %v", err)
	}
	mustExec(`SYSTEM FLUSH LOGS`)

	readRows := func(id string) uint64 {
		t.Helper()
		var rr uint64
		if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
			WHERE type = 'QueryFinish' AND event_time >= ? AND query_id = ?
			ORDER BY event_time_microseconds DESC LIMIT 1`, t0, id).Scan(&rr); err != nil {
			t.Fatalf("query_log (%s): %v", id, err)
		}
		return rr
	}
	legacyRead, readerRead := readRows(legacyID), readRows(readerID)
	t.Logf("read_rows: legacy=%d reader=%d (fixture: %d in-window writes to ONE reserve key, %d keys probed)",
		legacyRead, readerRead, churn, len(keys))

	if legacyRead < uint64(churn) {
		t.Fatalf("fixture no longer reproduces the pathology: the legacy shape read %d rows, expected at least the pool's %d in-window writes — the bound below would be vacuous",
			legacyRead, churn)
	}
	// The probe reads granules covering the requested keys, not the pool's
	// write history. Generous by design: what must NOT hold is the two
	// figures tracking each other.
	if readerRead*10 > legacyRead {
		t.Errorf("reader read %d rows vs legacy %d — the reserve lookup must be bounded by the KEY COUNT, not by how often the pool was written to (#504)",
			readerRead, legacyRead)
	}
}
