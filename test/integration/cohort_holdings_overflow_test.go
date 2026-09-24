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
