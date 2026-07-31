package clickhouse

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sdexclaim"
)

// ExtractLedger structurally decodes one LedgerCloseMeta into Tier-1 rows.
// Decoder-INDEPENDENT: it records the shape + raw XDR, not protocol meaning.
//
// Scope: ledgers, transactions, operations, operation_results, contract_events,
// supply_flows.
//
// ledger_entry_changes (was G12-03; CLOSED by ADR-0038 Phase C).
// Extract.Changes is now populated by extractEntryChanges (see
// extract_entry_changes.go): it walks the tx-meta v3/v4 op-change + fee-meta
// streams exactly as dispatcher.walkEntryChanges does (fee/tx-level at
// op_index -1, per-op changes at their index), base64s the entry+key XDR, and
// tags change/entry type. This gives the lake the LedgerEntry substrate the
// account-state explorer re-derives current balances/trustlines/offers/
// contract-data from, and fulfils ADR-0034's "re-derive the LedgerEntry supply
// observers from the lake" promise. Live capture starts when the indexer
// running this extractor is redeployed; historical coverage comes from a
// ch-rebuild over the range (billions of rows — the Phase C backfill).
//
// Resilient like dispatcher.CensusLedger: a per-tx read error is skipped +
// tolerated, not fatal, so one bad tx can't lose a whole ledger.
func ExtractLedger(lcm xdr.LedgerCloseMeta, passphrase string) (LedgerExtract, error) {
	seq := lcm.LedgerSequence()
	closeTime := lcm.ClosedAt().UTC()
	hdr := lcm.LedgerHeaderHistoryEntry().Header

	ext := LedgerExtract{
		Ledger: LedgerRow{
			LedgerSeq:       seq,
			CloseTime:       closeTime,
			LedgerHash:      hashHex(lcm.LedgerHash()),
			PrevHash:        hashHex(lcm.PreviousLedgerHash()),
			ProtocolVersion: lcm.ProtocolVersion(),
			BucketListHash:  hashHex(lcm.BucketListHash()),
			TotalCoins:      int64(hdr.TotalCoins),
			FeePool:         int64(hdr.FeePool),
			BaseFee:         uint32(hdr.BaseFee),
			BaseReserve:     uint32(hdr.BaseReserve),
		},
	}

	reader, err := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)
	if err != nil {
		return LedgerExtract{}, fmt.Errorf("clickhouse: extract reader ledger %d: %w", seq, err)
	}
	defer func() { _ = reader.Close() }()

	// Read the ledger's transactions up front: the entry-change walk is
	// ledger-wide and two-phase (all fee changes, then all apply-phase
	// changes), mirroring stellar-core's commit order and
	// dispatcher.walkLedgerEntryChanges exactly (C2-032, audit-2026-07-23).
	// LedgerTransaction holds slices into lcm, so retaining them is cheap.
	txs := make([]ingest.LedgerTransaction, 0, 64)
	for {
		tx, rerr := reader.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			// Skip + tolerate, mirroring CensusLedger — but COUNT it so a
			// silently-dropped tx is recoverable in the caller's signal
			// (the ledger still writes, keeping the lake contiguous).
			ext.TxReadErrors++
			continue
		}
		txs = append(txs, tx)
	}
	for i := range txs {
		extractTx(&ext, txs[i], seq, closeTime)
	}
	// ADR-0038 Phase C substrate (closes G12-03).
	extractLedgerEntryChanges(&ext, txs, seq, closeTime)

	return ext, nil
}

