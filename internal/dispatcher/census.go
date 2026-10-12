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

// Census is the decoder-independent per-ledger count of completeness primitives plus hash-chain anchors (ADR-0033 Claim 1); SorobanEventCount MUST equal COUNT(soroban_events) for the ledger, and ClassicTradeEffectCount counts ClaimAtoms (decoder-side), not served trades rows.
type Census struct {
	LedgerSeq               uint32
	LedgerCloseTime         time.Time
	LedgerHash              xdr.Hash
	PrevLedgerHash          xdr.Hash
	SorobanEventCount       int
	ClassicTradeEffectCount int

	// TxReadErrors counts transactions the reader could not decode. Non-zero means the counts may
	// undercount, so the caller must decline to write an authoritative substrate row for the ledger.
	TxReadErrors int

	// TxEventReadErrors counts transactions whose GetTransactionEvents() failed (e.g. an
	// unsupported future TransactionMeta version). Non-zero means SorobanEventCount undercounts, so
	// the caller must not write an authoritative "complete" row: the projection reconcile would
	// otherwise pass against a count that dropped to zero in lock-step with the sink.
	TxEventReadErrors int
}

// CensusLedger tallies completeness primitives from a LedgerCloseMeta without decoding event bodies, independent of the decoder path so a decoder bug cannot mask itself; it mirrors the dispatch walk's eligibility rules.
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
		// Per-operation events only; tx-level CAP-67 fee/diagnostic events are out of scope as in
		// the dispatcher, so the count matches what the sink writes. A GetTransactionEvents error
		// is recorded so the caller declines to write an authoritative row instead of silently
		// undercounting to zero.
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

// captureEligible reports whether the raw-event sink would land a contract event in soroban_events. It runs the live conversion
// ([contractEventToEventsEvent]), not a re-stated gate, so an event the sink drops (ScVal marshal or strkey failure) is not counted as a
// phantom projector shortfall.
func captureEligible(ce xdr.ContractEvent) bool {
	ev := contractEventToEventsEvent(ce, 0, "", 0, 0, "", nil)
	if ev == nil {
		return false
	}
	// The one per-event gate living in sorobanevents.Capture rather than the conversion: a
	// zero-topic event is skipped because topic_0_xdr is NOT NULL.
	return len(ev.Topic) > 0
}

// claimAtomCount counts the ClaimAtoms an operation produced that become trade rows. Selection mirrors sdex.extractClaimAtoms and the
// real-trade test is [sdexclaim.IsRealTrade], so the census equals the decoder's output (not the trade-row count; see [Census]).
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

// The count mirrors sdex.decodeClaimAtom's both-zero drop exactly (shared via internal/sdexclaim):
// both-zero no-op crosses are excluded, one-side-zero rounding-artifact fills are kept, so the
// census exceeds COUNT(trades) by the fills the writer cannot store.

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
