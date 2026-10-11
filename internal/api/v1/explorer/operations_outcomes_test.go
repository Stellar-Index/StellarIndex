package explorer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestStitchTxOutcomes is the D-PART-FAILEDTX core: a failed transaction's
// operation must be marked FAILED (transaction_successful=false + reason) when
// shown in a list view, a successful one marked applied, and an operation whose
// outcome was NOT read must be left UNKNOWN (nil) — never defaulted to success.
func TestStitchTxOutcomes(t *testing.T) {
	ops := []OpView{
		{TxHash: "failedtx", Type: "payment"},
		{TxHash: "oktx", Type: "payment"},
		{TxHash: "unreadtx", Type: "payment"},
	}
	outcomes := map[string]clickhouse.TxOutcome{
		"failedtx": {Successful: false, ResultCode: int32(xdr.TransactionResultCodeTxFailed)},
		"oktx":     {Successful: true, ResultCode: int32(xdr.TransactionResultCodeTxSuccess)},
		// "unreadtx" deliberately absent (outcome read did not return it).
	}
	stitchTxOutcomes(ops, outcomes)

	// failed tx: marked failed, with the reason.
	if ops[0].TransactionSuccessful == nil || *ops[0].TransactionSuccessful {
		t.Errorf("failed-tx op: transaction_successful = %v, want false", ops[0].TransactionSuccessful)
	}
	if ops[0].TransactionResult != "tx_failed" {
		t.Errorf("failed-tx op: transaction_result = %q, want tx_failed", ops[0].TransactionResult)
	}
	// successful tx: marked applied.
	if ops[1].TransactionSuccessful == nil || !*ops[1].TransactionSuccessful {
		t.Errorf("ok-tx op: transaction_successful = %v, want true", ops[1].TransactionSuccessful)
	}
	if ops[1].TransactionResult != "tx_success" {
		t.Errorf("ok-tx op: transaction_result = %q, want tx_success", ops[1].TransactionResult)
	}
	// unread outcome: UNKNOWN, never a defaulted success.
	if ops[2].TransactionSuccessful != nil {
		t.Errorf("unread-tx op: transaction_successful = %v, want nil (unknown, not success)", *ops[2].TransactionSuccessful)
	}
	if ops[2].TransactionResult != "" {
		t.Errorf("unread-tx op: transaction_result = %q, want empty", ops[2].TransactionResult)
	}
}

// TestStitchTxOutcomesDistinctPointers guards against a shared-pointer bug:
// two ops from different transactions with different outcomes must not alias
// the same *bool.
func TestStitchTxOutcomesDistinctPointers(t *testing.T) {
	ops := []OpView{{TxHash: "a"}, {TxHash: "b"}}
	stitchTxOutcomes(ops, map[string]clickhouse.TxOutcome{
		"a": {Successful: true, ResultCode: 0},
		"b": {Successful: false, ResultCode: -1},
	})
	if ops[0].TransactionSuccessful == ops[1].TransactionSuccessful {
		t.Fatal("distinct-outcome ops alias the same *bool")
	}
	if !*ops[0].TransactionSuccessful || *ops[1].TransactionSuccessful {
		t.Errorf("aliasing corrupted values: a=%v b=%v", *ops[0].TransactionSuccessful, *ops[1].TransactionSuccessful)
	}
}

func TestOpRowTxKeys(t *testing.T) {
	rows := []clickhouse.OpRow{
		{Seq: 500, TxHash: "h1"},
		{Seq: 300, TxHash: "h2"},
		{Seq: 700, TxHash: "h1"}, // dup hash, higher ledger
		{Seq: 400, TxHash: "h3"},
		{Seq: 500, TxHash: "h4"}, // dup ledger, new hash
	}
	ledgers, hashes := opRowTxKeys(rows)
	// The exact SET of ledgers touched, NOT [min,max]: a span ([300,700], 401 ledgers) would make
	// the outcome read scale with the account's idleness instead of the page. See
	// clickhouse.TxOutcomesByHash.
	want := map[uint32]bool{300: true, 400: true, 500: true, 700: true}
	if len(ledgers) != len(want) {
		t.Errorf("distinct ledgers = %d (%v), want %d", len(ledgers), ledgers, len(want))
	}
	for _, l := range ledgers {
		if !want[l] {
			t.Errorf("unexpected ledger %d in %v", l, ledgers)
		}
		delete(want, l)
	}
	if len(want) != 0 {
		t.Errorf("missing ledgers %v from %v", want, ledgers)
	}
	if len(hashes) != 4 {
		t.Errorf("distinct hashes = %d (%v), want 4", len(hashes), hashes)
	}
	// empty input is safe.
	if ls, hs := opRowTxKeys(nil); ls != nil || hs != nil {
		t.Errorf("empty input = (%v,%v), want (nil,nil)", ls, hs)
	}
}

