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
// cohort proof (2026-09-17), run through the real cycle on a real
// ClickHouse. A creation on a post-P23-only chain is recorded ONE way: a
// CAP-67 `transfer` movement paired with a CreateAccount operation. The
// cycle's post-P23 arm reads exactly that pair — but only for ledgers at or
// above its boundary, and pre-fix the boundary was pubnet's constant baked
// into the SQL. Every test-net ledger sits below 58,762,517, so the classic
// arm owned all of them and looked for `create_account` movements that chain
// never writes: zero account_creator_edges rows, zero `created` cohorts,
// however full the archive.
//
// Fixture: one CreateAccount operation and its funding transfer at a low
// ledger. The same fixture is rolled up twice — at the pubnet boundary
// (the pre-fix behaviour on a test net: no edge) and at the chain's start
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
	// the pre-fix test-net symptom, pinned so the substitution below is
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
