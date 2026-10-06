//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseAccountHistoryHidesFailedParticipantTxs: a failed classic tx
// still writes stellar.operation_participants rows, so anyone could plant a
// row in any account's public history for the price of a fee. The account
// listings must drop participant-only rows of failed transactions while
// keeping the account's own failed transactions (it sourced them) and every
// successful one. The 20 failed spam txs sit NEWEST so the participant arm
// has to page past them, and the limit-1 walk crosses several windows.
func TestClickHouseAccountHistoryHidesFailedParticipantTxs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		victim   = "GTEST_INV2697_VICTIM_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		spammer  = "GTEST_INV2697_SPAMMER_AAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		base     = uint32(7_269_701)
		spamFrom = base + 10
		spamN    = 20
	)
	// Far below the throughput test's global close_time tip (see
	// account_activity_watermark_test.go).
	closeTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)

	fixture := []participantFixtureTx{
		{base, true, spammer, spammer, true},     // successful payment to victim: visible
		{base + 1, false, victim, victim, false}, // victim's own failed tx: visible (sourced)
		{base + 2, false, victim, spammer, true}, // victim's own failed tx, op sourced by another: visible
	}
	for i := uint32(0); i < spamN; i++ {
		fixture = append(fixture, participantFixtureTx{spamFrom + i, false, spammer, spammer, true}) // hidden
	}
	insertParticipantFixture(t, ctx, victim, closeTime, fixture)

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	want := []uint32{base + 2, base + 1, base}

	t.Run("transactions", func(t *testing.T) {
		txs, _, err := er.AccountTransactions(ctx, victim, 50, chstore.ExplorerCursor{})
		if err != nil {
			t.Fatalf("AccountTransactions: %v", err)
		}
		assertSeqs(t, "AccountTransactions page", txSeqs(txs), want)

		var walked []uint32
		cur := chstore.ExplorerCursor{}
		for page := 0; page < len(want)+2; page++ {
			txs, _, err := er.AccountTransactions(ctx, victim, 1, cur)
			if err != nil {
				t.Fatalf("AccountTransactions limit-1 page %d: %v", page, err)
			}
			if len(txs) == 0 {
				break
			}
			walked = append(walked, txs[0].Seq)
			cur = chstore.ExplorerCursor{Ledger: txs[0].Seq, A: txs[0].TxIndex}
		}
		assertSeqs(t, "AccountTransactions limit-1 walk", walked, want)
	})

	t.Run("operations", func(t *testing.T) {
		ops, _, err := er.AccountOperations(ctx, victim, 50, chstore.ExplorerCursor{})
		if err != nil {
			t.Fatalf("AccountOperations: %v", err)
		}
		assertSeqs(t, "AccountOperations page", opSeqs(ops), want)

		var walked []uint32
		cur := chstore.ExplorerCursor{}
		for page := 0; page < len(want)+2; page++ {
			ops, _, err := er.AccountOperations(ctx, victim, 1, cur)
			if err != nil {
				t.Fatalf("AccountOperations limit-1 page %d: %v", page, err)
			}
			if len(ops) == 0 {
				break
			}
			walked = append(walked, ops[0].Seq)
			cur = chstore.ExplorerCursor{Ledger: ops[0].Seq, A: ops[0].TxIndex, B: ops[0].OpIndex}
		}
		assertSeqs(t, "AccountOperations limit-1 walk", walked, want)
	})

	t.Run("op-type counts", func(t *testing.T) {
		counts, err := er.AccountOperationTypeCounts(ctx, victim)
		if err != nil {
			t.Fatalf("AccountOperationTypeCounts: %v", err)
		}
		var total int64
		for _, c := range counts {
			total += c.Count
		}
		if total != int64(len(want)) {
			t.Fatalf("AccountOperationTypeCounts total = %d, want %d (failed participant-only ops counted): %+v",
				total, len(want), counts)
		}
	})

	// The spammer sourced every spam tx (and the op in base+2); its own
	// history keeps them all.
	t.Run("spammer keeps its own failed txs", func(t *testing.T) {
		txs, _, err := er.AccountTransactions(ctx, spammer, 50, chstore.ExplorerCursor{})
		if err != nil {
			t.Fatalf("AccountTransactions(spammer): %v", err)
		}
		if len(txs) != spamN+2 {
			t.Fatalf("spammer has %d txs, want %d", len(txs), spamN+2)
		}
	})
}

// participantFixtureTx is one single-op transaction of a participant fixture.
type participantFixtureTx struct {
	seq         uint32
	ok          bool
	txSource    string
	opSource    string
	participant bool // the op names the fixture's participant as a non-source participant
}

