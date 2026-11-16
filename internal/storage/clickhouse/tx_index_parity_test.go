package clickhouse_test

import (
	"bytes"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type txIndexed struct {
	TxHash  string
	TxIndex uint32
}

// txIndexParityTx builds one successful transaction invoking cid whose single operation emits a BLNT set_status event.
func txIndexParityTx(t *testing.T, seed byte, cid xdr.ContractId) (xdr.TransactionEnvelope, xdr.TransactionResultMeta) {
	t.Helper()
	src, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256{seed, 0x01})
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	op := xdr.Operation{Body: xdr.OperationBody{
		Type: xdr.OperationTypeInvokeHostFunction,
		InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
				FunctionName:    "set_status",
			},
		}},
	}}
	env := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{Tx: xdr.Transaction{
			SourceAccount: src, Fee: 100, SeqNum: 1,
			Cond:       xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
			Memo:       xdr.Memo{Type: xdr.MemoTypeMemoNone},
			Operations: []xdr.Operation{op},
		}},
	}
	hash, err := network.HashTransactionInEnvelope(env, parityPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	sym, status := xdr.ScSymbol("set_status"), xdr.Uint32(1)
	ev := xdr.ContractEvent{
		Type: xdr.ContractEventTypeContract, ContractId: &cid,
		Body: xdr.ContractEventBody{V: 0, V0: &xdr.ContractEventV0{
			Topics: []xdr.ScVal{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}},
			Data:   xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &status},
		}},
	}
	var retHash xdr.Hash
	results := []xdr.OperationResult{{Code: xdr.OperationResultCodeOpInner, Tr: &xdr.OperationResultTr{
		Type: xdr.OperationTypeInvokeHostFunction,
		InvokeHostFunctionResult: &xdr.InvokeHostFunctionResult{
			Code: xdr.InvokeHostFunctionResultCodeInvokeHostFunctionSuccess, Success: &retHash,
		},
	}}}
	return env, xdr.TransactionResultMeta{
		Result: xdr.TransactionResultPair{
			TransactionHash: xdr.Hash(hash),
			Result: xdr.TransactionResult{
				FeeCharged: 100,
				Result:     xdr.TransactionResultResult{Code: xdr.TransactionResultCodeTxSuccess, Results: &results},
			},
		},
		TxApplyProcessing: xdr.TransactionMeta{V: 4, V4: &xdr.TransactionMetaV4{
			Operations: []xdr.OperationMetaV2{{Events: []xdr.ContractEvent{ev}}},
		}},
	}
}

// txIndexParityLedger is a ledger of two BLNT transactions applied in the reverse of their hash order, so the lake's tx_hash sort
// disagrees with apply order.
func txIndexParityLedger(t *testing.T, seq uint32) (xdr.LedgerCloseMeta, []string) {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, blnt.MainnetFixedPool)
	if err != nil {
		t.Fatalf("decode pool strkey: %v", err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	envA, procA := txIndexParityTx(t, 0x10, cid)
	envB, procB := txIndexParityTx(t, 0x20, cid)
	if bytes.Compare(procA.Result.TransactionHash[:], procB.Result.TransactionHash[:]) < 0 {
		envA, envB, procA, procB = envB, envA, procB, procA
	}
	applied := []string{hex.EncodeToString(procA.Result.TransactionHash[:]), hex.EncodeToString(procB.Result.TransactionHash[:])}
	lcm := xdr.LedgerCloseMeta{V: 1, V1: &xdr.LedgerCloseMetaV1{
		LedgerHeader: xdr.LedgerHeaderHistoryEntry{Header: xdr.LedgerHeader{
			LedgerSeq: xdr.Uint32(seq),
			ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(1_760_000_000)},
		}},
		TxSet: xdr.GeneralizedTransactionSet{V: 1, V1TxSet: &xdr.TransactionSetV1{
			Phases: []xdr.TransactionPhase{{V: 0, V0Components: &[]xdr.TxSetComponent{{
				Type:                  xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
				TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{Txs: []xdr.TransactionEnvelope{envB, envA}},
			}}}},
		}},
		TxProcessing: []xdr.TransactionResultMeta{procA, procB},
	}}
	return lcm, applied
}

// The lake feed (extract -> stellar.transactions tx_index -> index-stamping orderer) and the dispatcher give BLNT the same 0-based
// apply positions, in apply order, for a ledger whose hash order is the reverse.
func TestTxIndexParity_LakeAndDispatcherAgree(t *testing.T) {
	lcm, applied := txIndexParityLedger(t, blnt.GenesisLedger+10)
	want := []txIndexed{{applied[0], 0}, {applied[1], 1}}

	ext, err := clickhouse.ExtractLedger(lcm, parityPassphrase)
	if err != nil {
		t.Fatalf("ExtractLedger: %v", err)
	}
	rows := slices.Clone(ext.Events)
	slices.SortStableFunc(rows, func(a, b clickhouse.ContractEventRow) int { return strings.Compare(a.TxHash, b.TxHash) })
	evs := make([]events.Event, 0, len(rows))
	for _, r := range rows {
		evs = append(evs, events.Event{
			Type: r.EventType, Ledger: r.LedgerSeq, LedgerClosedAt: r.CloseTime.UTC().Format(time.RFC3339),
			ContractID: r.ContractID, OperationIndex: int(r.OpIndex), EventIndex: int(r.EventIndex), TxHash: r.TxHash,
			InSuccessfulContractCall: r.InSuccessfulCall != 0, Topic: r.TopicsXDR, Value: r.DataXDR, OpArgs: r.OpArgsXDR,
		})
	}
	dec := blnt.NewDecoder()
	var lake []txIndexed
	if err := clickhouse.OrderWithTxIndex(evs, ext.Txs, func(ev events.Event) error {
		if !dec.Matches(ev) {
			t.Fatalf("blnt decoder rejected lake event %+v", ev)
		}
		out, derr := dec.Decode(ev)
		if derr != nil {
			return derr
		}
		for _, o := range out {
			lake = append(lake, decodedTxIndex(t, o))
		}
		return nil
	}); err != nil {
		t.Fatalf("lake path: %v", err)
	}

	out, err := dispatcher.New(blnt.NewDecoder()).ProcessLedger(lcm, parityPassphrase)
	if err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	var live []txIndexed
	for _, o := range out {
		live = append(live, decodedTxIndex(t, o))
	}

	if !slices.Equal(lake, want) {
		t.Errorf("lake path = %+v, want %+v", lake, want)
	}
	if !slices.Equal(live, want) {
		t.Errorf("dispatcher path = %+v, want %+v", live, want)
	}
}

func decodedTxIndex(t *testing.T, o any) txIndexed {
	t.Helper()
	ev, ok := o.(blnt.Event)
	if !ok || ev.TxIndex == nil {
		t.Fatalf("decoded %T without a TxIndex: %+v", o, o)
	}
	return txIndexed{TxHash: ev.TxHash, TxIndex: *ev.TxIndex}
}
