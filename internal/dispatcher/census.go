package dispatcher

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sdexclaim"
)

// Census is the decoder-independent count of a single ledger's
// completeness-relevant primitives, plus its hash-chain anchors.
// It is computed from the LedgerCloseMeta WITHOUT decoding any event
// body — the LCM's own ground truth (ADR-0033 Claim 1).
//
//   - SorobanEventCount MUST equal COUNT(soroban_events WHERE
//     ledger=seq) — any shortfall is a capture/persistence gap.
//
//   - ClassicTradeEffectCount counts ClaimAtoms exactly the way
//     internal/sources/sdex produces one trade per atom. It does NOT
//     equal COUNT(trades WHERE source='sdex' AND ledger=seq): the decoder
//     deliberately emits one-side-zero fills (one leg rounded to 0
//     stroops), which the writer stores (unpriceable) but older ledgers
//     lack, so the SDEX reconcile in internal/ops/chops compares
//     priceable fills only (sdexServedCensus, reconTarget.countFilter).
//     No projection oracle reads this counter.
//
//     The lockstep test that guards this comment compares the counter to
//     the DECODER, never to the writer.
//
// LedgerHash / PrevLedgerHash are the header hashes for the
// contiguity hash-chain check (prev_ledger_hash[N] == ledger_hash[N-1]).
type Census struct {
	LedgerSeq               uint32
	LedgerCloseTime         time.Time
	LedgerHash              xdr.Hash
	PrevLedgerHash          xdr.Hash
	SorobanEventCount       int
	ClassicTradeEffectCount int

	// TxReadErrors counts transactions the reader could not decode.
	// A non-zero value means the census saw a malformed tx and the
	// counts may undercount that tx's primitives — surfaced so the
	// caller can decline to write an authoritative substrate row for
	// a ledger we couldn't fully read.
	TxReadErrors int

	// TxEventReadErrors counts transactions whose GetTransactionEvents()
	// failed (e.g. an unsupported future TransactionMeta version). Like
	// TxReadErrors, a non-zero value means the census could NOT see this
	// tx's Soroban events, so SorobanEventCount undercounts — the caller
	// must decline to write an authoritative "complete" substrate row
	// rather than let the projection reconcile pass against a count that
	// silently dropped to zero in lock-step with the sink (G15-06).
	TxEventReadErrors int
}

// CensusLedger walks a LedgerCloseMeta and tallies the
// completeness-relevant primitives without decoding event bodies.
// It is deliberately INDEPENDENT of the decoder path (it does not
// call any Decoder) so it can serve as an oracle for what the
// decoders should have produced — a bug in a decoder cannot mask
// itself in the census.
//
// Counting mirrors the dispatch walk's eligibility rules exactly:
// only successful transactions contribute (ProcessLedger skips
// failed txs), contract events must be capture-eligible
// (see captureEligible), and trade ops must have succeeded.
func CensusLedger(lcm xdr.LedgerCloseMeta, passphrase string) (Census, error) { //nolint:gocognit,gocyclo // linear LCM walk; splitting reduces clarity (same as ProcessLedger).
	c := Census{
		LedgerSeq:       lcm.LedgerSequence(),
		LedgerCloseTime: lcm.ClosedAt().UTC(),
		LedgerHash:      lcm.LedgerHash(),
	}
	if h, ok := censusPrevLedgerHash(lcm); ok {
		c.PrevLedgerHash = h
	} else {
		return Census{}, fmt.Errorf("dispatcher: CensusLedger: cannot extract LedgerHeader for ledger %d", c.LedgerSeq)
	}

	reader, err := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)
	if err != nil {
		return Census{}, fmt.Errorf("dispatcher: CensusLedger: build reader for ledger %d: %w", c.LedgerSeq, err)
	}
	defer func() { _ = reader.Close() }()

	for {
		tx, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			c.TxReadErrors++
			continue
		}
		if !tx.Result.Successful() {
			continue
		}

		// ─── Soroban contract events ─────────────────────────────
		// Per-operation events only — tx-level CAP-67 fee/diagnostic
		// events are out of scope here exactly as in the dispatcher
		// (dispatcher.go), so the census count matches what the sink
		// writes. A GetTransactionEvents error (e.g. an
		// unsupported future meta version) means we cannot count this
		// tx's Soroban primitives — record it so the caller declines to
		// write an authoritative substrate row for a ledger it couldn't
		// fully read, instead of silently undercounting to zero.
		if txEvents, terr := tx.GetTransactionEvents(); terr == nil {
			for _, opEvents := range txEvents.OperationEvents {
				for i := range opEvents {
					if captureEligible(opEvents[i]) {
						c.SorobanEventCount++
					}
				}
			}
		} else {
			c.TxEventReadErrors++
		}

		// ─── Classic trade effects (ClaimAtoms) ──────────────────
		ops := tx.Envelope.Operations()
		if opResults, ok := tx.Result.Result.OperationResults(); ok {
			for i := range ops {
				if i >= len(opResults) {
					break
				}
				c.ClassicTradeEffectCount += claimAtomCount(ops[i], opResults[i])
			}
		}
	}

	return c, nil
}

