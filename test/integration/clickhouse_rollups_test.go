//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stellar/go-stellar-sdk/xdr"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
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

// TestHoldersRollupFreshness_ExecutesAgainstServer runs the real
// ch-holders-rollup cycle, then ages its stamp, against a real ClickHouse
// server. It proves: the writer's UTC-pinned cycle stamp round-trips
// as the true instant; a fresh cycle is served from the rollup, including
// the live-table stamp read for an asset absent from it; and once the cycle
// is older than the reader's max age, AssetHolders stops serving the rollup
// and answers from the live per-request scans.
func TestHoldersRollupFreshness_ExecutesAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	// Other tests in this package read AssetHolders expecting the legacy
	// path; leave the rollup as unpopulated as this test found it.
	t.Cleanup(func() {
		for _, table := range []string{
			"asset_holders_rollup", "asset_holders_counts", "accounts_stats",
			"accounts_wealth_histogram", "accounts_trustline_histogram",
			"asset_stats_daily", "asset_stats_daily_staging",
		} {
			_ = conn.Exec(context.Background(), "TRUNCATE TABLE stellar."+table)
		}
	})

	const listedAsset, lateAsset = "T390L-GISSUERT390", "T390N-GISSUERT390"
	seedEntry := func(entryType, asset, holder string, ledger uint32) {
		t.Helper()
		row := chstore.LedgerEntryChangeRow{
			LedgerSeq: ledger, CloseTime: time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC),
			TxHash: "t390", IntraLedgerSeq: 1, ChangeType: "created", EntryType: entryType,
			KeyXDR: "t390-" + entryType + "-" + holder + asset, EntryXDR: "t390", AccountID: holder,
			Asset: asset, Balance: 42,
		}
		if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
			t.Fatalf("InsertEntryChanges(%s %s): %v", entryType, asset, err)
		}
	}

	// The cycle's accounts_stats arm needs at least one funded account
	// (avg() over none is NaN, which toInt64 rejects).
	seedEntry("account", "", "GHOLDERT390L", 72_000_001)
	seedEntry("trustline", listedAsset, "GHOLDERT390L", 72_000_001)
	if err := chstore.RunHoldersRollup(ctx, addr, t.Logf); err != nil {
		t.Fatalf("RunHoldersRollup: %v", err)
	}
	// Issued after the cycle: absent from the rollup, present in the lake.
	seedEntry("trustline", lateAsset, "GHOLDERT390N", 72_000_002)

	var stamp time.Time
	if err := conn.QueryRow(ctx, `SELECT max(computed_at) FROM stellar.asset_holders_rollup`).Scan(&stamp); err != nil {
		t.Fatalf("read cycle stamp: %v", err)
	}
	if age := time.Since(stamp); age < -time.Minute || age > 5*time.Minute {
		t.Fatalf("cycle stamp %s is %s from now — the writer's stamp did not round-trip as the true instant", stamp, age)
	}

	holders := func(asset string) int64 {
		t.Helper()
		r, err := chstore.NewExplorerReader(ctx, addr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		_, total, err := r.AssetHolders(ctx, asset, 5)
		if err != nil {
			t.Fatalf("AssetHolders(%s): %v", asset, err)
		}
		return total
	}

	if got := holders(listedAsset); got != 1 {
		t.Errorf("fresh cycle, listed asset: total = %d, want 1", got)
	}
	// Fresh cycle: the rollup is authoritative, so the late asset reads as
	// zero holders — the live scan would have found one.
	if got := holders(lateAsset); got != 0 {
		t.Errorf("fresh cycle, late asset: total = %d, want 0 served from the rollup", got)
	}

	for _, table := range []string{"asset_holders_rollup", "asset_holders_counts"} {
		if err := conn.Exec(ctx, "ALTER TABLE stellar."+table+
			" UPDATE computed_at = computed_at - INTERVAL 3 HOUR WHERE 1 SETTINGS mutations_sync = 1"); err != nil {
			t.Fatalf("age %s: %v", table, err)
		}
	}
	// Stale cycle: the rollup must no longer answer; the live scan finds the
	// late asset's holder.
	if got := holders(lateAsset); got != 1 {
		t.Errorf("stale cycle, late asset: total = %d, want 1 from the live scan — a 3h-old rollup was served as current", got)
	}
}

