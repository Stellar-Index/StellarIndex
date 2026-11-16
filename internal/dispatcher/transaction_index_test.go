package dispatcher

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// txIndexTx builds one successful Soroban transaction whose single operation emits evs.
func txIndexTx(t *testing.T, seed byte, evs []xdr.ContractEvent) (xdr.TransactionEnvelope, xdr.TransactionResultMeta) {
	t.Helper()
	srcMuxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256{seed, 0x01})
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	op, _ := provInvokeOp(t, provContractA, "x")
	envelope := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{Tx: xdr.Transaction{
			SourceAccount: srcMuxed,
			Fee:           100,
			SeqNum:        1,
			Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
			Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
			Operations:    []xdr.Operation{op},
		}},
	}
	hash, err := network.HashTransactionInEnvelope(envelope, testPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	opResults := []xdr.OperationResult{{Code: xdr.OperationResultCodeOpInner}}
	return envelope, xdr.TransactionResultMeta{
		Result: xdr.TransactionResultPair{
			TransactionHash: xdr.Hash(hash),
			Result: xdr.TransactionResult{
				FeeCharged: 100,
				Result:     xdr.TransactionResultResult{Code: xdr.TransactionResultCodeTxSuccess, Results: &opResults},
			},
		},
		TxApplyProcessing: xdr.TransactionMeta{
			V:  4,
			V4: &xdr.TransactionMetaV4{Operations: []xdr.OperationMetaV2{{Events: evs}}},
		},
	}
}

// Events carry their transaction's 1-based TxProcessing position, not the hash order and not the position among readable
// transactions: an unreadable entry ahead of them still counts.
func TestProcessLedger_TransactionIndexIsApplyOrder(t *testing.T) {
	envX, procX := txIndexTx(t, 0x10, []xdr.ContractEvent{provEvent("FIRST", provContractA)})
	envY, procY := txIndexTx(t, 0x20, []xdr.ContractEvent{provEvent("SECOND", provContractA)})
	// Apply the hash-greater transaction first so apply order is the reverse of hash order.
	first, second := procX, procY
	firstEnv, secondEnv := envX, envY
	if bytes.Compare(procX.Result.TransactionHash[:], procY.Result.TransactionHash[:]) < 0 {
		first, second = procY, procX
		firstEnv, secondEnv = envY, envX
	}
	unreadable := xdr.TransactionResultMeta{Result: xdr.TransactionResultPair{TransactionHash: xdr.Hash{0xEE}}}
	lcm := mkEntryWalkLedger(9_000_002,
		[]xdr.TransactionEnvelope{secondEnv, firstEnv},
		[]xdr.TransactionResultMeta{unreadable, first, second})

	strA, err := contractIDToStrkey(provContractA)
	if err != nil {
		t.Fatal(err)
	}
	spy := &provSpyDecoder{name: "spy", contract: strA}
	disp := New(spy)
	if _, err := disp.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if got := disp.Stats().TxReadErrors; got != 1 {
		t.Fatalf("TxReadErrors = %d, want 1 (the unreadable entry must be skipped)", got)
	}
	if len(spy.got) != 2 {
		t.Fatalf("routed %d events, want 2", len(spy.got))
	}
	for i, want := range []struct {
		hash xdr.Hash
		ti   int
	}{{first.Result.TransactionHash, 2}, {second.Result.TransactionHash, 3}} {
		ev := spy.got[i]
		if ev.TxHash != hex.EncodeToString(want.hash[:]) || ev.TransactionIndex != want.ti {
			t.Errorf("event %d = (%s, %d), want (%s, %d)", i, ev.TxHash, ev.TransactionIndex, hex.EncodeToString(want.hash[:]), want.ti)
		}
	}
}
