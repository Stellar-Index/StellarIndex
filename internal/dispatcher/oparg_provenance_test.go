package dispatcher

import (
	"encoding/base64"
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── OpArgs provenance gate + lazy state-write enrichment ────────────
//
// The op's top-level InvokeContract args belong to the CALLEE of that
// top-level call. Pre-gate, ProcessLedger attached them to EVERY event
// the op produced — including events emitted by OTHER contracts reached
// as sub-invocations. That let a wrapper contract call
// adapter.write_prices with the genuine signed payload while its own
// (attacker-chosen) top-level args were handed to the adapter-event
// decoder as if they were the write_prices args — feed-attribution
// steering. These tests pin the gate at the ProcessLedger level: args
// attach exactly when invoked-contract == emitting-contract.

// provContracts: A is the invoked (callee) contract, B a sub-invoked
// contract emitting in the same op.
var (
	provContractA = xdr.ContractId{0xA1, 0x01}
	provContractB = xdr.ContractId{0xB2, 0x02}
)

// provSpyDecoder records every event routed to it. wantWrites opts the
// decoder into the StateWriteKeyConsumer enrichment for `contract`.
type provSpyDecoder struct {
	name       string
	contract   string
	wantWrites bool
	got        []events.Event
}

func (p *provSpyDecoder) Name() string { return p.name }
func (p *provSpyDecoder) Matches(ev events.Event) bool {
	return ev.ContractID == p.contract
}

func (p *provSpyDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	p.got = append(p.got, ev)
	return nil, nil
}

func (p *provSpyDecoder) StateWriteContracts() []string {
	if !p.wantWrites {
		return nil
	}
	return []string{p.contract}
}

// provEvent builds one capture-eligible contract event for cid.
func provEvent(sym string, cid xdr.ContractId) xdr.ContractEvent {
	s := xdr.ScSymbol(sym)
	u := xdr.Uint32(7)
	return xdr.ContractEvent{
		Type:       xdr.ContractEventTypeContract,
		ContractId: &cid,
		Body: xdr.ContractEventBody{
			V: 0,
			V0: &xdr.ContractEventV0{
				Topics: []xdr.ScVal{{Type: xdr.ScValTypeScvSymbol, Sym: &s}},
				Data:   xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u},
			},
		},
	}
}

// provInvokeOp builds an InvokeHostFunction op whose top-level call
// targets `cid` with one string arg.
func provInvokeOp(t *testing.T, cid xdr.ContractId, arg string) (xdr.Operation, string) {
	t.Helper()
	s := xdr.ScString(arg)
	op := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: &xdr.InvokeContractArgs{
						ContractAddress: xdr.ScAddress{
							Type:       xdr.ScAddressTypeScAddressTypeContract,
							ContractId: &cid,
						},
						FunctionName: "write_prices",
						Args:         []xdr.ScVal{{Type: xdr.ScValTypeScvString, Str: &s}},
					},
				},
			},
		},
	}
	raw, err := op.Body.InvokeHostFunctionOp.HostFunction.InvokeContract.Args[0].MarshalBinary()
	if err != nil {
		t.Fatalf("marshal arg: %v", err)
	}
	return op, base64.StdEncoding.EncodeToString(raw)
}

// provLedger assembles a one-tx LCM: the tx carries `op`, succeeds, and
// its V4 meta stamps `evs` on the operation along with `changes`.
func provLedger(t *testing.T, op xdr.Operation, evs []xdr.ContractEvent, changes []xdr.LedgerEntryChange) xdr.LedgerCloseMeta {
	t.Helper()
	var srcSeed [32]byte
	srcSeed[0] = 0x77
	srcMuxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256(srcSeed))
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	envelope := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{
			Tx: xdr.Transaction{
				SourceAccount: srcMuxed,
				Fee:           100,
				SeqNum:        1,
				Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
				Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
				Operations:    []xdr.Operation{op},
			},
		},
	}
	hash, err := network.HashTransactionInEnvelope(envelope, testPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	opResults := []xdr.OperationResult{{Code: xdr.OperationResultCodeOpInner}}
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
			V: 4,
			V4: &xdr.TransactionMetaV4{
				Operations: []xdr.OperationMetaV2{{
					Changes: changes,
					Events:  evs,
				}},
			},
		},
	}
	return mkEntryWalkLedger(9_000_001, []xdr.TransactionEnvelope{envelope}, []xdr.TransactionResultMeta{proc})
}

