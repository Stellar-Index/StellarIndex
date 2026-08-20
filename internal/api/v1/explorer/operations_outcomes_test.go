package explorer

import (
	"testing"

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
	}
	lo, hi, hashes := opRowTxKeys(rows)
	if lo != 300 || hi != 700 {
		t.Errorf("ledger span = [%d,%d], want [300,700]", lo, hi)
	}
	if len(hashes) != 3 {
		t.Errorf("distinct hashes = %d (%v), want 3", len(hashes), hashes)
	}
	// empty input is safe.
	if lo, hi, hs := opRowTxKeys(nil); lo != 0 || hi != 0 || hs != nil {
		t.Errorf("empty input = (%d,%d,%v), want (0,0,nil)", lo, hi, hs)
	}
}