// lakeOutcomeReader models `WHERE ledger_seq IN (?) AND tx_hash IN (?)`: a stored row is answered
// only when BOTH its ledger and its hash were asked for. The page's exact ledger set finds every
// transaction; a [lo,hi] span flattened into two values finds only the endpoint ledgers.
type lakeOutcomeReader struct {
	*capReader
	rows map[uint32]map[string]clickhouse.TxOutcome
}

func (r *lakeOutcomeReader) TxOutcomesByHash(_ context.Context, ledgers []uint32, hashes []string) (map[string]clickhouse.TxOutcome, error) {
	want := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		want[h] = struct{}{}
	}
	out := map[string]clickhouse.TxOutcome{}
	for _, l := range ledgers {
		for h, o := range r.rows[l] {
			if _, ok := want[h]; ok {
				out[h] = o
			}
		}
	}
	return out, nil
}

// TestStampTxOutcomes_IdleAccountPageIsFullyStamped: an idle account's 50-op page straddles
// millions of ledgers, so keying the parent-transaction read on that span (not the page's exact
// ledgers) scales the read with idleness, blows stampTxOutcomes' budget, and leaves every
// operation with an unknown outcome.
func TestStampTxOutcomes_IdleAccountPageIsFullyStamped(t *testing.T) {
	// 50 ops, one per ledger, ~50k ledgers apart — a real idle-account page
	// shape (spans of 2.5M-42M ledgers are ordinary on r1).
	const pageSize = 50
	rows := make([]clickhouse.OpRow, pageSize)
	lake := map[uint32]map[string]clickhouse.TxOutcome{}
	for i := range rows {
		seq := uint32(21_887_440 + i*50_000)
		hash := fmt.Sprintf("tx%02d", i)
		rows[i] = clickhouse.OpRow{Seq: seq, TxHash: hash, OpIndex: uint32(i)}
		// Every other transaction FAILED, so a regression that silently
		// defaults to "applied" is visible too.
		lake[seq] = map[string]clickhouse.TxOutcome{
			hash: {Successful: i%2 == 0, ResultCode: int32(i % 2)},
		}
	}
	ops := make([]OpView, pageSize)
	for i, o := range rows {
		ops[i] = OpView{TxHash: o.TxHash}
	}

	reader := &lakeOutcomeReader{capReader: &capReader{probe: &deadlineProbe{}}, rows: lake}
	h := newProbeHandler(reader, nil)

	note := h.stampTxOutcomes(context.Background(), ops, rows)
	if note != "" {
		t.Fatalf("coverage note fired on a healthy read: %q", note)
	}

	stamped := 0
	for i := range ops {
		if ops[i].TransactionSuccessful == nil {
			continue
		}
		stamped++
		if want := i%2 == 0; *ops[i].TransactionSuccessful != want {
			t.Errorf("op %d transaction_successful = %v, want %v", i, *ops[i].TransactionSuccessful, want)
		}
	}
	if stamped != pageSize {
		t.Errorf("stamped %d of %d operations — an idle account's page is serving ops with an "+
			"UNKNOWN transaction outcome (#332 F1); the outcome read must be keyed on the page's "+
			"exact ledger set, not its span", stamped, pageSize)
	}
}

// TestTxOutcomeStitchBudget_IsSizedToTheMeasuredRead pins the budget to the read it bounds: keyed
// on the exact ledger set the read is fast even cold, so the budget is a generous multiple of the
// worst case yet small enough to cap what this stitch adds to an unauthenticated request.
func TestTxOutcomeStitchBudget_IsSizedToTheMeasuredRead(t *testing.T) {
	if txOutcomeStitchBudget > time.Second {
		t.Errorf("txOutcomeStitchBudget = %v; the read it bounds is 32-61ms cold on r1, so a "+
			"budget above 1s only lengthens the tail an unauthenticated request can be held for",
			txOutcomeStitchBudget)
	}
	if txOutcomeStitchBudget < 500*time.Millisecond {
		t.Errorf("txOutcomeStitchBudget = %v; below ~8x the measured cold read a transient "+
			"lake hiccup would degrade the page to UNKNOWN outcomes unnecessarily",
			txOutcomeStitchBudget)
	}
}
