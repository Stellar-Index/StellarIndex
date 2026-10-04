// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"context"
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func cmpAccountChange(t *testing.T, seed byte, balance int64) xdr.LedgerEntryChange {
	t.Helper()
	id, err := xdr.NewAccountId(xdr.PublicKeyTypePublicKeyTypeEd25519, xdr.Uint256{seed})
	if err != nil {
		t.Fatalf("NewAccountId: %v", err)
	}
	return xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		Updated: &xdr.LedgerEntry{Data: xdr.LedgerEntryData{
			Type:    xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{AccountId: id, Balance: xdr.Int64(balance)},
		}},
	}
}

// cmpLedger is a one-transaction ledger whose single operation applied block.
func cmpLedger(t *testing.T, seq uint32, block []xdr.LedgerEntryChange) xdr.LedgerCloseMeta {
	t.Helper()
	src, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256{0x42})
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	env := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{Tx: xdr.Transaction{
			SourceAccount: src,
			Fee:           100,
			SeqNum:        xdr.SequenceNumber(seq),
			Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
			Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
			Operations:    []xdr.Operation{},
		}},
	}
	hash, err := network.HashTransactionInEnvelope(env, network.TestNetworkPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	return xdr.LedgerCloseMeta{V: 1, V1: &xdr.LedgerCloseMetaV1{
		LedgerHeader: xdr.LedgerHeaderHistoryEntry{Header: xdr.LedgerHeader{
			LedgerSeq: xdr.Uint32(seq),
			ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(1_700_000_000)},
		}},
		TxSet: xdr.GeneralizedTransactionSet{V: 1, V1TxSet: &xdr.TransactionSetV1{
			Phases: []xdr.TransactionPhase{{V: 0, V0Components: &[]xdr.TxSetComponent{{
				Type:                  xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
				TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{Txs: []xdr.TransactionEnvelope{env}},
			}}}},
		}},
		TxProcessing: []xdr.TransactionResultMeta{{
			Result: xdr.TransactionResultPair{
				TransactionHash: xdr.Hash(hash),
				Result: xdr.TransactionResult{
					FeeCharged: 100,
					Result:     xdr.TransactionResultResult{Code: xdr.TransactionResultCodeTxSuccess, Results: &[]xdr.OperationResult{}},
				},
			},
			TxApplyProcessing: xdr.TransactionMeta{V: 3, V3: &xdr.TransactionMetaV3{
				Operations: []xdr.OperationMeta{{Changes: block}},
			}},
		}},
	}}
}

func fakeSource(ledgers ...xdr.LedgerCloseMeta) ledgerSource {
	return func(_ context.Context, from, to uint32, cb func(xdr.LedgerCloseMeta) error) error {
		for _, l := range ledgers {
			if s := l.LedgerSequence(); s >= from && s <= to {
				if err := cb(l); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

// Two exports listing one ledger's block in different key orders must compare
// equal: the re-extract has to be identical whichever export it read.
func TestCompareEntryChangeRange_ExportOrderIsNotADifference(t *testing.T) {
	a, b, c := cmpAccountChange(t, 1, 10), cmpAccountChange(t, 2, 20), cmpAccountChange(t, 3, 30)
	ours := fakeSource(cmpLedger(t, 10, []xdr.LedgerEntryChange{a, b, c}), cmpLedger(t, 11, []xdr.LedgerEntryChange{b}))
	aws := fakeSource(cmpLedger(t, 10, []xdr.LedgerEntryChange{c, a, b}), cmpLedger(t, 11, []xdr.LedgerEntryChange{b}))

	rep, err := compareEntryChangeRange(context.Background(), ours, aws, network.TestNetworkPassphrase, 10, 11, 1)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if rep.Differences != 0 {
		t.Fatalf("differences = %d (%+v), want 0 — export order alone must not change the extracted rows", rep.Differences, rep.Sample)
	}
	if rep.Ledgers != 2 || rep.RowsOurs != 4 || rep.RowsAWS != 4 {
		t.Fatalf("compared %d ledgers, %d/%d rows; want 2 ledgers, 4/4 rows", rep.Ledgers, rep.RowsOurs, rep.RowsAWS)
	}
}

func TestCompareEntryChangeRange_ReportsContentAndMissingLedgers(t *testing.T) {
	a, b := cmpAccountChange(t, 1, 10), cmpAccountChange(t, 2, 20)
	ours := fakeSource(cmpLedger(t, 10, []xdr.LedgerEntryChange{a, b}), cmpLedger(t, 11, []xdr.LedgerEntryChange{a}))
	aws := fakeSource(cmpLedger(t, 10, []xdr.LedgerEntryChange{a, cmpAccountChange(t, 2, 21)}))

	rep, err := compareEntryChangeRange(context.Background(), ours, aws, network.TestNetworkPassphrase, 10, 11, 100)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if rep.Differences != 2 || len(rep.Sample) != 2 {
		t.Fatalf("differences = %d (%+v), want 2: one changed row, one missing ledger", rep.Differences, rep.Sample)
	}
	if got := rep.Sample[0]; got.Ledger != 10 || got.Position != 1 {
		t.Errorf("first difference = %+v, want ledger 10 position 1 (the changed balance)", got)
	}
	if got := rep.Sample[1]; got.Ledger != 11 || got.Position != -1 || got.Detail != "missing from the AWS export" {
		t.Errorf("second difference = %+v, want ledger 11 missing from the AWS export", got)
	}
}