// captureEligible reports whether a contract event is one that the
// raw-event sink would land in soroban_events.
//
// It runs the ACTUAL conversion the live path runs
// ([contractEventToEventsEvent]) and asks whether it produced an event,
// rather than re-stating its gate. That is the difference between
// agreeing by construction and two functions agreeing to stay in step —
// the same reason claimAtomCount delegates to sdexclaim.IsRealTrade.
//
// Re-stating only the cheap half of the gate (Type=Contract, ContractId
// set, body version 0, ≥1 topic) would skip the ScVal MarshalBinary
// round-trip and the contract-id strkey encode: an event that fails
// either is dropped by the sink but would be counted by the census, and
// the reconcile would show a phantom projector shortfall.
//
// Still NOT modelled (and deliberately so — they are per-TRANSACTION, not
// per-event, so they cannot make one event of a tx diverge from another):
// sorobanevents.Capture's tx-hash-hex and ledger-close-time parses.
func captureEligible(ce xdr.ContractEvent) bool {
	ev := contractEventToEventsEvent(ce, 0, "", 0, 0, "", nil)
	if ev == nil {
		return false
	}
	// The one per-event gate that lives in sorobanevents.Capture rather
	// than in the conversion: a zero-topic event is skipped because
	// topic_0_xdr is NOT NULL. Every real contract event has ≥1 topic.
	return len(ev.Topic) > 0
}

// claimAtomCount returns the number of ClaimAtoms an operation
// produced that will become trade rows. It mirrors
// internal/sources/sdex.extractClaimAtoms exactly (same op types,
// same success gating) for atom SELECTION, and delegates the per-atom
// "is this a real trade" test to [sdexclaim.IsRealTrade], which is the
// same predicate sdex.decodeClaimAtom enforces — so the census equals
// the decoder's trade output by construction, not by three files
// agreeing to stay in step. It does not
// equal the trade-row count; see [Census].
// Returns the count rather than the slice to avoid allocation in the
// hot per-ledger census walk.
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
		// arm, so GetCreatePassiveSellOfferResult returns ok=false on real
		// data. Try the passive arm, fall back to manage-sell. Must mirror
		// sdex.extractClaimAtoms exactly so the census equals the SDEX count.
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
// the ClickHouse structural extractor. The count mirrors sdex.decodeClaimAtom's
// both-zero drop EXACTLY: both-zero no-op crosses are excluded, one-side-zero
// rounding-artifact fills are kept, so the census equals the decoder's output
// and exceeds COUNT(trades) by the fills the writer cannot store.)

// censusPrevLedgerHash extracts header.PreviousLedgerHash across the
// LedgerCloseMeta versions (mirrors the cmd-side extractLedgerHeader).
func censusPrevLedgerHash(lcm xdr.LedgerCloseMeta) (xdr.Hash, bool) {
	switch lcm.V {
	case 0:
		if lcm.V0 == nil {
			return xdr.Hash{}, false
		}
		return lcm.V0.LedgerHeader.Header.PreviousLedgerHash, true
	case 1:
		if lcm.V1 == nil {
			return xdr.Hash{}, false
		}
		return lcm.V1.LedgerHeader.Header.PreviousLedgerHash, true
	case 2:
		if lcm.V2 == nil {
			return xdr.Hash{}, false
		}
		return lcm.V2.LedgerHeader.Header.PreviousLedgerHash, true
	}
	return xdr.Hash{}, false
}