// extractTx appends one transaction's rows (the tx, its ops + results, its
// contract events) to ext and updates the per-ledger counts. The tx's
// LedgerEntryChanges are NOT appended here — they are walked ledger-wide by
// extractLedgerEntryChanges, which needs every tx before it can emit anything.
func extractTx(ext *LedgerExtract, tx ingest.LedgerTransaction, seq uint32, closeTime time.Time) {
	txIndex := tx.Index - 1 // Index is 1-based; store 0-based
	txHash := hex.EncodeToString(tx.Result.TransactionHash[:])
	txSource, _ := tx.Account()
	feeCharged, _ := tx.FeeCharged()

	ext.Txs = append(ext.Txs, TransactionRow{
		LedgerSeq:      seq,
		CloseTime:      closeTime,
		TxHash:         txHash,
		TxIndex:        txIndex,
		SourceAccount:  txSource,
		FeeCharged:     feeCharged,
		MaxFee:         int64(tx.MaxFee()),
		OperationCount: uint16(tx.OperationCount()),
		Successful:     b2u8(tx.Result.Successful()),
		ResultCode:     int32(tx.Result.Result.Result.Code),
		MemoType:       tx.MemoType(),
		Memo:           tx.Memo(),
	})
	ext.Ledger.TxCount++

	extractOps(ext, tx, seq, closeTime, txHash, txSource, txIndex, tx.Result.Successful())
	extractEvents(ext, tx, seq, closeTime, txHash, opArgsByIndex(tx.Envelope.Operations()), tx.Result.Successful())
}

// opInvokeArgs is one operation's top-level InvokeContract snapshot:
// the invoked contract's C-strkey plus its base64-SCVal args. The
// contract identity is what lets extractEvents apply the OpArgs
// provenance gate (args attach only to events the CALLEE itself
// emitted — the lake-side twin of the dispatcher's gate).
type opInvokeArgs struct {
	ContractID string
	Args       []string
}

// opArgsByIndex returns the top-level InvokeContract call per operation
// index (nil for non-InvokeContract ops). Mirrors the OpArgs side of
// dispatcher.extractInvokeContractCalls exactly (same MarshalBinary +
// base64.Std), so an event's op_args_xdr equals events.Event.OpArgs — which
// decoders that need the invoking call's args read (Redstone zips feed_ids
// from here; the event body carries none). A slot whose contract address
// can't be strkey-encoded yields nil (no args rather than args with
// unverifiable provenance).
func opArgsByIndex(ops []xdr.Operation) []*opInvokeArgs {
	out := make([]*opInvokeArgs, len(ops))
	for i := range ops {
		if ops[i].Body.Type != xdr.OperationTypeInvokeHostFunction {
			continue
		}
		ihf, ok := ops[i].Body.GetInvokeHostFunctionOp()
		if !ok || ihf.HostFunction.Type != xdr.HostFunctionTypeHostFunctionTypeInvokeContract {
			continue
		}
		ic, ok := ihf.HostFunction.GetInvokeContract()
		if !ok {
			continue
		}
		if ic.ContractAddress.Type != xdr.ScAddressTypeScAddressTypeContract {
			continue // account-typed target is protocol-invalid; skip defensively
		}
		callee := ic.ContractAddress.MustContractId()
		calleeStr, serr := strkey.Encode(strkey.VersionByteContract, callee[:])
		if serr != nil {
			continue
		}
		args := make([]string, 0, len(ic.Args))
		argsOK := true
		for j := range ic.Args {
			raw, merr := ic.Args[j].MarshalBinary()
			if merr != nil {
				argsOK = false
				break
			}
			args = append(args, base64.StdEncoding.EncodeToString(raw))
		}
		if argsOK && len(args) > 0 {
			out[i] = &opInvokeArgs{ContractID: calleeStr, Args: args}
		}
	}
	return out
}

