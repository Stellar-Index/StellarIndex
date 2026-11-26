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

// A derive_rev 4 writer against a table without interest_drawn / gulped / reconcile_backstop_loss fails whole and
// writes nothing, as does the proof's read; lending_interest.sql adds them. Then the per-cause split round-trips
// exactly at Int256 width, FINAL keeps the newer derive of an hour, and a revision-3 row reads back as revision 3
// with zero causes, so the proof's derive_rev filter reports it INCOMPLETE rather than comparing it.
func TestLendingInterestHourly_CauseColumnsRequiredAndRoundTrip(t *testing.T) {
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
		`ALTER TABLE stellar.lending_interest_hourly DROP COLUMN reconcile_backstop_loss, DROP COLUMN gulped, DROP COLUMN interest_drawn`,
	} {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	const pool, asset = "CPOOLCAUSE", "CASSETCAUSE"
	hour := time.Date(2027, 5, 1, 3, 0, 0, 0, time.UTC)
	n := func(v int64) *big.Int { return big.NewInt(v) }
	// Past Int128: an hourly sum of i128 lots needs Int256's headroom.
	drawn := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 130))
	gulped, reconciled := n(4), n(-2)
	explained := new(big.Int).Add(new(big.Int).Add(drawn, gulped), reconciled)
	row := lendinginterest.HourRow{
		Pool: pool, Asset: asset, Protocol: lendinginterest.BLNT, Hour: hour, FirstLedger: 10, LastLedger: 12, Versions: 3,
		AccruedVersions: 3, Gross: n(5), ProtocolFee: n(0), Backstop: n(1), SupplierIntended: n(4), SupplierRealised: n(4),
		BackstopCreditDelta: n(1), UnreconciledVersions: 3, BackstopResidual: explained, SupplyResidual: n(0),
		DefaultLoss: n(0), FeeCreditResidual: n(0), ResidualExplained: explained, ResidualAttributed: n(0), ExplainedVersions: 3,
		InterestDrawn: drawn, Gulped: gulped, ReconcileBackstopLoss: reconciled,
	}
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{row}, 4); err == nil {
		t.Fatal("insert into a table without the cause columns succeeded")
	}
	if _, err := chstore.LendingInterestStoredRows(ctx, addr, []string{pool}, hour, hour.Add(time.Hour)); err == nil {
		t.Fatal("the proof's read of a table without the cause columns succeeded")
	}
	var count uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.lending_interest_hourly FINAL`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("%d rows written by the failed insert (%v)", count, err)
	}
	apply()

	// The same hour derived twice, revision 3 first: FINAL keeps the newer derive.
	old := row
	old.InterestDrawn, old.Gulped, old.ReconcileBackstopLoss = n(0), n(0), n(0)
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{old}, 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // derived_at is ms: the second derive must be strictly newer
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{row}, 4); err != nil {
		t.Fatal(err)
	}
	// A revision-3 row of the next hour, as an older binary left it.
	stale := old
	stale.Hour = hour.Add(time.Hour)
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{stale}, 3); err != nil {
		t.Fatal(err)
	}
	got, err := chstore.LendingInterestStoredRows(ctx, addr, []string{pool}, hour, hour.Add(2*time.Hour))
	if err != nil || len(got) != 2 {
		t.Fatalf("%d rows, %v", len(got), err)
	}
	g := got[0]
	if g.DeriveRev != 4 || g.Row.InterestDrawn.Cmp(drawn) != 0 || g.Row.Gulped.Cmp(gulped) != 0 ||
		g.Row.ReconcileBackstopLoss.Cmp(reconciled) != 0 || g.ResidualExplained.Cmp(explained) != 0 {
		t.Fatalf("hour %s: derive_rev %d, by cause %s + %s + %s, residual_explained %s; want rev 4, %s + %s + %s = %s",
			g.Row.Hour, g.DeriveRev, g.Row.InterestDrawn, g.Row.Gulped, g.Row.ReconcileBackstopLoss, g.ResidualExplained,
			drawn, gulped, reconciled, explained)
	}
	if s := got[1]; s.DeriveRev != 3 || s.Row.InterestDrawn.Sign() != 0 || s.Row.Gulped.Sign() != 0 || s.Row.ReconcileBackstopLoss.Sign() != 0 {
		t.Fatalf("stale hour: derive_rev %d, by cause %s %s %s", s.DeriveRev, s.Row.InterestDrawn, s.Row.Gulped, s.Row.ReconcileBackstopLoss)
	}
	// The served-tier read shape: FINAL and the current revision only.
	var sum string
	if err := conn.QueryRow(ctx, `SELECT toString(sum(interest_drawn)) FROM stellar.lending_interest_hourly FINAL
		WHERE contract_id = ? AND derive_rev = 4`, pool).Scan(&sum); err != nil || sum != drawn.String() {
		t.Fatalf("Σ interest_drawn at derive_rev 4 = %s (%v), want %s", sum, err, drawn)
	}
	ids, err := chstore.LendingInterestRowsBeyond(ctx, addr, []string{pool}, hour)
	if err != nil || len(ids) != 1 || ids[0].DeriveRev != 3 {
		t.Fatalf("rows beyond %s: %+v, %v", hour, ids, err)
	}
}
