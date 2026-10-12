package band

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Oracle auth-tree price forgery: auth entries need not be exercised, so an attacker can declare a Band relay() with
// forged rates in an unrelated tx. Driving the real decoder through the real dispatcher auth-tree walk: a forged,
// never-executed entry yields zero prices (counted uncorroborated); a genuine top-level relay still prices.

const testPassphrase = network.TestNetworkPassphrase

// relayArgsScVals builds a relay() argument vector (from, symbol_rates, resolve_time, request_id) as raw ScVals,
// usable as a genuine call or a forged auth entry.
func relayArgsScVals(t *testing.T, symbol string, rateE9, resolveSec, requestID uint64) []xdr.ScVal {
	t.Helper()

	// from: Address(relayerG)
	raw, err := strkey.Decode(strkey.VersionByteAccountID, relayerG)
	if err != nil {
		t.Fatalf("decode relayer strkey: %v", err)
	}
	var pub xdr.Uint256
	copy(pub[:], raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	fromAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	fromSv := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &fromAddr}

	// symbol_rates: Vec<(Symbol, u64)>
	sym := xdr.ScSymbol(symbol)
	symSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
	rate := xdr.Uint64(rateE9)
	rateSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &rate}
	tuple := xdr.ScVec{symSv, rateSv}
	pt := &tuple
	tupleSv := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pt}
	outer := xdr.ScVec{tupleSv}
	po := &outer
	ratesSv := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &po}

	resolve := xdr.Uint64(resolveSec)
	resolveSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &resolve}
	req := xdr.Uint64(requestID)
	reqSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &req}

	return []xdr.ScVal{fromSv, ratesSv, resolveSv, reqSv}
}

// contractIDFromStrkey turns a C-strkey into its xdr.ContractId.
func contractIDFromStrkey(t *testing.T, c string) xdr.ContractId {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, c)
	if err != nil {
		t.Fatalf("decode contract strkey: %v", err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	return cid
}

func invokeContractArgs(cid xdr.ContractId, fn string, args []xdr.ScVal) *xdr.InvokeContractArgs {
	return &xdr.InvokeContractArgs{
		ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
		FunctionName:    xdr.ScSymbol(fn),
		Args:            args,
	}
}

// oneTxSorobanLedger wraps a single successful InvokeHostFunction op in
// an LCM the dispatcher will process.
func oneTxSorobanLedger(t *testing.T, op xdr.Operation) xdr.LedgerCloseMeta {
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
			V:  4,
			V4: &xdr.TransactionMetaV4{Operations: []xdr.OperationMetaV2{{}}},
		},
	}
	return xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(52_000_001),
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(1_745_000_100)},
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &xdr.TransactionSetV1{
					Phases: []xdr.TransactionPhase{{
						V: 0,
						V0Components: &[]xdr.TxSetComponent{{
							Type: xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
							TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{
								Txs: []xdr.TransactionEnvelope{envelope},
							},
						}},
					}},
				},
			},
			TxProcessing: []xdr.TransactionResultMeta{proc},
		},
	}
}

func countBandUpdates(outs []interface{ Source() string }) int {
	n := 0
	for _, o := range outs {
		if o.Source() == SourceName {
			n++
		}
	}
	return n
}

// TestProcessLedger_ForgedBandAuthEntry_NotRecognisedAsPrice: a benign call to a non-Band contract carrying a forged
// auth entry naming Band StandardReference with fabricated relay() rates never executed, so no price may be decoded.
func TestProcessLedger_ForgedBandAuthEntry_NotRecognisedAsPrice(t *testing.T) {
	const forgedRateE9 = uint64(1_000_000_000_000_000) // $1,000,000/BTC — the fabricated print
	bandCID := contractIDFromStrkey(t, adapterC)
	var benignCID xdr.ContractId
	for i := range benignCID {
		benignCID[i] = 0x5A
	}

	forgedArgs := relayArgsScVals(t, "BTC", forgedRateE9, 1_745_000_000, 7)

	// Top level: a benign call the attacker is actually allowed to make.
	op := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type:           xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: invokeContractArgs(benignCID, "noop", nil),
				},
				// The forgery: a source-account auth entry (authorized by
				// the attacker's own tx signature, no separate proof)
				// declaring a Band relay() that never runs.
				Auth: []xdr.SorobanAuthorizationEntry{{
					Credentials: xdr.SorobanCredentials{
						Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount,
					},
					RootInvocation: xdr.SorobanAuthorizedInvocation{
						Function: xdr.SorobanAuthorizedFunction{
							Type:       xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
							ContractFn: invokeContractArgs(bandCID, FnRelay, forgedArgs),
						},
					},
				}},
			},
		},
	}

	lcm := oneTxSorobanLedger(t, op)
	d := dispatcher.New()
	d.AddContractCallDecoder(NewDecoder(adapterC))

	outs, err := d.ProcessLedger(lcm, testPassphrase)
	if err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	events := make([]interface{ Source() string }, 0, len(outs))
	for _, o := range outs {
		if e, ok := o.(interface{ Source() string }); ok {
			events = append(events, e)
		}
	}
	if got := countBandUpdates(events); got != 0 {
		t.Fatalf("forged auth-only Band relay produced %d price event(s), want 0 — "+
			"an unexecuted, attacker-declared invocation was laundered into an oracle price (W8.4a)", got)
	}
	if got := d.Stats().UncorroboratedCalls[SourceName]; got != 1 {
		t.Errorf("Stats().UncorroboratedCalls[%q] = %d, want 1 — the rejection must be counted, not silent", SourceName, got)
	}
}

// TestProcessLedger_GenuineTopLevelBandRelay_IsRecognised is the non-vacuous companion: a real
// top-level EXECUTED relayer relay() call still yields its price, so the gate cannot "pass" by
// dropping all Band traffic.
func TestProcessLedger_GenuineTopLevelBandRelay_IsRecognised(t *testing.T) {
	const rateE9 = uint64(500_000_000_000_000)
	bandCID := contractIDFromStrkey(t, adapterC)
	args := relayArgsScVals(t, "BTC", rateE9, 1_745_000_000, 42)

	op := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type:           xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: invokeContractArgs(bandCID, FnRelay, args),
				},
				// Genuine relay: the call IS the top-level executed op.
				Auth: nil,
			},
		},
	}

	lcm := oneTxSorobanLedger(t, op)
	d := dispatcher.New()
	d.AddContractCallDecoder(NewDecoder(adapterC))

	outs, err := d.ProcessLedger(lcm, testPassphrase)
	if err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	events := make([]interface{ Source() string }, 0, len(outs))
	for _, o := range outs {
		if e, ok := o.(interface{ Source() string }); ok {
			events = append(events, e)
		}
	}
	if got := countBandUpdates(events); got != 1 {
		t.Fatalf("genuine top-level Band relay produced %d price event(s), want 1 — "+
			"the corroboration gate must not drop real relayer updates", got)
	}
	if got := d.Stats().UncorroboratedCalls[SourceName]; got != 0 {
		t.Errorf("Stats().UncorroboratedCalls[%q] = %d, want 0 for a genuine executed call", SourceName, got)
	}
}