// extractOps appends one tx's operation + operation_result rows and updates
// the op + classic-trade-effect counts.
func extractOps(ext *LedgerExtract, tx ingest.LedgerTransaction, seq uint32, closeTime time.Time, txHash, txSource string, txIndex uint32, successful bool) {
	ops := tx.Envelope.Operations()
	opResults, hasResults := tx.Result.OperationResults()
	for i := range ops {
		op := ops[i]
		opSource := txSource
		if op.SourceAccount != nil {
			opSource = op.SourceAccount.ToAccountId().Address()
		}
		body, berr := xdr.MarshalBase64(op.Body)
		if berr != nil {
			continue
		}
		ext.Ops = append(ext.Ops, OperationRow{
			LedgerSeq:     seq,
			CloseTime:     closeTime,
			TxHash:        txHash,
			TxIndex:       txIndex,
			OpIndex:       uint32(i),
			OpType:        op.Body.Type.String(),
			SourceAccount: opSource,
			BodyXDR:       body,
		})
		ext.Ledger.OpCount++

		// ADR-0038 Phase B: index the op's NON-source participants (the
		// incoming/counterparty accounts in the op body — payment dest,
		// trustor, merge target, clawback victim, …) so account history
		// covers received activity, not just sourced. The op source stays
		// in operations.source_account; the reader unions the two. Shared
		// with ch-participant-backfill via operationParticipantRows so live
		// capture and the historical re-derive can never drift. A decode
		// failure soft-skips (perr != nil) — the raw op still wrote above.
		if prs, perr := operationParticipantRows(body, opSource, seq, closeTime, txHash, txIndex, uint32(i)); perr == nil {
			ext.Participants = append(ext.Participants, prs...)
		}
		if hasResults && i < len(opResults) {
			appendOpResult(ext, seq, txHash, uint32(i), opResults[i]) // capture all op results (incl. failed) for the lake
			if successful {
				// classic_trade_effect_count mirrors the census/SDEX count,
				// which only counts trades in SUCCESSFUL txs (rolled-back ops
				// in a failed tx show success codes but never happened).
				ext.Ledger.ClassicTradeEffectCount += uint32(claimAtomCount(op, opResults[i]))
			}
		}
	}
}

func appendOpResult(ext *LedgerExtract, seq uint32, txHash string, opIndex uint32, res xdr.OperationResult) {
	rxdr, xerr := xdr.MarshalBase64(res)
	if xerr != nil {
		return
	}
	ext.Results = append(ext.Results, OperationResultRow{
		LedgerSeq:  seq,
		TxHash:     txHash,
		OpIndex:    opIndex,
		ResultCode: int32(res.Code),
		ResultXDR:  rxdr,
	})
}

// extractEvents appends one tx's eligible contract-event rows. opArgs holds
// the top-level InvokeContract call per operation index (from opArgsByIndex);
// an event of op i carries opArgs[i].Args ONLY when the event's own contract
// IS opArgs[i].ContractID — the OpArgs provenance gate, identical to the
// dispatcher's (dispatcher.ProcessLedger): the top-level args belong to the
// callee, and attaching them to events emitted by OTHER contracts in the same
// op (sub-invocations under a wrapper) hands attacker-chosen wrapper args to
// decoders like Redstone that read feed identity out of them. Wrapper-reached
// events land with empty op_args_xdr, so an args-requiring decoder refuses
// honestly at projection time instead of misattributing.
//
// TX-SUCCESS GATE (C2-010 sibling, audit-2026-07-23). `successful` is
// tx.Result.Successful(); a failed transaction contributes nothing. Pre-fix
// this was the ONLY one of the three ledger walks with no such gate —
// dispatcher.ProcessLedger skips failed txs outright (dispatcher.go, "Failed
// transactions don't produce real price signal") and dispatcher.CensusLedger
// does the same before counting — so the lake's soroban_event_count and the
// census oracle it is reconciled against were computed over different
// populations, and eventRow stamped `in_successful_call = 1` on every row
// regardless. Three invariants now hold by construction instead of by
// coincidence:
//
//   - ledgers.soroban_event_count == COUNT(contract_events) for the range
//     (what ReadGateCounts compares);
//   - ext.Ledger.SorobanEventCount == census.SorobanEventCount for the same
//     LCM (what ch-gate's EXTRACT≠CENSUS check compares);
//   - in_successful_call = 1 is TRUE of every row written, rather than a
//     hard-coded constant.
//
// The rejected alternative was to keep the rows and stamp
// in_successful_call = 0: that preserves lake fidelity but breaks the first
// invariant above (the gate counts contract_events rows, not
// in_successful_call=1 rows), and it would leave supply_flows crediting
// mint/burn amounts from transactions that never applied — a money-visible
// error for a fidelity gain in events the protocol does not actually emit
// (see the note in ExtractLedger's own doc on failed-tx meta shape).
func extractEvents(ext *LedgerExtract, tx ingest.LedgerTransaction, seq uint32, closeTime time.Time, txHash string, opArgs []*opInvokeArgs, successful bool) {
	if !successful {
		return
	}
	txEvents, terr := tx.GetTransactionEvents()
	if terr != nil {
		// G15-06: an unsupported future TransactionMeta version makes this
		// fail for every tx — count it so the lost events are visible
		// instead of looking like a clean empty ledger. Tx-level CAP-67
		// fee/diagnostic events (txEvents.TransactionEvents) are
		// deliberately not captured here, matching the dispatcher + census.
		ext.TxEventReadErrors++
		return
	}
	for opIdx, opEvents := range txEvents.OperationEvents {
		var call *opInvokeArgs
		if opIdx < len(opArgs) {
			call = opArgs[opIdx]
		}
		for evIdx := range opEvents {
			row, ok := eventRow(opEvents[evIdx], seq, closeTime, txHash, opIdx, evIdx, call)
			if !ok {
				continue
			}
			ext.Events = append(ext.Events, row)
			ext.Ledger.SorobanEventCount++
			// Decode-at-ingest (ADR-0034): for supply-affecting events
			// (mint/burn/clawback) decode the i128 amount now and emit a
			// supply_flows row, so per-token supply is a pure SQL sum with no
			// read-time XDR decode and no rollup refresh. An undecodable body
			// (skipped here) just doesn't contribute — the raw event is still
			// in contract_events for audit.
			if IsSupplyFlowSym(row.Topic0Sym) {
				if amt, _, okAmt := DecodeSupplyAmountXDR(row.DataXDR); okAmt {
					ext.SupplyFlows = append(ext.SupplyFlows, SupplyFlowRow{
						ContractID: row.ContractID,
						LedgerSeq:  seq,
						CloseTime:  closeTime,
						TxHash:     txHash,
						OpIndex:    row.OpIndex,
						EventIndex: row.EventIndex,
						Kind:       row.Topic0Sym,
						Amount:     amt,
					})
				}
			}
		}
	}
}

