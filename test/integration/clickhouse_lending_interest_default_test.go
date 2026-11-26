//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// A derive_rev 3 writer against a table without default_loss / default_versions fails whole and writes nothing,
// as does the proof's read; applying lending_interest.sql adds both, and the row then round-trips them exactly.
func TestLendingInterestHourly_DefaultColumnsRequiredAndHealedByTheOperatorFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	apply := func() {
		t.Helper()
		stmts, err := clickHouseDeployStatements("lending_interest.sql")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if err := conn.Exec(ctx, s); err != nil {
				t.Fatalf("apply lending_interest.sql: %.72q: %v", s, err)
			}
		}
	}
	apply()
	defer apply() // before cancel: later tests need the columns back
	for _, s := range []string{
		`TRUNCATE TABLE stellar.lending_interest_hourly`,
		`ALTER TABLE stellar.lending_interest_hourly DROP COLUMN default_versions, DROP COLUMN default_loss`,
	} {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	const pool, asset = "CPOOLDEFAULT", "CASSETDEFAULT"
	hour := time.Date(2027, 4, 1, 3, 0, 0, 0, time.UTC)
	n := func(v int64) *big.Int { return big.NewInt(v) }
	row := lendinginterest.HourRow{
		Pool: pool, Asset: asset, Protocol: lendinginterest.BlendV2, Hour: hour, FirstLedger: 10, LastLedger: 10, Versions: 1,
		AccruedVersions: 1, Gross: n(5), ProtocolFee: n(0), Backstop: n(1), SupplierIntended: n(4), SupplierRealised: n(4),
		BackstopCreditDelta: n(1), UnreconciledVersions: 1, BRateMismatches: 1, BackstopResidual: n(0),
		SupplyResidual: n(-592388690912445), DefaultLoss: n(-592388690912445), DefaultVersions: 1, FeeCreditResidual: n(0),
		ResidualExplained: n(0), ResidualAttributed: n(0), ExplainedVersions: 1,
	}
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{row}, 3); err == nil {
		t.Fatal("insert into a table without the default columns succeeded")
	}
	if _, err := chstore.LendingInterestStoredRows(ctx, addr, []string{pool}, hour, hour.Add(time.Hour)); err == nil {
		t.Fatal("the proof's read of a table without the default columns succeeded")
	}
	var count uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.lending_interest_hourly FINAL`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("%d rows written by the failed insert (%v)", count, err)
	}
	apply()
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{row}, 3); err != nil {
		t.Fatal(err)
	}
	got, err := chstore.LendingInterestStoredRows(ctx, addr, []string{pool}, hour, hour.Add(time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("%d rows, %v", len(got), err)
	}
	if g := got[0].Row; g.DefaultLoss.Cmp(row.DefaultLoss) != 0 || g.DefaultVersions != 1 || g.SupplyResidual.Cmp(row.SupplyResidual) != 0 {
		t.Fatalf("stored default loss %s over %d, supply residual %s", g.DefaultLoss, g.DefaultVersions, g.SupplyResidual)
	}
}
