//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseAccountsByWealthNativeExact executes accountsByWealthQuery on
// the native_xlm basis over two accounts whose balances (2^62+1 and 2^62
// stroops) are indistinguishable as float64. The reader must return each
// balance exactly in NativeStroops and rank the larger one first — the
// native_stroops tiebreak, since the float ranking key ties.
func TestClickHouseAccountsByWealthNativeExact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		bigger  = "GTEST_WEALTH_EXACT_BIGGER_AAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		smaller = "GTEST_WEALTH_EXACT_SMALLER_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		ledger  = uint32(77_900_001)
	)
	balances := map[string]int64{bigger: 1<<62 + 1, smaller: 1 << 62}
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare ledger_entries_current: %v", err)
	}
	// Insert the smaller first so insertion order cannot pass for ranking.
	for _, acct := range []string{smaller, bigger} {
		if err := b.Append("account", "wealth-exact-"+acct, acct, "", balances[acct],
			"updated", ledger, at, ""); err != nil {
			t.Fatalf("append account: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send accounts: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	rows, err := er.AccountsByWealth(ctx, []string{"native"}, []float64{1.0}, 500)
	if err != nil {
		t.Fatalf("AccountsByWealth: %v", err)
	}

	pos := map[string]int{}
	for i, w := range rows {
		want, ours := balances[w.AccountID]
		if !ours {
			continue
		}
		pos[w.AccountID] = i
		if got := w.NativeStroops.BigInt(); !got.IsInt64() || got.Int64() != want {
			t.Errorf("%s NativeStroops = %s, want exactly %d", w.AccountID, got, want)
		}
	}
	if len(pos) != 2 {
		t.Fatalf("ranking returned %d of the 2 seeded accounts (%d rows)", len(pos), len(rows))
	}
	if pos[bigger] > pos[smaller] {
		t.Errorf("bigger balance ranked at %d, after smaller at %d; native_stroops must break the float tie",
			pos[bigger], pos[smaller])
	}
}
