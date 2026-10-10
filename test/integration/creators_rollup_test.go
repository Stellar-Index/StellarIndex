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

// TestCreatorsRollup_BoundaryIsTheNetworks is the test nets' empty-`created`
// cohort proof, run through the real cycle on a real
// ClickHouse. A creation on a post-P23-only chain is recorded ONE way: a
// CAP-67 `transfer` movement paired with a CreateAccount operation. The
// cycle's post-P23 arm reads exactly that pair — but only for ledgers at or
// above its boundary, which must be the network's, not pubnet's constant baked
// into the SQL. Every test-net ledger sits below 58,762,517, so with the pubnet
// constant the classic arm owns all of them and looks for `create_account`
// movements that chain never writes: zero account_creator_edges rows, zero
// `created` cohorts, however full the archive.
//
// Fixture: one CreateAccount operation and its funding transfer at a low
// ledger. The same fixture is rolled up twice — at the pubnet boundary
// (the wrong boundary on a test net: no edge) and at the chain's start
// (the network's boundary: one edge) — so the test pins the substitution,
// not merely that the SQL runs.
func TestCreatorsRollup_BoundaryIsTheNetworks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// A low ledger, as every ledger on a reset test net is. Distinct from
	// the first-run watermark test's [2, 6] so neither disturbs the other's
	// contiguity or lake-min expectations.
	const (
		ledger = uint32(40)
		txHash = "creatorsboundaryaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	creator := gAccountFromSeed(t, 0x31)
	created := gAccountFromSeed(t, 0x32)
	closeTime := time.Date(2027, 9, 1, 0, 0, 40, 0, time.UTC)

	if err := chstore.EnsureAccountMovementsTable(ctx, addr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}

	// The operation, as the indexer's extract lands it: the ledger row (the
	// per-ledger commit marker the walk's tip is read from) plus the
	// CreateAccount operation. The operations side contributes the join
	// key only, so no body is needed.
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	if err := sink.Add(ctx, chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime,
			LedgerHash: "aa03", PrevHash: "bb03", ProtocolVersion: 23, BucketListHash: "cc03",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
		Ops: []chstore.OperationRow{{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, TxIndex: 0, OpIndex: 0,
			OpType: "OperationTypeCreateAccount", SourceAccount: creator,
		}},
	}); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	// The funding leg, as ch-cap67-movements derives it from the CAP-67
	// transfer event: a `transfer` movement from the creator to the new
	// account at the same (ledger, tx_hash, op_index).
	if _, err := chstore.InsertAccountMovements(ctx, addr, []chstore.AccountMovement{{
		MovementKind:    "transfer",
		Provenance:      chstore.ProvenanceCAP67Derived,
		Ledger:          ledger,
		LedgerCloseTime: closeTime,
		TxHash:          txHash,
		OpIndex:         0,
		LegIndex:        0,
		Asset:           "native",
		Amount:          big.NewInt(100_000_000),
		FromAddress:     creator,
		ToAddress:       created,
	}}); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// edgesAt runs one full cycle at the given boundary and reads back the
	// served edge for this creator → created pair: (rows, creations).
	edgesAt := func(boundary uint32) (uint64, uint64) {
		t.Helper()
		if err := chstore.RunCreatorsRollup(ctx, addr, boundary, t.Logf); err != nil {
			t.Fatalf("RunCreatorsRollup(boundary=%d): %v", boundary, err)
		}
		var rows, creations uint64
		const q = `SELECT toUInt64(count()), toUInt64(sum(creations))
			FROM stellar.account_creator_edges
			WHERE creator = ? AND created = ?`
		if err := conn.QueryRow(ctx, q, creator, created).Scan(&rows, &creations); err != nil {
			t.Fatalf("read account_creator_edges (boundary=%d): %v", boundary, err)
		}
		return rows, creations
	}

	// Pubnet's boundary on a chain whose every ledger sits below it: the
	// classic arm owns ledger 40 and finds no create_account movement —
	// the test-net symptom, pinned so the substitution below is
	// shown to be what changes the outcome.
	if rows, _ := edgesAt(chstore.P23BoundaryLedger); rows != 0 {
		t.Fatalf("boundary %d: %d edge rows for a post-P23 creation below the boundary, want 0 "+
			"(the classic arm cannot see a CAP-67 transfer; if it now does, the arms overlap)",
			chstore.P23BoundaryLedger, rows)
	}

	// The network's boundary — the chain's start: the post-P23 arm owns
	// ledger 40, pairs the transfer with the CreateAccount operation, and
	// the creation reaches the served edge table exactly once.
	rows, creations := edgesAt(1)
	if rows != 1 || creations != 1 {
		t.Fatalf("boundary 1: account_creator_edges has %d row(s) / %d creation(s) for the pair, want 1 / 1 — "+
			"the post-P23 arm must own every ledger of a post-P23-only chain", rows, creations)
	}
}

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