// eventRow maps one contract event to a ContractEventRow, applying the same
// capture-eligibility gate as dispatcher.captureEligible (Type=Contract,
// ContractId set, body V0, ≥1 topic). Returns ok=false to skip ineligible
// events so the row count matches the census oracle.
func eventRow(ce xdr.ContractEvent, seq uint32, closeTime time.Time, txHash string, opIdx, evIdx int, call *opInvokeArgs) (ContractEventRow, bool) {
	if ce.Type != xdr.ContractEventTypeContract || ce.ContractId == nil || ce.Body.V != 0 {
		return ContractEventRow{}, false
	}
	v0, ok := ce.Body.GetV0()
	if !ok || len(v0.Topics) == 0 {
		return ContractEventRow{}, false
	}
	cid, err := strkey.Encode(strkey.VersionByteContract, ce.ContractId[:])
	if err != nil {
		return ContractEventRow{}, false
	}
	topics := make([]string, 0, len(v0.Topics))
	for i := range v0.Topics {
		raw, merr := v0.Topics[i].MarshalBinary()
		if merr != nil {
			return ContractEventRow{}, false
		}
		topics = append(topics, base64.StdEncoding.EncodeToString(raw))
	}
	dataRaw, derr := v0.Data.MarshalBinary()
	if derr != nil {
		return ContractEventRow{}, false
	}
	var topic0Sym string
	if sym, sok := v0.Topics[0].GetSym(); sok {
		topic0Sym = string(sym)
	}
	// OpArgs provenance gate (see extractEvents' doc): the producing
	// op's top-level args are stored only on events the invoked contract
	// itself emitted.
	var opArgs []string
	if call != nil && call.ContractID == cid {
		opArgs = call.Args
	}
	return ContractEventRow{
		LedgerSeq:  seq,
		CloseTime:  closeTime,
		TxHash:     txHash,
		OpIndex:    uint32(opIdx),
		EventIndex: uint32(evIdx),
		ContractID: cid,
		EventType:  "contract",
		TopicCount: uint8(len(topics)),
		Topic0Sym:  topic0Sym,
		TopicsXDR:  topics,
		DataXDR:    base64.StdEncoding.EncodeToString(dataRaw),
		OpArgsXDR:  opArgs, // callee-only InvokeContract args (Redstone feed_ids, etc.)
		// Constant 1 is now a STATEMENT OF FACT, not an assumption: the only
		// caller, extractEvents, returns early for a failed transaction
		// (C2-010 sibling, audit-2026-07-23). Pre-fix this literal was
		// stamped on every row while extractEvents had no tx-success gate at
		// all, so the column asserted something the extractor had not checked.
		InSuccessfulCall: 1,
	}, true
}

