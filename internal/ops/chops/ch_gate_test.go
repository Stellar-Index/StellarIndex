// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const gateTestPassphrase = network.PublicNetworkPassphrase

func gateBumpOps(n int) []xdr.Operation {
	ops := make([]xdr.Operation, n)
	for i := range ops {
		ops[i] = xdr.Operation{Body: xdr.OperationBody{
			Type:           xdr.OperationTypeBumpSequence,
			BumpSequenceOp: &xdr.BumpSequenceOp{BumpTo: xdr.SequenceNumber(10 + i)},
		}}
	}
	return ops
}

// gateTx builds one successful transaction with nOps operations. When
// unreadable is set, its processing entry carries a hash that matches no
// envelope in the tx set, so the SDK tx reader fails on it.
func gateTx(t *testing.T, seed byte, nOps int, unreadable bool) (xdr.TransactionEnvelope, xdr.TransactionResultMeta) {
	t.Helper()
	var src [32]byte
	for i := range src {
		src[i] = seed + byte(i)
	}
	srcMuxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256(src))
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	env := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{Tx: xdr.Transaction{
			SourceAccount: srcMuxed,
			Fee:           100,
			SeqNum:        1,
			Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
			Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
			Operations:    gateBumpOps(nOps),
		}},
	}
	hash, err := network.HashTransactionInEnvelope(env, gateTestPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	if unreadable {
		hash[0] ^= 0xff
	}
	opResults := make([]xdr.OperationResult, nOps)
	for i := range opResults {
		opResults[i] = xdr.OperationResult{Code: xdr.OperationResultCodeOpInner, Tr: &xdr.OperationResultTr{
			Type:          xdr.OperationTypeBumpSequence,
			BumpSeqResult: &xdr.BumpSequenceResult{Code: xdr.BumpSequenceResultCodeBumpSequenceSuccess},
		}}
	}
	proc := xdr.TransactionResultMeta{
		Result: xdr.TransactionResultPair{
			TransactionHash: xdr.Hash(hash),
			Result: xdr.TransactionResult{
				FeeCharged: 100,
				Result: xdr.TransactionResultResult{
					Code:    xdr.TransactionResultCodeTxSuccess,
					Results: &opResults,
				},
			},
		},
		TxApplyProcessing: xdr.TransactionMeta{
			V:  3,
			V3: &xdr.TransactionMetaV3{Operations: make([]xdr.OperationMeta, nOps)},
		},
	}
	return env, proc
}

func gateLCM(envs []xdr.TransactionEnvelope, procs []xdr.TransactionResultMeta) xdr.LedgerCloseMeta {
	return xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{Header: xdr.LedgerHeader{
				LedgerSeq: 777,
				ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(1_700_000_000)},
			}},
			TxSet: xdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &xdr.TransactionSetV1{Phases: []xdr.TransactionPhase{{
					V: 0,
					V0Components: &[]xdr.TxSetComponent{{
						Type:                  xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
						TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{Txs: envs},
					}},
				}}},
			},
			TxProcessing: procs,
		},
	}
}

// TestGateLedger_UnreadableTxIsCountedAndFlagged pins that the gate's tx/op
// census comes from the LCM, not from the extract it checks. ExtractLedger
// skips a tx the reader cannot decode and so never counts it; a census taken
// from extract's own TxCount/OpCount agreed with that drop and the gate
// certified a ledger missing a transaction and its operations.
func TestGateLedger_UnreadableTxIsCountedAndFlagged(t *testing.T) {
	okEnv, okProc := gateTx(t, 0x11, 1, false)
	badEnv, badProc := gateTx(t, 0x22, 2, true)
	lcm := gateLCM([]xdr.TransactionEnvelope{okEnv, badEnv}, []xdr.TransactionResultMeta{okProc, badProc})

	want, mismatch, err := gateLedger(lcm, gateTestPassphrase)
	if err != nil {
		t.Fatalf("gateLedger: %v", err)
	}
	if want.tx != 2 || want.op != 3 {
		t.Errorf("census tx=%d op=%d, want tx=2 op=3 (the unreadable tx and its 2 ops are on chain)", want.tx, want.op)
	}
	if !strings.Contains(mismatch, "txs census=2 extract=1") || !strings.Contains(mismatch, "ops census=3 extract=1") {
		t.Errorf("mismatch = %q, want the extract's dropped tx and ops reported", mismatch)
	}
}

func TestGateLedger_CleanLedgerAgrees(t *testing.T) {
	aEnv, aProc := gateTx(t, 0x11, 1, false)
	bEnv, bProc := gateTx(t, 0x22, 2, false)
	lcm := gateLCM([]xdr.TransactionEnvelope{aEnv, bEnv}, []xdr.TransactionResultMeta{aProc, bProc})

	want, mismatch, err := gateLedger(lcm, gateTestPassphrase)
	if err != nil {
		t.Fatalf("gateLedger: %v", err)
	}
	if want != (gateCounts{tx: 2, op: 3}) {
		t.Errorf("census = %+v, want tx=2 op=3 events=0 trades=0", want)
	}
	if mismatch != "" {
		t.Errorf("mismatch = %q on a fully readable ledger, want none", mismatch)
	}
}
