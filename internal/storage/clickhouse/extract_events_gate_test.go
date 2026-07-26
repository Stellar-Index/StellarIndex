// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// gateContractEvent builds one capture-eligible contract event whose
// topic_0 is a supply-flow symbol ("mint") carrying an i128 amount — so
// the fixture exercises BOTH the contract_events row and the
// supply_flows row extractEvents derives from it.
func gateContractEvent(t *testing.T) xdr.ContractEvent {
	t.Helper()
	var cid xdr.ContractId
	cid[0] = 0x42
	sym := xdr.ScSymbol("mint")
	amount := xdr.Int128Parts{Hi: 0, Lo: 5_000_000}
	return xdr.ContractEvent{
		Type:       xdr.ContractEventTypeContract,
		ContractId: &cid,
		Body: xdr.ContractEventBody{
			V: 0,
			V0: &xdr.ContractEventV0{
				Topics: []xdr.ScVal{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}},
				Data:   xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &amount},
			},
		},
	}
}

// gateTx builds a meta-V4 LedgerTransaction whose single operation meta
// carries `ev`, with the transaction result set to success or failure.
//
// Note the fixture is deliberately SYNTHETIC on the failure arm: real
// stellar-core leaves TransactionMetaV4.Operations nil for a failed tx
// (see xdr.TransactionMeta.GetContractEventsForOperation's own comment,
// "For a failed transaction, txMeta.Operations slice will be nil"). This
// test pins the EXTRACTOR'S gate, not core's meta shape — the gate has
// to hold whether or not the protocol also happens to.
func gateTx(t *testing.T, ev xdr.ContractEvent, successful bool) ingest.LedgerTransaction {
	t.Helper()
	code := xdr.TransactionResultCodeTxSuccess
	if !successful {
		code = xdr.TransactionResultCodeTxFailed
	}
	var opResults []xdr.OperationResult
	if successful {
		opResults = []xdr.OperationResult{}
	}
	return ingest.LedgerTransaction{
		Index: 1,
		Envelope: xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTx,
			V1: &xdr.TransactionV1Envelope{
				Tx: xdr.Transaction{SourceAccount: xdr.MustMuxedAddress(ecTestG)},
			},
		},
		Result: xdr.TransactionResultPair{
			Result: xdr.TransactionResult{
				Result: xdr.TransactionResultResult{Code: code, Results: &opResults},
			},
		},
		UnsafeMeta: xdr.TransactionMeta{
			V: 4,
			V4: &xdr.TransactionMetaV4{
				Operations: []xdr.OperationMetaV2{{Events: []xdr.ContractEvent{ev}}},
			},
		},
	}
}

// TestExtractEvents_TxSuccessGate is the regression guard for the
// C2-010 sibling (audit-2026-07-23): extractEvents was the ONLY one of
// the three ledger walks with no tx-success gate.
// dispatcher.ProcessLedger skips failed txs before dispatching, and
// dispatcher.CensusLedger skips them before counting — so the lake's
// soroban_event_count was computed over a strictly larger population
// than the census oracle it is reconciled against, while eventRow
// stamped `in_successful_call = 1` on every row it wrote.
//
// Asserted here on the SAME event, twice: it must land (row + count +
// supply_flow) when the tx succeeded, and produce nothing at all when it
// failed. The supply_flows half matters independently — a failed-tx mint
// that reached the lake would credit tokens the chain never minted.
func TestExtractEvents_TxSuccessGate(t *testing.T) {
	ev := gateContractEvent(t)
	closeTime := time.Unix(1_700_000_000, 0).UTC()

	t.Run("successful tx: event captured", func(t *testing.T) {
		ext := &LedgerExtract{}
		extractEvents(ext, gateTx(t, ev, true), 42, closeTime, "txok", nil, true)

		if len(ext.Events) != 1 {
			t.Fatalf("Events = %d, want 1", len(ext.Events))
		}
		if ext.Ledger.SorobanEventCount != 1 {
			t.Errorf("SorobanEventCount = %d, want 1", ext.Ledger.SorobanEventCount)
		}
		if got := ext.Events[0].InSuccessfulCall; got != 1 {
			t.Errorf("in_successful_call = %d, want 1", got)
		}
		if len(ext.SupplyFlows) != 1 {
			t.Fatalf("SupplyFlows = %d, want 1 (mint decodes to a supply flow)", len(ext.SupplyFlows))
		}
		if ext.SupplyFlows[0].Kind != "mint" {
			t.Errorf("supply flow kind = %q, want mint", ext.SupplyFlows[0].Kind)
		}
	})

	t.Run("failed tx: nothing captured", func(t *testing.T) {
		ext := &LedgerExtract{}
		extractEvents(ext, gateTx(t, ev, false), 42, closeTime, "txfail", nil, false)

		if len(ext.Events) != 0 {
			t.Errorf("Events = %d, want 0 — a failed transaction's contract events are not "+
				"dispatched and not counted by the census, so the lake must not write them either "+
				"(ledgers.soroban_event_count == COUNT(contract_events) is what ReadGateCounts checks)",
				len(ext.Events))
		}
		if ext.Ledger.SorobanEventCount != 0 {
			t.Errorf("SorobanEventCount = %d, want 0 — this is the count ch-gate compares "+
				"against census.SorobanEventCount for the same LCM", ext.Ledger.SorobanEventCount)
		}
		if len(ext.SupplyFlows) != 0 {
			t.Errorf("SupplyFlows = %d, want 0 — a rolled-back mint never happened; crediting it "+
				"would inflate per-token circulating supply", len(ext.SupplyFlows))
		}
		if ext.TxEventReadErrors != 0 {
			t.Errorf("TxEventReadErrors = %d, want 0 — a gated failed tx is a skip, not a read error",
				ext.TxEventReadErrors)
		}
	})
}
