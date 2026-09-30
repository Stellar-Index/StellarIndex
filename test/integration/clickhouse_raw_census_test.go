//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestVerifyLake_RawCensusCatchesDroppedTablePartition proves verify-lake's
// raw-table census on a real ClickHouse: a DROP PARTITION on a non-ledger raw
// table leaves stellar.ledgers contiguous and hash-chained, so SubstrateProblem
// (the contiguity + hash-chain population) still reports the range clean, and
// only the census sees the rows are gone.
func TestVerifyLake_RawCensusCatchesDroppedTablePartition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Partitions 148/149: nothing else in the suite writes there.
	const lo, hi = uint32(148_999_997), uint32(149_000_002)
	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		conn := dialClickHouse(t, cctx, "stellar")
		for _, table := range []string{"ledgers", "transactions", "operations", "operation_results", "operation_participants", "contract_events"} {
			if err := conn.Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.%s DELETE
				WHERE ledger_seq BETWEEN %d AND %d SETTINGS mutations_sync = 2`, table, lo, hi)); err != nil {
				t.Errorf("purge raw census fixture %s: %v", table, err)
			}
		}
	})

	// Stay below the 2027-06-15 close_time TestNetworkThroughput_DedupsReingestedLedger
	// reserves as the global tip.
	closeTime := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for seq := lo; seq <= hi; seq++ {
		ext := chstore.LedgerExtract{Ledger: chstore.LedgerRow{
			LedgerSeq: seq, CloseTime: closeTime,
			LedgerHash: fmt.Sprintf("h%d", seq), PrevHash: fmt.Sprintf("h%d", seq-1),
			ProtocolVersion: 22, BucketListHash: "cc00",
			TxCount: 2, OpCount: 3, SorobanEventCount: 2,
			TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
		}}
		// Distinct sort keys per row, so a merge cannot collapse fixture rows.
		for tx := uint32(0); tx < 2; tx++ {
			ext.Txs = append(ext.Txs, chstore.TransactionRow{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: fmt.Sprintf("rawcensus%d-%d", seq, tx),
				TxIndex: tx, SourceAccount: "GRAWCENSUSFIXTURE", OperationCount: 1, Successful: 1,
			})
		}
		for op := uint32(0); op < 3; op++ {
			txHash := fmt.Sprintf("rawcensus%d-0", seq)
			ext.Ops = append(ext.Ops, chstore.OperationRow{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: txHash, OpIndex: op, OpType: "payment",
				SourceAccount: "GRAWCENSUSFIXTURE",
			})
			ext.Results = append(ext.Results, chstore.OperationResultRow{LedgerSeq: seq, TxHash: txHash, OpIndex: op})
		}
		ext.Participants = append(ext.Participants, chstore.OperationParticipantRow{
			Account: "GRAWCENSUSPARTICIPANT", LedgerSeq: seq, CloseTime: closeTime,
			TxHash: fmt.Sprintf("rawcensus%d-0", seq),
		})
		for ev := uint32(0); ev < 2; ev++ {
			ext.Events = append(ext.Events, chstore.ContractEventRow{
				LedgerSeq: seq, CloseTime: closeTime, TxHash: fmt.Sprintf("rawcensus%d-1", seq),
				EventIndex: ev, ContractID: "CRAWCENSUSFIXTURE", EventType: "contract",
				TopicCount: 1, Topic0Sym: "transfer", InSuccessfulCall: 1,
			})
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	textfile := filepath.Join(t.TempDir(), "lake_verify.prom")
	run := func() error {
		return chops.Run([]string{
			"verify-lake", "-checks", "rawcensus",
			"-config", filepath.Join(t.TempDir(), "absent.toml"), "-ch-addr", addr,
			"-from", fmt.Sprint(lo), "-to", fmt.Sprint(hi), "-textfile", textfile,
		})
	}
	assertExit := func(t *testing.T, stage string, err error, code int, failures string) {
		t.Helper()
		var exit *opsutil.ExitCodeError
		if !errors.As(err, &exit) || exit.Code != code {
			t.Fatalf("%s: verify-lake err = %v, want ExitCodeError{Code:%d}", stage, err, code)
		}
		body, rerr := os.ReadFile(textfile)
		if rerr != nil {
			t.Fatalf("%s: read textfile: %v", stage, rerr)
		}
		if want := `stellarindex_lake_verify_failures{check="raw_census"} ` + failures + "\n"; !strings.Contains(string(body), want) {
			t.Fatalf("%s: textfile missing %q:\n%s", stage, want, body)
		}
	}

	// (a) intact fixture passes.
	if err := run(); err != nil {
		t.Fatalf("verify-lake over the intact fixture = %v, want nil", err)
	}

	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, "ALTER TABLE stellar.operations DROP PARTITION 149"); err != nil {
		t.Fatalf("drop operations partition: %v", err)
	}

	// (b) the pre-census population cannot see it.
	if p, has, d, err := chstore.SubstrateProblem(ctx, addr, lo, hi); err != nil || has {
		t.Fatalf("SubstrateProblem after the operations drop = (%d, %v, %q, %v); this test assumes ledgers stay intact", p, has, d, err)
	}

	// (c) the census does.
	assertExit(t, "operations dropped", run(), 1, "1")

	// (d) a presence-only table's loss is reported as such.
	t.Run("presence-only participants drop", func(t *testing.T) {
		if err := conn.Exec(ctx, "ALTER TABLE stellar.operation_participants DROP PARTITION 149"); err != nil {
			t.Fatalf("drop operation_participants partition: %v", err)
		}
		short, _, err := chstore.RawTableCensus(ctx, addr, lo, hi)
		if err != nil {
			t.Fatalf("RawTableCensus: %v", err)
		}
		want := []chstore.RawTableShortfall{
			{Table: "operations", Partition: 149, Expected: 9, Present: 0},
			{Table: "operation_participants", Partition: 149, Expected: 1, Present: 0},
		}
		if !reflect.DeepEqual(short, want) {
			t.Fatalf("shortfalls = %+v, want %+v", short, want)
		}
		assertExit(t, "participants dropped", run(), 2, "2")
	})
}
