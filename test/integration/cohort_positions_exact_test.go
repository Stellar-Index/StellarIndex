//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
