//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseCohortHoldingsSumPastInt64 runs one cohort cycle over two
// members each holding 2^62 stroops of one classic asset. The cohort's
// holding is 2^63 — one past Int64's maximum — and must be served exactly;
// summing the Int64 balance column before widening wraps it negative.
func TestClickHouseCohortHoldingsSumPastInt64(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		root  = "GTEST_COHORT_WIDE_SPONSOR_AAAAAAAAAAAAAAAAAAAAAAAAAA"
		asset = "FAKE-GTEST_COHORT_WIDE_ISSUER_AAAAAAAAAAAAAAAAAAAAAAAAA"
		ldg   = uint32(77_800_001)
	)
	members := []string{
		"GTEST_COHORT_WIDE_MEMBER1_AAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GTEST_COHORT_WIDE_MEMBER2_AAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	at := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	const half = int64(1) << 62

	if err := raw.Exec(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)
		VALUES (?, ?, ?, '00', 23)`, ldg, at, fmt.Sprintf("%064d", ldg)); err != nil {
		t.Fatalf("insert ledger: %v", err)
	}
	for i, m := range members {
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_sponsor_edges
			(sponsor, sponsored, sponsorships_started, first_ledger, last_ledger, first_at, last_at)
			VALUES (?, ?, 1, ?, ?, ?, ?)`, root, m, ldg, ldg, at, at); err != nil {
			t.Fatalf("insert sponsor edge: %v", err)
		}
		if err := raw.Exec(ctx, `INSERT INTO stellar.ledger_entries_current
			(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)
			VALUES ('trustline', ?, ?, ?, ?, 'updated', ?, ?, '')`,
			fmt.Sprintf("wide-trustline-%d", i), m, asset, half, ldg, at); err != nil {
			t.Fatalf("insert trustline: %v", err)
		}
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
	want := new(big.Int).Lsh(big.NewInt(1), 63)
	for _, h := range cohort.Holdings {
		if h.Asset != asset {
			continue
		}
		if h.Holders != 2 || h.Balance.Cmp(want) != 0 {
			t.Fatalf("holding %s = (%d holders, %s), want (2, %s)", asset, h.Holders, h.Balance, want)
		}
		return
	}
	t.Fatalf("no holding for %s: %+v", asset, cohort.Holdings)
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