// TestClickHouseMovementsByAsset pins the movements_by_asset table: the MV copies live
// account_movements rows, both participants of one movement survive FINAL,
// an Int128 amount above 2^64 stays exact, and the operator catch-up
// statement (partition-bounded INSERT..SELECT) is idempotent over MV rows and skips superseded source rows (FINAL).
func TestClickHouseMovementsByAsset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		asset  = "TESTA-GISSUERMOVEMENTSBYASSET"
		other  = "TESTB-GISSUERMOVEMENTSBYASSET"
		ledger = uint32(78_000_001)
		tx     = "mba-tx-1"
	)
	big128 := new(big.Int).Lsh(big.NewInt(1), 100)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.account_movements
		(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
		 movement_kind, provenance, asset, counterparty, amount)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	rows := [][]any{
		{"GSENDER", ledger, at, tx, uint32(0), uint32(0), "sent", "payment", "classic", asset, "GRECV", big128},
		{"GRECV", ledger, at, tx, uint32(0), uint32(0), "received", "payment", "classic", asset, "GSENDER", big128},
		{"GSENDER", ledger, at, "mba-tx-2", uint32(0), uint32(0), "sent", "payment", "classic", other, "GRECV", big.NewInt(5)},
	}
	for _, r := range rows {
		if err := b.Append(r...); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}

	count := func(t *testing.T) (n uint64, sum string) {
		t.Helper()
		if err := raw.QueryRow(ctx, `SELECT count(), toString(sum(amount)) FROM stellar.movements_by_asset FINAL
			WHERE asset = ? AND ledger = ?`, asset, ledger).Scan(&n, &sum); err != nil {
			t.Fatalf("query: %v", err)
		}
		return n, sum
	}
	want := new(big.Int).Mul(big128, big.NewInt(2)).String()
	if n, sum := count(t); n != 2 || sum != want {
		t.Fatalf("after MV: n=%d sum=%s, want 2 rows summing %s", n, sum, want)
	}

	const catchUp = `INSERT INTO stellar.movements_by_asset
		SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
		       movement_kind, provenance, asset, counterparty, amount, attributes, ingested_at
		FROM stellar.account_movements FINAL
		WHERE ledger >= 78000000 AND ledger < 79000000`
	if err := raw.Exec(ctx, catchUp); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	if n, sum := count(t); n != 2 || sum != want {
		t.Fatalf("after catch-up: n=%d sum=%s, want 2 rows summing %s", n, sum, want)
	}

	// A superseded source row (same key, older ingested_at, different asset)
	// is still unmerged in account_movements; FINAL must keep it out.
	stale := "TESTSTALE-GISSUERMOVEMENTSBYASSET"
	if err := raw.Exec(ctx, `SYSTEM STOP MERGES stellar.account_movements`); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), `SYSTEM START MERGES stellar.account_movements`) })
	// Two inserts = two parts, so the pair stays unmerged (merges are stopped).
	for _, r := range []struct{ asset, ts string }{{stale, "2026-09-01 00:00:00"}, {asset, "2026-09-02 00:00:00"}} {
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_movements
			(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
			 movement_kind, provenance, asset, counterparty, amount, ingested_at)
			VALUES ('GSTALE', 78000002, ?, 'mba-tx-stale', 0, 0, 'sent', 'payment', 'classic', ?, '', 1, ?)`,
			at, r.asset, r.ts); err != nil {
			t.Fatalf("insert stale pair: %v", err)
		}
	}
	if err := raw.Exec(ctx, `DELETE FROM stellar.movements_by_asset WHERE ledger = 78000002 SETTINGS mutations_sync = 2`); err != nil {
		t.Fatalf("clear MV rows: %v", err)
	}
	if err := raw.Exec(ctx, catchUp); err != nil {
		t.Fatalf("catch-up 2: %v", err)
	}
	var n uint64
	if err := raw.QueryRow(ctx, `SELECT count() FROM stellar.movements_by_asset WHERE ledger = 78000002 AND asset = ?`, stale).Scan(&n); err != nil {
		t.Fatalf("stale query: %v", err)
	}
	if n != 0 {
		t.Fatalf("stale superseded row copied by catch-up: %d rows under asset %s", n, stale)
	}
}

// TestClickHouseAssetMovementsReader pins the A1 reader and its route: one
// entry per movement rebuilt from either participant row, newest re-derive
// wins, keyset paging, the ledger ceiling, and the backfill marker.
func TestClickHouseAssetMovementsReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dialClickHouse(t, ctx, "stellar")
	const (
		issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		asset  = "MBAREAD-" + issuer
		l      = uint32(50_100_001) // below P23: admitted by the ceiling without a cap67 watermark
	)
	big128 := new(big.Int).Lsh(big.NewInt(1), 100)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	insert := func(address string, ledger uint32, tx, dir, cp string, amt *big.Int, ingested string) {
		t.Helper()
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_movements
			(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
			 movement_kind, provenance, asset, counterparty, amount, ingested_at)
			VALUES (?, ?, ?, ?, 0, 0, ?, 'payment', 'classic_derived', ?, ?, ?, ?)`,
			address, ledger, at, tx, dir, asset, cp, amt, ingested); err != nil {
			t.Fatalf("insert %s/%s: %v", tx, address, err)
		}
	}
	// Movement A: a stale first derive under the wrong sender, then the re-derive.
	insert("GSTALE", l, "mba-a", "sent", "GB", big128, "2026-09-01 00:00:00")
	insert("GA", l, "mba-a", "sent", "GB", big128, "2026-09-02 00:00:00")
	insert("GB", l, "mba-a", "received", "GA", big128, "2026-09-02 00:00:00")
	insert("GSELF", l+1, "mba-b", "self", "", big.NewInt(7), "2026-09-02 00:00:00")
	insert("GCLAIM", l+2, "mba-c", "received", "", big.NewInt(9), "2026-09-02 00:00:00")
	insert("GTOP", l+3, "mba-d", "sent", "GB", big.NewInt(1), "2026-09-02 00:00:00")

	er, err := chstore.NewExplorerReader(ctx, clickhouseAddr(t))
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	page1, err := er.AssetMovements(ctx, asset, 2, chstore.AccountMovementCursor{}, l+2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 2 || page1[0].TxHash != "mba-c" || page1[1].TxHash != "mba-b" {
		t.Fatalf("page 1 = %+v, want mba-c, mba-b (mba-d is above the ceiling)", page1)
	}
	if page1[0].From != "" || page1[0].To != "GCLAIM" || page1[1].From != "GSELF" || page1[1].To != "GSELF" {
		t.Fatalf("page 1 sides = %+v", page1)
	}
	last := page1[1]
	page2, err := er.AssetMovements(ctx, asset, 2, chstore.AccountMovementCursor{
		Ledger: last.Ledger, TxHash: last.TxHash, OpIndex: last.OpIndex, LegIndex: last.LegIndex,
	}, l+2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 1 || page2[0].From != "GA" || page2[0].To != "GB" || page2[0].Amount.Cmp(big128) != 0 {
		t.Fatalf("page 2 = %+v, want one GA->GB movement of 2^100", page2)
	}

	if thru, err := er.AssetMovementsBackfilledThru(ctx); err != nil || thru != 0 {
		t.Fatalf("marker before write = %d, %v; want 0", thru, err)
	}
	if err := raw.Exec(ctx, `INSERT INTO stellar.cap67_movements_watermark (name, thru_ledger) VALUES (?, ?)`,
		chstore.MovementsByAssetBackfillMarker, uint32(64_000_000)); err != nil {
		t.Fatalf("marker insert: %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Exec(context.Background(), `DELETE FROM stellar.cap67_movements_watermark WHERE name = ? SETTINGS mutations_sync = 2`,
			chstore.MovementsByAssetBackfillMarker)
	})
	if thru, err := er.AssetMovementsBackfilledThru(ctx); err != nil || thru != 64_000_000 {
		t.Fatalf("marker after write = %d, %v; want 64000000", thru, err)
	}

	ts := httptest.NewServer(v1.New(v1.Options{Explorer: er}).Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/assets/MBAREAD:" + issuer + "/movements?limit=5")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Data struct {
			Asset      string `json:"asset"`
			LowerBound bool   `json:"lower_bound"`
			Movements  []struct {
				TxHash string `json:"tx_hash"`
				Amount string `json:"amount"`
			} `json:"movements"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d decode %v", resp.StatusCode, err)
	}
	if got.Data.Asset != asset || got.Data.LowerBound || len(got.Data.Movements) != 4 ||
		got.Data.Movements[3].Amount != big128.String() {
		t.Fatalf("HTTP view = %+v, want 4 movements of %s ending in the exact 2^100 amount, not a lower bound", got.Data, asset)
	}
}

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

// TestAssetStatsDaily_ExecutesAgainstServer runs the real ch-holders-rollup
// cycle twice against a real ClickHouse server and reads the day's snapshot
// back. It proves the trustline count includes zero-balance lines while
// holders do not, the balance sums stay exact past 2^63, the Gini matches a
// hand-computed value, and a second cycle on the same day replaces the day's
// rows rather than adding to them.
func TestAssetStatsDaily_ExecutesAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	t.Cleanup(func() {
		for _, table := range []string{
			"asset_holders_rollup", "asset_holders_counts", "accounts_stats",
			"accounts_wealth_histogram", "accounts_trustline_histogram",
			"asset_stats_daily", "asset_stats_daily_staging",
		} {
			_ = conn.Exec(context.Background(), "TRUNCATE TABLE stellar."+table)
		}
	})

	const spreadAsset, wideAsset = "A3AS-GISSUERA3A", "A3AW-GISSUERA3A"
	seed := func(entryType, asset, holder string, balance int64) {
		t.Helper()
		row := chstore.LedgerEntryChangeRow{
			LedgerSeq: 73_000_001, CloseTime: time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC),
			TxHash: "a3a", IntraLedgerSeq: 1, ChangeType: "created", EntryType: entryType,
			KeyXDR: "a3a-" + entryType + "-" + holder + asset, EntryXDR: "a3a", AccountID: holder,
			Asset: asset, Balance: balance,
		}
		if _, err := chstore.InsertEntryChanges(ctx, addr, []chstore.LedgerEntryChangeRow{row}, 0); err != nil {
			t.Fatalf("InsertEntryChanges(%s %s %s): %v", entryType, asset, holder, err)
		}
	}
	// The cycle's accounts_stats arm needs at least one funded account.
	seed("account", "", "GHOLDERA3A1", 42)
	seed("trustline", spreadAsset, "GHOLDERA3A1", 10)
	seed("trustline", spreadAsset, "GHOLDERA3A2", 20)
	seed("trustline", spreadAsset, "GHOLDERA3A3", 30)
	seed("trustline", spreadAsset, "GHOLDERA3A4", 0)
	seed("trustline", wideAsset, "GHOLDERA3A1", math.MaxInt64)
	seed("trustline", wideAsset, "GHOLDERA3A2", math.MaxInt64)

	for range 2 {
		if err := chstore.RunHoldersRollup(ctx, addr, t.Logf); err != nil {
			t.Fatalf("RunHoldersRollup: %v", err)
		}
	}

	type snapshot struct {
		day                  time.Time
		holders, trustlines  int64
		total, top10, top100 string
		gini                 *float64
	}
	read := func(asset string) snapshot {
		t.Helper()
		rows, err := conn.Query(ctx, `
			SELECT day, holders, trustlines, toString(balance_total), toString(top10_balance),
			       toString(top100_balance), gini
			FROM stellar.asset_stats_daily WHERE asset = ?`, asset)
		if err != nil {
			t.Fatalf("read snapshot %s: %v", asset, err)
		}
		defer func() { _ = rows.Close() }()
		var out []snapshot
		for rows.Next() {
			var s snapshot
			if err := rows.Scan(&s.day, &s.holders, &s.trustlines, &s.total, &s.top10, &s.top100, &s.gini); err != nil {
				t.Fatalf("scan snapshot %s: %v", asset, err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot rows %s: %v", asset, err)
		}
		if len(out) != 1 {
			t.Fatalf("%s has %d snapshot row(s) after two same-day cycles, want exactly 1", asset, len(out))
		}
		return out[0]
	}

	today := time.Now().UTC().Format("2006-01-02")
	spread := read(spreadAsset)
	if got := spread.day.Format("2006-01-02"); got != today {
		t.Errorf("snapshot day = %s, want the cycle's UTC day %s", got, today)
	}
	if spread.holders != 3 || spread.trustlines != 4 {
		t.Errorf("holders/trustlines = %d/%d, want 3/4 (the zero-balance line counts as a trustline only)", spread.holders, spread.trustlines)
	}
	if spread.total != "60" || spread.top10 != "60" || spread.top100 != "60" {
		t.Errorf("total/top10/top100 = %s/%s/%s, want 60/60/60", spread.total, spread.top10, spread.top100)
	}
	// Mean absolute difference over 10, 20, 30: 80 / (2 · 3² · 20) = 2/9.
	if spread.gini == nil || math.Abs(*spread.gini-2.0/9.0) > 1e-12 {
		t.Errorf("gini = %s, want 2/9", giniString(spread.gini))
	}

	wide := read(wideAsset)
	if wide.total != "18446744073709551614" || wide.top10 != "18446744073709551614" {
		t.Errorf("total/top10 = %s/%s, want 18446744073709551614 (2·(2^63−1), exact past Int64)", wide.total, wide.top10)
	}
	if wide.gini == nil || *wide.gini != 0 {
		t.Errorf("gini of two equal holders = %s, want 0", giniString(wide.gini))
	}

	native := read("native")
	if native.holders < 1 || native.trustlines < native.holders {
		t.Errorf("native holders/trustlines = %d/%d, want ≥1 and trustlines ≥ holders", native.holders, native.trustlines)
	}
}

func giniString(g *float64) string {
	if g == nil {
		return "NULL"
	}
	return strconv.FormatFloat(*g, 'g', -1, 64)
}

// TestDailySupplyFlowsPerKindSplit executes the daily supply-flows query
// against real ClickHouse: per-kind sums beside the net, i128 amounts above
// 2^63 intact, and a re-inserted flow collapsed by FINAL.
func TestDailySupplyFlowsPerKindSplit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	const contract = "CDAILYKINDSPLITXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	const insert = `INSERT INTO stellar.supply_flows
		(contract_id, ledger_seq, close_time, tx_hash, op_index, event_index, kind, amount)
		VALUES (?, ?, ?, ?, 0, ?, ?, toInt128(?))`
	day1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 3, 23, 59, 0, 0, time.UTC)
	rows := []struct {
		ledger uint32
		at     time.Time
		tx     string
		ev     uint32
		kind   string
		amount string
	}{
		{80_000_001, day1, "a1", 0, "mint", "18446744073709551621"}, // 2^64 + 5
		{80_000_001, day1, "a1", 1, "burn", "21"},
		{80_000_002, day1, "a2", 0, "clawback", "4"},
		{80_050_000, day2, "b1", 0, "mint", "100"},
		{80_050_000, day2, "b1", 0, "mint", "100"}, // same flow identity: FINAL collapses it
	}
	for _, r := range rows {
		if err := conn.Exec(ctx, insert, contract, r.ledger, r.at, r.tx, r.ev, r.kind, r.amount); err != nil {
			t.Fatalf("insert %s: %v", r.kind, err)
		}
	}

	sr, err := chstore.NewSupplyReader(ctx, addr)
	if err != nil {
		t.Fatalf("new supply reader: %v", err)
	}
	t.Cleanup(func() { _ = sr.Close() })
	got, err := sr.DailySupplyFlowsForContracts(ctx, []string{contract})
	if err != nil {
		t.Fatalf("DailySupplyFlowsForContracts: %v", err)
	}
	type day struct{ date, mint, burn, clawback, net string }
	want := []day{
		{"2026-09-01", "18446744073709551621", "21", "4", "18446744073709551596"},
		{"2026-09-03", "100", "0", "0", "100"},
	}
	wantFlows := []uint64{3, 1}
	if len(got) != len(want) {
		t.Fatalf("got %d days, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		gd := day{g.Day.Format(time.DateOnly), g.Mint.String(), g.Burn.String(), g.Clawback.String(), g.Net.String()}
		if gd != w || g.Flows != wantFlows[i] {
			t.Errorf("day %d = %+v flows=%d, want %+v flows=%d", i, gd, g.Flows, w, wantFlows[i])
		}
	}
}

// TestSupplyObservedAt_StampsLedgerCloseTimeNotWallClock is the M4-callers
// end-to-end proof against a REAL ClickHouse lake. The bug: the supply-snapshot
// ledger resolvers (internal/ops/supply/supply.go::resolveSnapshotLedger and
// cmd/stellarindex-aggregator/main.go::supplyAggregatorLedgers.LatestKnownLedger)
// stamped a snapshot's ObservedAt with time.Now().UTC() instead of the chosen
// ledger's real close time — so a re-derived HISTORICAL supply snapshot carried
// the wall-clock write-time, corrupting point-in-time supply/observation
// queries (the operator re-derives supply constantly).
//
// The fix resolves the ledger's real close_time from stellar.ledgers via
// *clickhouse.ExplorerReader.CloseTimeForLedger and stamps THAT. This test
// seeds one stellar.ledgers row whose close_time is ~2.5y stale, resolves it
// through the production reader, feeds it through the production XLM computer
// exactly as the resolver→computer path does, and asserts the snapshot's
// ObservedAt equals the seeded close time — NOT ≈now. Callers that skipped the lake
// discarded this resolved value entirely (they never read the lake), so this
// stale, non-wall-clock ObservedAt is precisely what they could not produce.
func TestSupplyObservedAt_StampsLedgerCloseTimeNotWallClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Isolated high ledger_seq + a deliberately stale close time (~2.5y before
	// this test runs) so a wall-clock stamp is unmistakable and no other test's
	// rows can collide.
	const ledger = uint32(210_000_007)
	closeTime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "a1b2", PrevHash: "c3d4",
			ProtocolVersion: 22, BucketListHash: "e5f6",
			TxCount: 1, OpCount: 1, SorobanEventCount: 0,
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		},
	}
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	// Left behind, this row owns the global max ledger_seq with a 2024 close time,
	// which empties clickhouse_storage_test's NetworkThroughput tip window.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		conn := dialClickHouse(t, cctx, "stellar")
		if err := conn.Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.ledgers DELETE
			WHERE ledger_seq = %d SETTINGS mutations_sync = 2`, ledger)); err != nil {
			t.Errorf("purge supply fixture ledger: %v", err)
		}
	})
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	// 1. The production close-time read returns the ledger's REAL close time.
	got, found, err := reader.CloseTimeForLedger(ctx, ledger)
	if err != nil {
		t.Fatalf("CloseTimeForLedger: %v", err)
	}
	if !found {
		t.Fatalf("CloseTimeForLedger(%d) found=false — seeded ledger row not read back", ledger)
	}
	if !got.Equal(closeTime) {
		t.Fatalf("CloseTimeForLedger = %v, want the seeded close time %v", got, closeTime)
	}
	if time.Since(got) < 365*24*time.Hour {
		t.Fatalf("resolved close time %v is suspiciously close to now — reader returned wall-clock, not the lake close time", got)
	}

	// 2. The resolver→computer path stamps that close time onto the snapshot's
	//    ObservedAt (the XLM total is a constant, so a nil reserve reader is
	//    fine — this mirrors internal/supply/xlm_test.go's fixture).
	computer, err := supply.NewXLMComputer(nil, nil)
	if err != nil {
		t.Fatalf("NewXLMComputer: %v", err)
	}
	snap, err := computer.Compute(ctx, ledger, got)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !snap.ObservedAt.Equal(closeTime) {
		t.Errorf("snapshot ObservedAt = %v, want the ledger close time %v (wall-clock stamp regression)", snap.ObservedAt, closeTime)
	}
	if time.Since(snap.ObservedAt) < 365*24*time.Hour {
		t.Errorf("snapshot ObservedAt %v is suspiciously close to now — the wall-clock M4-callers bug is back", snap.ObservedAt)
	}
}

