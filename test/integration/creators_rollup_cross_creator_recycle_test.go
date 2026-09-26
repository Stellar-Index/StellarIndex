//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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
