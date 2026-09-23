//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestSampleAccountIDs_SeededChangeLogFrame proves reconcile-balances' -sample
// frame (GH-1096) on real ClickHouse:
//
//   - it is drawn from stellar.ledger_entry_changes, so an account the
//     ledger_entries_current projection lost is still drawable (the old frame
//     read the projection and returned nothing here);
//   - one seed reproduces its cohort exactly, and different seeds draw
//     different cohorts (the old unseeded cityHash64 order froze one cohort);
//   - the floor excludes accounts at or below it.
func TestSampleAccountIDs_SeededChangeLogFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Far above any other fixture so the floor isolates this test's rows;
	// purged afterwards because max(ledger_seq) bounds every whole-lake walk.
	const (
		below     = uint32(4_000_001_000)
		floor     = uint32(4_000_001_050)
		above     = uint32(4_000_001_100)
		accounts  = 40
		cohort    = 8
		belowAcct = "gh1096-sample-frame-below-floor"
	)
	purgeLakeFixtureLedgers(t, addr, below, above)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	want := make([]string, 0, accounts)
	rows := make([]chstore.LedgerEntryChangeRow, 0, accounts+1)
	for i := range uint32(accounts) {
		acct := fmt.Sprintf("gh1096-sample-frame-%02d", i)
		want = append(want, acct)
		rows = append(rows, chstore.LedgerEntryChangeRow{
			LedgerSeq: above, CloseTime: closeTime, TxHash: "gh1096", ChangeIndex: i,
			IntraLedgerSeq: i, ChangeType: "updated", EntryType: "account",
			KeyXDR: "gh1096-key-" + acct, AccountID: acct, Balance: int64(i),
		})
	}
	rows = append(rows, chstore.LedgerEntryChangeRow{
		LedgerSeq: below, CloseTime: closeTime, TxHash: "gh1096-below", ChangeType: "updated", EntryType: "account",
		KeyXDR: "gh1096-key-" + belowAcct, AccountID: belowAcct, Balance: 1,
	})
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// Simulate a projection that lost these accounts: the change log still
	// holds them, the current-state view does not.
	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, `ALTER TABLE stellar.ledger_entries_current DELETE
		WHERE startsWith(account_id, 'gh1096-sample-frame-') SETTINGS mutations_sync = 2`); err != nil {
		t.Fatalf("drop fixture accounts from ledger_entries_current: %v", err)
	}

	all, err := chstore.SampleAccountIDs(ctx, addr, floor, 1, accounts+10)
	if err != nil {
		t.Fatalf("SampleAccountIDs(all): %v", err)
	}
	slices.Sort(all)
	if !slices.Equal(all, want) {
		t.Fatalf("frame above floor = %v, want the %d change-log accounts (none below the floor, none lost with the projection)", all, accounts)
	}

	draw := func(seed uint64) []string {
		t.Helper()
		ids, err := chstore.SampleAccountIDs(ctx, addr, floor, seed, cohort)
		if err != nil {
			t.Fatalf("SampleAccountIDs(seed %d): %v", seed, err)
		}
		if len(ids) != cohort {
			t.Fatalf("SampleAccountIDs(seed %d) = %d ids, want %d", seed, len(ids), cohort)
		}
		return ids
	}
	first := draw(1)
	if again := draw(1); !slices.Equal(first, again) {
		t.Errorf("seed 1 drew %v then %v; one seed must reproduce its cohort", first, again)
	}
	distinct := 1
	for seed := uint64(2); seed <= 4; seed++ {
		if !slices.Equal(draw(seed), first) {
			distinct++
		}
	}
	if distinct == 1 {
		t.Errorf("seeds 1..4 all drew cohort %v; the seed does not rotate the sample", first)
	}
}
