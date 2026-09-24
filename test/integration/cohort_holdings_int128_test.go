//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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