// TestSampleAccountIDs_SeededChangeLogFrame proves reconcile-balances' -sample
// frame on real ClickHouse:
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

// TestDistinctTopicShapes_NonSymbolTopic0 is the proof on a real
// ClickHouse: events whose topic[0] is not a Symbol all carry an empty topic_0_sym,
// so keying on (contract, topic_0_sym) alone collapsed them into one shape and
// one exemplar. They must split on topics_xdr[1], topics_xdr[2] and arity,
// while Symbol shapes keep their (contract, topic_0_sym) identity.
func TestDistinctTopicShapes_NonSymbolTopic0(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	// A ledger range and contract nothing else in the suite writes.
	const lo, contract = uint32(145_000_000), "CGH807SHAPEFIXTURE"
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		if err := dialClickHouse(t, cctx, "stellar").Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.contract_events
			DELETE WHERE contract_id = '%s' SETTINGS mutations_sync = 2`, contract)); err != nil {
			t.Errorf("purge shape fixture events: %v", err)
		}
	})

	type ev struct {
		sym    string
		topics []string
	}
	fixture := []ev{
		{"", []string{"T0", "T1a"}},
		{"", []string{"T0", "T1b"}},       // differs from the first only in topics_xdr[2]
		{"", []string{"T0", "T1a", "T2"}}, // differs only in arity
		{"swap", []string{"SWAP", "P1"}},  // Symbol shapes ignore topic[1] ...
		{"swap", []string{"SWAP", "P2"}},  // ... so these two are one shape
	}
	for i, e := range fixture {
		q := fmt.Sprintf(`INSERT INTO stellar.contract_events
			(ledger_seq, close_time, tx_hash, op_index, event_index, contract_id, event_type,
			 topic_count, topic_0_sym, topics_xdr, data_xdr, op_args_xdr, in_successful_call)
			VALUES (%d, '2026-08-01 00:00:00', 'gh807tx', 0, %d, '%s', 'contract', %d, '%s', ['%s'], 'D%d', [], 1)`,
			lo+uint32(i), i, contract, len(e.topics), e.sym, strings.Join(e.topics, "','"), i)
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("insert fixture event %d: %v", i, err)
		}
	}

	shapes, err := chstore.DistinctTopicShapes(ctx, addr, lo, lo+uint32(len(fixture)), nil)
	if err != nil {
		t.Fatalf("DistinctTopicShapes: %v", err)
	}
	var got []string
	for _, s := range shapes {
		if s.ContractID != contract {
			continue
		}
		got = append(got, fmt.Sprintf("%s|%s|%d|%s", s.Topic0Sym, strings.Join(s.Topics, ","), s.Count, s.DataXDR))
	}
	// Sorted by count desc, then (topic_0_sym, t0, t1, arity); each exemplar is
	// its own shape's event, and the Symbol shape's is its latest (argMax by ledger).
	want := []string{
		"swap|SWAP,P2|2|D4",
		"|T0,T1a|1|D0",
		"|T0,T1a,T2|1|D2",
		"|T0,T1b|1|D1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("shapes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// These tests prove the v0.21.4 TTL-liveness path end-to-end against a real
// ClickHouse: rows inserted into stellar.ledger_entry_changes flow through the
// ttl_live_until_mv materialized view (applied from tier1_schema.sql by the
// harness) into the slim stellar.ttl_live_until projection, and
// ClassifyTTLLiveness resolves verdicts from THAT table — the
// ledger_entries_current scan path is not consulted.

// ttlAsOf is the reference ledger for verdicts. Fixture ledgers sit far above
// any other test's ranges to keep the shared container's keys disjoint.
const ttlAsOf = uint32(72_500_000)

// ttlGovernedKeyXDR builds a distinctive base64 LedgerKey for the governed
// (e.g. contract_data) entry — the input shape callers hand to
// ClassifyTTLLiveness.
func ttlGovernedKeyXDR(tag string) string {
	return base64.StdEncoding.EncodeToString([]byte("ttl-liveness-it-governed-" + tag))
}

// ttlChangeRow renders the lake's TTL change for the entry governed by
// governedXDR: KeyXDR is the 36-byte TTL LedgerKey (type=00000009 |
// sha256(decoded governed key)), EntryXDR the TTLEntry
// (lastModified(4) | data.type(4) | keyHash(32) | liveUntil(4) | ext(4)),
// truncated/padded to entryLen so malformed shapes can be seeded too.
func ttlChangeRow(governedXDR string, ledger, intra, liveUntil uint32, entryLen int) chstore.LedgerEntryChangeRow {
	rawKey, err := base64.StdEncoding.DecodeString(governedXDR)
	if err != nil {
		panic(fmt.Sprintf("fixture governed key must be valid base64: %v", err))
	}
	keyHash := sha256.Sum256(rawKey)

	ttlKey := make([]byte, 0, 36)
	ttlKey = append(ttlKey, 0x00, 0x00, 0x00, 0x09) // LedgerEntryType TTL = 9
	ttlKey = append(ttlKey, keyHash[:]...)

	full := make([]byte, 48)
	binary.BigEndian.PutUint32(full[0:4], ledger) // lastModifiedLedgerSeq
	binary.BigEndian.PutUint32(full[4:8], 9)      // data.type = TTL
	copy(full[8:40], keyHash[:])
	binary.BigEndian.PutUint32(full[40:44], liveUntil) // XDR big-endian
	// full[44:48] = ext.v 0
	entry := full
	if entryLen != len(full) {
		entry = make([]byte, entryLen)
		copy(entry, full)
	}

	return chstore.LedgerEntryChangeRow{
		LedgerSeq:      ledger,
		CloseTime:      time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		TxHash:         "ttl-liveness-it-" + governedXDR[:8],
		OpIndex:        0,
		ChangeIndex:    0,
		ChangeType:     "updated",
		EntryType:      "ttl",
		KeyXDR:         base64.StdEncoding.EncodeToString(ttlKey),
		EntryXDR:       base64.StdEncoding.EncodeToString(entry),
		IntraLedgerSeq: intra,
	}
}

func TestClassifyTTLLiveness_SlimProjectionEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	var (
		archived  = ttlGovernedKeyXDR("archived")   // lapsed long ago
		boundary  = ttlGovernedKeyXDR("boundary")   // live_until == asOf → LIVE (not yet lapsed)
		extended  = ttlGovernedKeyXDR("extended")   // lapsed then bumped in a later ledger → LIVE
		sameLedgr = ttlGovernedKeyXDR("sameledger") // bumped twice in ONE ledger → intra tie-break
		malformed = ttlGovernedKeyXDR("malformed")  // 47-byte entry → MV skips → UNKNOWN
		absent    = ttlGovernedKeyXDR("absent")     // no TTL row at all → UNKNOWN
	)

	rows := []chstore.LedgerEntryChangeRow{
		ttlChangeRow(archived, 72_000_001, 1, ttlAsOf-1_000_000, 48),
		ttlChangeRow(boundary, 72_000_002, 1, ttlAsOf, 48),
		// extended: the LATER (winning) change is handed to the writer FIRST —
		// adversarial insert order; argMax(live_until, version) must not care.
		ttlChangeRow(extended, 72_000_100, 1, ttlAsOf+5_000_000, 48),
		ttlChangeRow(extended, 72_000_003, 1, ttlAsOf-2_000_000, 48),
		// sameLedgr: two changes in ONE ledger. Only intra_ledger_seq (the low
		// 32 bits of the RMT version) separates them; the later one extends.
		ttlChangeRow(sameLedgr, 72_000_004, 7, ttlAsOf+3_000_000, 48),
		ttlChangeRow(sameLedgr, 72_000_004, 6, ttlAsOf-3_000_000, 48),
		ttlChangeRow(malformed, 72_000_005, 1, ttlAsOf+1_000_000, 47),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	keys := []string{archived, boundary, extended, sameLedgr, malformed, absent, "not!valid!base64"}
	got, err := chstore.ClassifyTTLLiveness(ctx, conn, keys, ttlAsOf)
	if err != nil {
		t.Fatalf("ClassifyTTLLiveness: %v", err)
	}

	want := map[string]chstore.TTLLiveness{
		archived:           chstore.TTLArchived,
		boundary:           chstore.TTLLive,
		extended:           chstore.TTLLive,
		sameLedgr:          chstore.TTLLive,
		malformed:          chstore.TTLUnknown, // fail-open: unrecognised shape never proves archived
		absent:             chstore.TTLUnknown,
		"not!valid!base64": chstore.TTLUnknown, // undecodable input key stays kept, no error
	}
	if len(got) != len(want) {
		t.Errorf("got %d verdicts, want %d", len(got), len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("verdict[%s] = %v, want %v", k, got[k], w)
		}
	}
}

// TestClassifyTTLLiveness_MissingProjectionFailsLoud proves the no-fallback
// contract on a real server: with stellar.ttl_live_until absent, the
// classifier returns an error naming the DDL artifact instead of silently
// degrading every key to TTLUnknown (which would read as "nothing archived").
// The table is renamed away and restored — integration tests in this package
// run sequentially, and no ledger_entry_changes insert happens in the window
// (the MV would otherwise error on its missing target).
func TestClassifyTTLLiveness_MissingProjectionFailsLoud(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	if err := conn.Exec(ctx, "RENAME TABLE stellar.ttl_live_until TO stellar.ttl_live_until_it_hidden"); err != nil {
		t.Fatalf("hide ttl_live_until: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := conn.Exec(ctx, "RENAME TABLE stellar.ttl_live_until_it_hidden TO stellar.ttl_live_until"); err != nil {
			t.Fatalf("restore ttl_live_until (container state now broken for later tests): %v", err)
		}
	}
	defer restore()

	_, err := chstore.ClassifyTTLLiveness(ctx, conn, []string{ttlGovernedKeyXDR("missing-table")}, ttlAsOf)
	if err == nil {
		t.Fatal("ClassifyTTLLiveness succeeded without the projection; the deleted scan path must not have a silent fallback")
	}
	if !strings.Contains(err.Error(), "deploy/clickhouse/ttl_live_until.sql") {
		t.Errorf("error does not point the operator at the DDL artifact: %v", err)
	}

	// Restore, then prove the guard clears: absent keys resolve UNKNOWN, no error.
	restore()
	got, err := chstore.ClassifyTTLLiveness(ctx, conn, []string{ttlGovernedKeyXDR("missing-table")}, ttlAsOf)
	if err != nil {
		t.Fatalf("ClassifyTTLLiveness after restore: %v", err)
	}
	if got[ttlGovernedKeyXDR("missing-table")] != chstore.TTLUnknown {
		t.Errorf("expected TTLUnknown for a key with no TTL row, got %v", got[ttlGovernedKeyXDR("missing-table")])
	}
}

// TestSDEXOrderBook_ConvergesAfterLakeHoleIsFilled is the served-data proof
// for the order book's cursor discipline.
//
// The LiveSink drops a WHOLE ledger under buffer pressure, leaving a hole in
// the lake that ch-live-catchup back-fills minutes later. A reader that
// bounds its incremental read by max(ledger_seq) of ledger_entry_changes
// lets Advance read straight across the hole and commit the cursor PAST it;
// the healed rows then land BELOW the cursor and are never read. An offer
// removed in the dropped ledger would stay on /v1/sdex/orderbook as resting
// liquidity — and an offer created in it would never appear — until the API
// process restarted.
//
// This drives the REAL reader and the REAL cache through the REAL endpoint
// against a real ClickHouse, in the production order of events:
//
//	B+1  offer A created                   → Load
//	B+2  offer A REMOVED, offer B created  ← this ledger is DROPPED
//	B+3  (empty), B+4 offer C created      → Advance  (hole still open)
//	B+2  healed by catch-up                → Advance  (no restart, no re-Load)
//
// and asserts both halves of the contract: while the hole is open the cursor
// HOLDS at B+1 (it never crosses the hole), and once the hole is filled the
// SAME cache advances to B+4 and serves exactly {B, C}.
//
// Proven red: on a reader without the hole check the final book is {A, C} — phantom A
// still served, B never seen — with as_of_ledger already at B+4 after the
// first Advance.
func TestSDEXOrderBook_ConvergesAfterLakeHoleIsFilled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated range ABOVE every other ledger the suite seeds (the
	// watermark tests sit at 215M/216M): the book's Load cursor is derived
	// from the lake's global tip, so this test's ledgers must be that tip.
	const base = uint32(217_000_000)
	const (
		sellerAccount = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
		issuerAccount = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
		assetCode     = "LAKEHOLE"
	)
	selling := assetCode + "-" + issuerAccount
	// Shared-ClickHouse isolation: TestNetworkThroughput_DedupsReingestedLedger
	// anchors its window on the GLOBAL max(close_time) and reserves 2027/06/15
	// as that tip. Nothing here keys on close_time (the book orders by
	// ledger_seq), so stay far below it rather than compete for the tip.
	closeTime := time.Date(2025, 3, 3, 4, 5, 6, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// land writes one whole ledger the way the live sink / ch-backfill does:
	// its entry changes AND its stellar.ledgers commit marker, in one extract.
	land := func(seq uint32, changes ...chstore.LedgerEntryChangeRow) {
		t.Helper()
		for i := range changes {
			changes[i].LedgerSeq = seq
			changes[i].CloseTime = closeTime
		}
		ext := chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{
				LedgerSeq: seq, CloseTime: closeTime,
				LedgerHash: "aa62", PrevHash: "bb62", ProtocolVersion: 23, BucketListHash: "cc62",
				TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
			},
			Changes: changes,
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("flush ledger %d: %v", seq, err)
		}
	}
	created := func(offerID int64, priceN int32) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			TxHash: "f162", OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 3,
			ChangeType: "created", EntryType: "offer", AccountID: sellerAccount,
			KeyXDR:   lakeHoleOfferKeyB64(t, sellerAccount, offerID),
			EntryXDR: lakeHoleOfferEntryB64(t, sellerAccount, offerID, assetCode, issuerAccount, priceN),
		}
	}
	// A distinct tx from `created`: ledger_entry_changes is a
	// ReplacingMergeTree over (ledger_seq, tx_hash, op_index, change_index),
	// so two same-ledger rows sharing that identity collapse into one.
	removed := func(offerID int64) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			TxHash: "f162-take", OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 2,
			ChangeType: "removed", EntryType: "offer", AccountID: sellerAccount,
			KeyXDR: lakeHoleOfferKeyB64(t, sellerAccount, offerID),
		}
	}
	const offerA, offerB, offerC = int64(9_162_001), int64(9_162_002), int64(9_162_003)

	// ── Process start: [B, B+1] landed; offer A rests at price 2. ─────────
	land(base)
	land(base+1, created(offerA, 2))

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	cache := v1.NewSDEXOrderBookCache(reader, nil)
	if err := cache.Load(ctx); err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	ts := httptest.NewServer(v1.New(v1.Options{SDEXOrderBook: cache}).Handler())
	t.Cleanup(ts.Close)
	book := func() v1.SDEXOrderBookView {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v1/sdex/orderbook?selling=" + selling + "&buying=native")
		if err != nil {
			t.Fatalf("GET orderbook: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET orderbook status = %d, want 200", resp.StatusCode)
		}
		var env struct {
			Data v1.SDEXOrderBookView `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode orderbook: %v", err)
		}
		return env.Data
	}
	askPrices := func(b v1.SDEXOrderBookView) []string {
		out := make([]string, 0, len(b.Asks))
		for _, lvl := range b.Asks {
			out = append(out, lvl.Price)
		}
		return out
	}

	loaded := book()
	if loaded.AsOfLedger != base+1 {
		t.Fatalf("precondition: as_of_ledger after Load = %d, want %d — this test's range must be the "+
			"lake's global tip; another test now seeds stellar.ledgers above %d", loaded.AsOfLedger, base+1, base)
	}
	if got := askPrices(loaded); len(got) != 1 || got[0] != "2.0000000" {
		t.Fatalf("precondition: asks after Load = %v, want [2.0000000] (offer A)", got)
	}

	// ── The drop: B+2 (A removed, B created) never lands; B+3, B+4 do. ────
	land(base + 3)
	land(base+4, created(offerC, 5))
	if err := cache.Advance(ctx); err != nil {
		t.Fatalf("Advance across the open hole: %v", err)
	}
	held := book()
	if held.AsOfLedger != base+1 {
		t.Errorf("as_of_ledger with the hole at %d still open = %d, want %d — the cursor must HOLD below "+
			"a lake hole; past it, the healed ledger's offer changes land below the cursor and are never read",
			base+2, held.AsOfLedger, base+1)
	}

	// ── The heal: ch-live-catchup back-fills B+2. Same cache, no restart. ─
	land(base+2, removed(offerA), created(offerB, 3))
	if err := cache.Advance(ctx); err != nil {
		t.Fatalf("Advance after the heal: %v", err)
	}
	healed := book()
	if healed.AsOfLedger != base+4 {
		t.Errorf("as_of_ledger after the hole healed = %d, want %d — a held cursor must RESUME to the "+
			"lake tip once the hole is filled (holding forever is not a fix)", healed.AsOfLedger, base+4)
	}
	got := askPrices(healed)
	if len(got) != 2 || got[0] != "3.0000000" || got[1] != "5.0000000" {
		t.Fatalf("asks after the hole healed = %v (ask_offers=%d), want [3.0000000 5.0000000]: offer A was "+
			"REMOVED in the healed ledger %d and must leave the book (a 2.0000000 level is the phantom), "+
			"offer B was CREATED in it and must appear, offer C landed above it",
			got, healed.AskOffers, base+2)
	}
}

