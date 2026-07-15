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
// It is computed directly from the LedgerCloseMeta WITHOUT decoding
// any event body — the LCM's own ground truth (ADR-0033 Claim 1).
//
// The two counts are the checksums the completeness model reconciles
// against:
//
//   - SorobanEventCount MUST equal COUNT(soroban_events WHERE
//     ledger=seq) — any shortfall is a capture/persistence gap.
//   - ClassicTradeEffectCount MUST equal COUNT(trades WHERE
//     source='sdex' AND ledger=seq) — it counts ClaimAtoms exactly
//     the way internal/sources/sdex produces one trade per atom.
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
		// writes. G15-06: a GetTransactionEvents error (e.g. an
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
// raw-event sink would land in soroban_events. Mirrors the
// eligibility gate in contractEventToEventsEvent + sorobanevents.Capture
// (Type=Contract, ContractId set, body version 0, at least one topic)
// WITHOUT decoding the body — so the census count equals the
// soroban_events row count for the ledger.
func captureEligible(ce xdr.ContractEvent) bool {
	if ce.Type != xdr.ContractEventTypeContract {
		return false
	}
	if ce.ContractId == nil {
		return false
	}
	if ce.Body.V != 0 {
		// V != 0 is an unaudited protocol bump; contractEventToEventsEvent
		// drops it, so it never lands in soroban_events either.
		return false
	}
	v0, ok := ce.Body.GetV0()
	if !ok {
		return false
	}
	// sorobanevents.Capture skips zero-topic events (NOT NULL
	// topic_0_xdr); every real contract event has ≥1 topic anyway.
	return len(v0.Topics) > 0
}

// claimAtomCount returns the number of ClaimAtoms an operation
// produced — one classic-DEX trade each. It mirrors
// internal/sources/sdex.extractClaimAtoms exactly (same op types,
// same success gating) so the census equals the SDEX trade-row count.
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
// rounding-artifact fills are kept, so the census equals COUNT(trades).)

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
