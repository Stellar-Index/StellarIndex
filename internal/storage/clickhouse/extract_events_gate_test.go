// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/strkey"
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

// TestExtractEvents_OpArgsProvenanceGate pins the lake-side twin of the
// dispatcher's OpArgs provenance rule: an op's top-level InvokeContract
// args are stored ONLY on events emitted by the invoked contract itself.
// Pre-gate, every event the op produced — including events emitted by
// OTHER contracts reached as sub-invocations of a wrapper — carried the
// wrapper's (attacker-chosen) top-level args in op_args_xdr, which is
// exactly what Redstone's projector reads feed identity out of.
func TestExtractEvents_OpArgsProvenanceGate(t *testing.T) {
	calleeID := xdr.ContractId{0xCA, 0x11}
	otherID := xdr.ContractId{0x0B, 0x22}
	arg := xdr.ScString("feed-vec-sentinel")
	op := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: &xdr.InvokeContractArgs{
						ContractAddress: xdr.ScAddress{
							Type:       xdr.ScAddressTypeScAddressTypeContract,
							ContractId: &calleeID,
						},
						FunctionName: "write_prices",
						Args:         []xdr.ScVal{{Type: xdr.ScValTypeScvString, Str: &arg}},
					},
				},
			},
		},
	}
	opArgs := opArgsByIndex([]xdr.Operation{op})
	if len(opArgs) != 1 || opArgs[0] == nil {
		t.Fatalf("opArgsByIndex = %+v, want one populated slot", opArgs)
	}
	wantCallee, err := strkey.Encode(strkey.VersionByteContract, calleeID[:])
	if err != nil {
		t.Fatal(err)
	}
	if opArgs[0].ContractID != wantCallee || len(opArgs[0].Args) != 1 {
		t.Fatalf("opArgs[0] = %+v, want callee %s with 1 arg", opArgs[0], wantCallee)
	}

	mkEv := func(cid xdr.ContractId) xdr.ContractEvent {
		ev := gateContractEvent(t)
		c := cid
		ev.ContractId = &c
		return ev
	}
	tx := gateTx(t, mkEv(calleeID), true)
	tx.UnsafeMeta.V4.Operations[0].Events = append(tx.UnsafeMeta.V4.Operations[0].Events, mkEv(otherID))

	ext := &LedgerExtract{}
	extractEvents(ext, tx, 42, time.Unix(1_700_000_000, 0).UTC(), "txok", opArgs, true)
	if len(ext.Events) != 2 {
		t.Fatalf("Events = %d, want 2", len(ext.Events))
	}
	if got := ext.Events[0].OpArgsXDR; len(got) != 1 {
		t.Errorf("callee event op_args_xdr = %v, want the call's 1 arg", got)
	}
	if got := ext.Events[1].OpArgsXDR; len(got) != 0 {
		t.Errorf("foreign-contract event op_args_xdr = %v, want empty (args belong to the callee)", got)
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