// TestCreatorsRollup_RecycledAddressCountsOnce is the recycled-address proof
// against real ClickHouse. stellar.account_creators_ops is one row
// per creation OPERATION, so one creator recycling one address (create ->
// merge -> create) produces two rows sharing the same `created`. Joining
// the board's live_accounts/live_stroops live-entry set onto that per-event
// table directly counts the still-live address TWICE and sums its balance
// TWICE, so the board aggregates over the distinct
// (creator, created) pair before the outer sum.
//
// Fixture: one creator, one address created by two distinct operations,
// live with a single known balance. The served board row must show
// live_accounts = 1 and live_stroops = that one balance, not 2x.
func TestCreatorsRollup_RecycledAddressCountsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger    = uint32(41)
		txHashOne = "recycledaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"
		txHashTwo = "recycledaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2"
		balance   = int64(100_000_000) // one XLM's worth of stroops, the survivor's true balance
	)
	creator := gAccountFromSeed(t, 0x41)
	created := gAccountFromSeed(t, 0x42)
	closeTime := time.Date(2027, 9, 2, 0, 0, 41, 0, time.UTC)

	if err := chstore.EnsureAccountMovementsTable(ctx, addr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	if err := sink.Add(ctx, chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime,
			LedgerHash: "aa04", PrevHash: "bb04", ProtocolVersion: 23, BucketListHash: "cc04",
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
		Ops: []chstore.OperationRow{
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashOne, TxIndex: 0, OpIndex: 0,
				OpType: "OperationTypeCreateAccount", SourceAccount: creator,
			},
			{
				LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashTwo, TxIndex: 1, OpIndex: 0,
				OpType: "OperationTypeCreateAccount", SourceAccount: creator,
			},
		},
	}); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	// Two funding legs for the SAME (creator, created) pair — the recycle:
	// created, presumably merged away, created again by the same funder.
	if _, err := chstore.InsertAccountMovements(ctx, addr, []chstore.AccountMovement{
		{
			MovementKind:    "transfer",
			Provenance:      chstore.ProvenanceCAP67Derived,
			Ledger:          ledger,
			LedgerCloseTime: closeTime,
			TxHash:          txHashOne,
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(50_000_000),
			FromAddress:     creator,
			ToAddress:       created,
		},
		{
			MovementKind:    "transfer",
			Provenance:      chstore.ProvenanceCAP67Derived,
			Ledger:          ledger,
			LedgerCloseTime: closeTime,
			TxHash:          txHashTwo,
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(50_000_000),
			FromAddress:     creator,
			ToAddress:       created,
		},
	}); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	// The address's current live state: one account entry, one balance.
	if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHashTwo, OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "account",
			KeyXDR: "recycled-account-key-" + created, EntryXDR: "recycled-account-entry",
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

	var (
		accountsCreated uint64
		liveAccounts    uint64
		liveStroops     big.Int
	)
	const q = `SELECT accounts_created, live_accounts, live_stroops
		FROM stellar.account_creators_rollup
		WHERE creator = ?`
	if err := conn.QueryRow(ctx, q, creator).Scan(&accountsCreated, &liveAccounts, &liveStroops); err != nil {
		t.Fatalf("read account_creators_rollup: %v", err)
	}

	// accounts_created is immutable history and stays per-event: two
	// creation operations, so two.
	if accountsCreated != 2 {
		t.Errorf("accounts_created = %d, want 2 (immutable per-event history)", accountsCreated)
	}
	// live_accounts/live_stroops describe the SURVIVING set: one address,
	// its one true balance — not doubled by the two creation events that
	// produced it.
	if liveAccounts != 1 {
		t.Errorf("live_accounts = %d, want 1 (one surviving address recycled twice, not 2)", liveAccounts)
	}
	if liveStroops.Cmp(big.NewInt(balance)) != 0 {
		t.Errorf("live_stroops = %s, want %d (the address's one true balance, not summed once per creation event)", liveStroops.String(), balance)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	hit, ok, err := reader.AccountCreators(ctx, 10, creator)
	if err != nil || !ok || len(hit.Board) != 1 {
		t.Fatalf("AccountCreators(keyed hit) = %d rows, ok %v, err %v", len(hit.Board), ok, err)
	}
	// A keyed miss still reports the cycle's time, never the zero time.
	miss, ok, err := reader.AccountCreators(ctx, 10, gAccountFromSeed(t, 0x7e))
	if err != nil || !ok {
		t.Fatalf("AccountCreators(keyed miss) = ok %v, err %v", ok, err)
	}
	if len(miss.Board) != 0 {
		t.Fatalf("keyed miss Board = %+v, want empty", miss.Board)
	}
	assertCycleTime(t, miss.ComputedAt, hit.ComputedAt)
}