// insertParticipantFixture writes txs to stellar.transactions, operations and
// operation_participants in the sink's flush order.
func insertParticipantFixture(t *testing.T, ctx context.Context, participant string, closeTime time.Time, txs []participantFixtureTx) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	tb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.transactions
		(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
		 operation_count, successful, result_code, memo_type, memo)`)
	if err != nil {
		t.Fatalf("prepare tx batch: %v", err)
	}
	ob, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.operations
		(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)`)
	if err != nil {
		t.Fatalf("prepare op batch: %v", err)
	}
	pb, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.operation_participants
		(account, ledger_seq, close_time, tx_hash, tx_index, op_index)`)
	if err != nil {
		t.Fatalf("prepare participant batch: %v", err)
	}
	for _, f := range txs {
		hash := fmt.Sprintf("%064d", f.seq)
		successful, code := uint8(1), int32(0)
		if !f.ok {
			successful, code = 0, -1
		}
		if err := tb.Append(f.seq, closeTime, hash, uint32(0), f.txSource, int64(100), int64(100),
			uint16(1), successful, code, "MemoTypeMemoNone", ""); err != nil {
			t.Fatalf("append tx: %v", err)
		}
		if err := ob.Append(f.seq, closeTime, hash, uint32(0), uint32(0), "OperationTypePayment",
			f.opSource, "Ym9keQ=="); err != nil {
			t.Fatalf("append op: %v", err)
		}
		if f.participant {
			if err := pb.Append(participant, f.seq, closeTime, hash, uint32(0), uint32(0)); err != nil {
				t.Fatalf("append participant: %v", err)
			}
		}
	}
	// Ingest order (Sink.Flush): transactions, operations, participants.
	if err := tb.Send(); err != nil {
		t.Fatalf("send tx: %v", err)
	}
	if err := ob.Send(); err != nil {
		t.Fatalf("send op: %v", err)
	}
	if err := pb.Send(); err != nil {
		t.Fatalf("send participant: %v", err)
	}
}

// TestClickHouseAccountHistoryParticipantBulkFailedTxs runs the participant
// arm's multi-chunk visibility SQL and its query budget on real ClickHouse:
// 1,500 participant txs (every third successful) put more than one
// visibility chunk in a window, and the 9,000-tx failed run below them is
// longer than one request's budget, so pages end short at a scan frontier.
// Walking every page the way the handler does must yield exactly the
// successful txs, newest first, none skipped or repeated.
func TestClickHouseAccountHistoryParticipantBulkFailedTxs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		victim  = "GTEST_INV2697_BULKVICTIM_AAAAAAAAAAAAAAAAAAAAAAAAAA"
		spammer = "GTEST_INV2697_BULKSPAM_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		base    = uint32(7_270_000)
		failedN = 9000
		mixedN  = 1500
		limit   = 200
	)
	closeTime := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)

	fixture := []participantFixtureTx{{base, true, spammer, spammer, true}}
	for i := uint32(1); i <= failedN; i++ {
		fixture = append(fixture, participantFixtureTx{base + i, false, spammer, spammer, true})
	}
	for i := uint32(1); i <= mixedN; i++ {
		fixture = append(fixture, participantFixtureTx{base + failedN + i, i%3 == 0, spammer, spammer, true})
	}
	var want []uint32
	for i := len(fixture) - 1; i >= 0; i-- {
		if fixture[i].ok {
			want = append(want, fixture[i].seq)
		}
	}
	insertParticipantFixture(t, ctx, victim, closeTime, fixture)

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	// walk pages like the handler: resume when set, else the last row of a
	// full page, else stop.
	walk := func(t *testing.T, page func(chstore.ExplorerCursor) ([]uint32, chstore.ExplorerCursor, chstore.ExplorerCursor, error)) {
		t.Helper()
		var got []uint32
		var cur chstore.ExplorerCursor
		resumes := 0
		for n := 0; n < 200; n++ {
			seqs, last, resume, err := page(cur)
			if err != nil {
				t.Fatalf("page %d: %v", n, err)
			}
			if n == 0 && len(seqs) != limit {
				t.Fatalf("first page has %d rows, want a full %d from two visibility chunks", len(seqs), limit)
			}
			got = append(got, seqs...)
			switch {
			case resume.IsSet():
				resumes++
				cur = resume
			case len(seqs) == limit:
				cur = last
			default:
				if resumes == 0 {
					t.Fatal("no page stopped at a scan frontier; the budget path did not run")
				}
				assertSeqs(t, "bulk walk", got, want)
				return
			}
		}
		t.Fatal("walk did not end")
	}

	t.Run("transactions", func(t *testing.T) {
		walk(t, func(cur chstore.ExplorerCursor) ([]uint32, chstore.ExplorerCursor, chstore.ExplorerCursor, error) {
			txs, resume, err := er.AccountTransactions(ctx, victim, limit, cur)
			var last chstore.ExplorerCursor
			if len(txs) > 0 {
				last = chstore.ExplorerCursor{Ledger: txs[len(txs)-1].Seq, A: txs[len(txs)-1].TxIndex}
			}
			return txSeqs(txs), last, resume, err
		})
	})
	t.Run("operations", func(t *testing.T) {
		walk(t, func(cur chstore.ExplorerCursor) ([]uint32, chstore.ExplorerCursor, chstore.ExplorerCursor, error) {
			ops, resume, err := er.AccountOperations(ctx, victim, limit, cur)
			var last chstore.ExplorerCursor
			if n := len(ops); n > 0 {
				last = chstore.ExplorerCursor{Ledger: ops[n-1].Seq, A: ops[n-1].TxIndex, B: ops[n-1].OpIndex}
			}
			return opSeqs(ops), last, resume, err
		})
	})
}

func txSeqs(txs []chstore.TxSummary) []uint32 {
	out := make([]uint32, len(txs))
	for i, tx := range txs {
		out[i] = tx.Seq
	}
	return out
}

func opSeqs(ops []chstore.OpRow) []uint32 {
	out := make([]uint32, len(ops))
	for i, op := range ops {
		out[i] = op.Seq
	}
	return out
}

func assertSeqs(t *testing.T, what string, got, want []uint32) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: ledgers %v, want %v (failed participant-only txs must be absent)", what, got, want)
	}
}