func lakeHoleOfferKeyB64(t *testing.T, seller string, offerID int64) string {
	t.Helper()
	var key xdr.LedgerKey
	if err := key.SetOffer(xdr.MustAddress(seller), uint64(offerID)); err != nil {
		t.Fatalf("offer ledger key: %v", err)
	}
	b64, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal offer key: %v", err)
	}
	return b64
}

// lakeHoleOfferEntryB64 builds an offer LedgerEntry selling 10 units of
// code-issuer for native at priceN/1. lastModifiedLedgerSeq is left zero:
// the book versions an offer by its change row's own ledger, not the entry's.
func lakeHoleOfferEntryB64(t *testing.T, seller string, offerID int64, code, issuer string, priceN int32) string {
	t.Helper()
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeOffer,
			Offer: &xdr.OfferEntry{
				SellerId: xdr.MustAddress(seller),
				OfferId:  xdr.Int64(offerID),
				Selling:  xdr.MustNewCreditAsset(code, issuer),
				Buying:   xdr.MustNewNativeAsset(),
				Amount:   100_000_000,
				Price:    xdr.Price{N: xdr.Int32(priceN), D: 1},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal offer entry: %v", err)
	}
	return b64
}

// TestClickHouseContractActivitySummaryRMTDedup is the live-ClickHouse proof
// for ContractActivitySummaryFor read stellar.contract_active_ledgers
// (a ReplacingMergeTree) with a bare count(), so an overlapping backfill window
// that re-inserts the same (contract, ledger) keys as a second un-merged part
// inflated ActiveLedgersTotal (a headline card number) and the daily bars up to
// ~2x until a background merge. The fix counts uniqExact(ledger_seq). SYSTEM STOP
// MERGES pins the two parts un-merged so the dedup MUST come from the query —
// reverting the fix makes the total read 6 instead of the 3 distinct ledgers.
func TestClickHouseContractActivitySummaryRMTDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const contractID = "CTEST_CHQ2_ACTIVITY_DEDUP_AAAAAAAAAAAAAAAAAAA"
	// Three DISTINCT active ledgers, all recent so they land inside the daily
	// window (close_time within the last few days).
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	ledgers := []uint32{80_100_001, 80_100_002, 80_100_003}

	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.contract_active_ledgers"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.contract_active_ledgers")
	})

	// Two IDENTICAL inserts → two un-merged parts, each carrying the same three
	// (contract, ledger) keys (the overlapping-backfill / re-ingest state).
	for pass := 0; pass < 2; pass++ {
		b, err := raw.PrepareBatch(ctx,
			`INSERT INTO stellar.contract_active_ledgers (contract_id, ledger_seq, close_time, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare active_ledgers batch (pass %d): %v", pass, err)
		}
		ing := time.Now().UTC().Add(time.Duration(pass) * time.Minute)
		for i, l := range ledgers {
			if err := b.Append(contractID, l, base.Add(time.Duration(i)*time.Hour), ing); err != nil {
				t.Fatalf("append active ledger (pass %d): %v", pass, err)
			}
		}
		if err := b.Send(); err != nil {
			t.Fatalf("send active_ledgers batch (pass %d): %v", pass, err)
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	s, ok, err := er.ContractActivitySummaryFor(ctx, contractID, 30)
	if err != nil || !ok {
		t.Fatalf("ContractActivitySummaryFor: ok=%v err=%v", ok, err)
	}
	if s.ActiveLedgersTotal != 3 {
		t.Fatalf("ActiveLedgersTotal = %d, want 3 distinct ledgers — the un-merged duplicate part "+
			"must be deduped by uniqExact (pre-fix count(): 6)", s.ActiveLedgersTotal)
	}
	var dailySum uint64
	for _, d := range s.Daily {
		dailySum += d.ActiveLedgers
	}
	if dailySum != 3 {
		t.Fatalf("daily active-ledger sum = %d across %d days, want 3 (per-day bars must dedup too)",
			dailySum, len(s.Daily))
	}
}

// TestClickHouseContractEventsRecentPartialBackfill is the live-ClickHouse proof
// for contract_active_ledgers' availability probe is a
// LIMIT-1 table-global emptiness check that cannot see PARTIAL backfill
// coverage. In the applied-but-still-backfilling state the index is globally
// non-empty (some OTHER contract's rows) but holds NO rows for a quiet contract
// whose events do exist in contract_events. The old reader trusted that empty
// per-contract walk and served an authoritative "no events". The fix falls
// through to the unbounded contract_events scan (the source of truth). Reverting
// the fix makes ContractEventsRecent return 0 rows here instead of the real event.
func TestClickHouseContractEventsRecentPartialBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		quietContract = "CTEST_CHROLLUP3_QUIET_AAAAAAAAAAAAAAAAAAAAAAA"
		decoyContract = "CTEST_CHROLLUP3_DECOY_AAAAAAAAAAAAAAAAAAAAAAA"
		ledger        = uint32(5_000_101) // low ledger: the un-backfilled prefix
		txHash        = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	closeTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// Ingest the quiet contract's event. This also populates
	// contract_active_ledgers via its MV — which we then TRUNCATE to
	// reproduce the partial-backfill state (index present, this contract's
	// coverage NOT yet backfilled).
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "aa11aa11", PrevHash: "bb22bb22",
			ProtocolVersion: 22, TxCount: 1, OpCount: 1, SorobanEventCount: 1,
		},
		Events: []chstore.ContractEventRow{{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 0,
			ContractID: quietContract, EventType: "contract", TopicCount: 1, Topic0Sym: "transfer",
			TopicsXDR: []string{scval.MustEncodeSymbol("transfer")}, DataXDR: scval.MustEncodeString("x"),
			OpArgsXDR: []string{}, InSuccessfulCall: 1,
		}},
	}
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	raw := dialClickHouse(t, ctx, "stellar")
	// Wipe the MV-populated coverage, then seed ONE decoy row so the table is
	// globally non-empty (probe passes) but holds nothing for the quiet
	// contract — the exact partial-backfill state the finding describes.
	if err := raw.Exec(ctx, "TRUNCATE TABLE stellar.contract_active_ledgers"); err != nil {
		t.Fatalf("truncate active_ledgers: %v", err)
	}
	db, err := raw.PrepareBatch(ctx,
		`INSERT INTO stellar.contract_active_ledgers (contract_id, ledger_seq, close_time, ingested_at)`)
	if err != nil {
		t.Fatalf("prepare decoy batch: %v", err)
	}
	if err := db.Append(decoyContract, uint32(90_000_000), closeTime, time.Now().UTC()); err != nil {
		t.Fatalf("append decoy: %v", err)
	}
	if err := db.Send(); err != nil {
		t.Fatalf("send decoy: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, err := er.ContractEventsRecent(ctx, quietContract, 100, chstore.ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ContractEventsRecent returned %d rows, want 1 — the partial-backfill empty walk must "+
			"fall through to the unbounded contract_events scan, not serve a confidently-wrong empty page "+
			"(pre-fix: 0)", len(rows))
	}
	if rows[0].Seq != ledger {
		t.Fatalf("served event at ledger %d, want %d", rows[0].Seq, ledger)
	}
}