func TestProcessLedger_OpArgsAttachOnlyToCalleeEvents(t *testing.T) {
	op, argB64 := provInvokeOp(t, provContractA, "feed-vec-sentinel")
	lcm := provLedger(t, op, []xdr.ContractEvent{
		provEvent("REDSTONE", provContractA), // emitted by the callee
		provEvent("REDSTONE", provContractB), // emitted by a sub-invoked contract
	}, nil)

	strA, err := contractIDToStrkey(provContractA)
	if err != nil {
		t.Fatal(err)
	}
	strB, err := contractIDToStrkey(provContractB)
	if err != nil {
		t.Fatal(err)
	}
	spyA := &provSpyDecoder{name: "spyA", contract: strA}
	spyB := &provSpyDecoder{name: "spyB", contract: strB}
	disp := New(spyA, spyB)
	if _, err := disp.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	if len(spyA.got) != 1 || len(spyB.got) != 1 {
		t.Fatalf("routed events: A=%d B=%d, want 1 each", len(spyA.got), len(spyB.got))
	}
	// Callee's event carries the call's args…
	if len(spyA.got[0].OpArgs) != 1 || spyA.got[0].OpArgs[0] != argB64 {
		t.Errorf("callee event OpArgs = %v, want [%s]", spyA.got[0].OpArgs, argB64)
	}
	// …and the foreign (sub-invoked) contract's event carries NONE:
	// the top-level args describe a call into A, not into B. Handing
	// them to B's decoder is the feed-steering vector this gate closes.
	if len(spyB.got[0].OpArgs) != 0 {
		t.Errorf("foreign-contract event OpArgs = %v, want none (args belong to the callee)", spyB.got[0].OpArgs)
	}
}

func TestProcessLedger_StateWriteKeysOnlyForDeclaredConsumers(t *testing.T) {
	op, _ := provInvokeOp(t, provContractA, "x")
	// Both contracts change one of their own contract-data entries in
	// the op; only A's decoder declares StateWriteKeyConsumer interest.
	changes := []xdr.LedgerEntryChange{
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryCreated, swkDataEntry(provContractA, "FEED_A", 1)),
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryCreated, swkDataEntry(provContractB, "FEED_B", 2)),
	}
	lcm := provLedger(t, op, []xdr.ContractEvent{
		provEvent("REDSTONE", provContractA),
		provEvent("REDSTONE", provContractB),
	}, changes)

	strA, err := contractIDToStrkey(provContractA)
	if err != nil {
		t.Fatal(err)
	}
	strB, err := contractIDToStrkey(provContractB)
	if err != nil {
		t.Fatal(err)
	}
	spyA := &provSpyDecoder{name: "spyA", contract: strA, wantWrites: true}
	spyB := &provSpyDecoder{name: "spyB", contract: strB} // no interest declared
	disp := New(spyA, spyB)
	if _, err := disp.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	if len(spyA.got) != 1 || len(spyB.got) != 1 {
		t.Fatalf("routed events: A=%d B=%d, want 1 each", len(spyA.got), len(spyB.got))
	}
	// Declared consumer gets its own contract's changed keys.
	if len(spyA.got[0].StateWriteKeys) != 1 {
		t.Fatalf("A StateWriteKeys = %v, want exactly FEED_A's key", spyA.got[0].StateWriteKeys)
	}
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(spyA.got[0].StateWriteKeys[0], &lk); err != nil {
		t.Fatalf("key round-trip: %v", err)
	}
	if cd, ok := lk.GetContractData(); !ok {
		t.Fatalf("key type = %v, want contract data", lk.Type)
	} else if s, _ := cd.Key.GetStr(); string(s) != "FEED_A" {
		t.Errorf("key = %q, want FEED_A", s)
	}
	// Undeclared decoder's event carries nil — "unknown", by contract.
	if spyB.got[0].StateWriteKeys != nil {
		t.Errorf("B StateWriteKeys = %v, want nil (no StateWriteKeyConsumer declared)", spyB.got[0].StateWriteKeys)
	}
}
