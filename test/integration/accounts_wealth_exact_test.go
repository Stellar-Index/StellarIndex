//go:build integration

package integration_test

import (
	"context"
	"math/big"
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
	rows, err := er.AccountsByWealth(ctx, []string{"native"}, []string{"1"}, 500)
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

// TestClickHouseAccountsByWealthUSDExact executes accountsByWealthQuery on the
// usd basis: 10 XLM at 0.1 plus 3 TOK at 0.333333333333333333 is exactly
// 1.999999999999999999 dollars. A float64 sum returns 2 (or 1.9999999999999998),
// a rounded number the endpoint would serve as an exact decimal string.
func TestClickHouseAccountsByWealthUSDExact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		holder = "GTEST_WEALTH_USD_EXACT_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		tok    = "TOK-GTEST_WEALTH_USD_EXACT_ISSUER_AAAAAAAAAAAAAAAAAAAAAA"
		ledger = uint32(77_900_002)
	)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare ledger_entries_current: %v", err)
	}
	if err := b.Append("account", "wealth-usd-exact-acct", holder, "", int64(100_000_000),
		"updated", ledger, at, ""); err != nil {
		t.Fatalf("append account: %v", err)
	}
	if err := b.Append("trustline", "wealth-usd-exact-tl", holder, tok, int64(30_000_000),
		"updated", ledger, at, ""); err != nil {
		t.Fatalf("append trustline: %v", err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send entries: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	rows, err := er.AccountsByWealth(ctx, []string{"native", tok}, []string{"0.1", "0.333333333333333333"}, 500)
	if err != nil {
		t.Fatalf("AccountsByWealth: %v", err)
	}
	want, _ := new(big.Rat).SetString("1.999999999999999999")
	for _, w := range rows {
		if w.AccountID != holder {
			continue
		}
		if w.Value == nil || w.Value.Cmp(want) != 0 {
			t.Fatalf("%s value = %v, want exactly %s", holder, w.Value, want.FloatString(18))
		}
		return
	}
	t.Fatalf("ranking returned %d rows without the seeded holder", len(rows))
}