// claimAtomCount mirrors dispatcher.claimAtomCount exactly (same op types +
// success gating) so classic_trade_effect_count equals the SDEX trade count.
// The per-atom predicate is [sdexclaim.IsRealTrade] on both sides — the same
// rule sdex.decodeClaimAtom applies — so the mirror this comment claims is
// enforced by a shared function, not by inspection (C2-010, audit-2026-07-23;
// TestClaimAtomCount_LockStepWithDecoder in the dispatcher's external test
// package pins all three against the same divergent-atom table).
func claimAtomCount(op xdr.Operation, result xdr.OperationResult) int { //nolint:gocognit // switch over 5 trade op types, with a dual result-arm fallback for passive offers; linear and clearer unsplit.
	if result.Code != xdr.OperationResultCodeOpInner {
		return 0
	}
	tr, ok := result.GetTr()
	if !ok {
		return 0
	}
	switch op.Body.Type {
	case xdr.OperationTypeManageSellOffer:
		r, ok := tr.GetManageSellOfferResult()
		if !ok || r.Code != xdr.ManageSellOfferResultCodeManageSellOfferSuccess {
			return 0
		}
		return sdexclaim.RealTradeCount(r.MustSuccess().OffersClaimed)
	case xdr.OperationTypeManageBuyOffer:
		r, ok := tr.GetManageBuyOfferResult()
		if !ok || r.Code != xdr.ManageBuyOfferResultCodeManageBuyOfferSuccess {
			return 0
		}
		return sdexclaim.RealTradeCount(r.MustSuccess().OffersClaimed)
	case xdr.OperationTypeCreatePassiveSellOffer:
		// stellar-core emits passive-offer results under the ManageSellOffer
		// arm (passive offers are processed as manage-sell-offers), so
		// GetCreatePassiveSellOfferResult returns ok=false on real on-chain
		// data — confirmed vs Hubble at ledger 62701151. Try the passive arm
		// (XDR spec) first, then fall back to manage-sell (what core emits).
		// Mirror sdex.extractClaimAtoms + dispatcher.census exactly.
		if r, ok := tr.GetCreatePassiveSellOfferResult(); ok {
			if r.Code != xdr.ManageSellOfferResultCodeManageSellOfferSuccess {
				return 0
			}
			return sdexclaim.RealTradeCount(r.MustSuccess().OffersClaimed)
		}
		if r, ok := tr.GetManageSellOfferResult(); ok {
			if r.Code != xdr.ManageSellOfferResultCodeManageSellOfferSuccess {
				return 0
			}
			return sdexclaim.RealTradeCount(r.MustSuccess().OffersClaimed)
		}
		return 0
	case xdr.OperationTypePathPaymentStrictReceive:
		r, ok := tr.GetPathPaymentStrictReceiveResult()
		if !ok || r.Code != xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveSuccess {
			return 0
		}
		return sdexclaim.RealTradeCount(r.MustSuccess().Offers)
	case xdr.OperationTypePathPaymentStrictSend:
		r, ok := tr.GetPathPaymentStrictSendResult()
		if !ok || r.Code != xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess {
			return 0
		}
		return sdexclaim.RealTradeCount(r.MustSuccess().Offers)
	}
	return 0
}

// (realTradeCount / claimAtomAmounts moved to internal/sdexclaim — shared with
// the dispatcher census. Both-zero no-op crosses are excluded, one-side-zero
// rounding-artifact fills kept, so the census equals COUNT(trades).)

func hashHex(h xdr.Hash) string { return hex.EncodeToString(h[:]) }

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
