package completeness

import (
	"context"
	"fmt"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the per-source decode surface the reconciler re-runs over the lake's contract_events.
// Matches + Decode are the SAME functions the projector uses, so the re-derive recomputes what the
// projector should have written instead of a parallel implementation that could disagree for the
// wrong reason.
type Decoder interface {
	Matches(events.Event) bool
	Decode(events.Event) ([]consumer.Event, error)
}

// eventLedgerCarrier is an optional interface for a decoder whose correlation buffer can emit an
// output while processing a LATER stream event than the one it belongs to (the phoenix 7-field-era
// sweep rescue). The re-derive then counts the output at its own ledger, where the served row
// lives; otherwise every sweep-rescued trade shows as a +/- per-ledger shift pair in a strict
// reconcile (net-zero noise that forces phoenix onto aggregate netting). 1:1 decoders need not
// implement it.
type eventLedgerCarrier interface{ EventLedger() uint32 }

// countLedger picks the per-ledger bucket for one decoded output: the
// output's own declared ledger when it carries one (non-zero), else
// the stream event/row it was decoded from.
func countLedger(out consumer.Event, streamLedger uint32) uint32 {
	if c, ok := out.(eventLedgerCarrier); ok {
		if l := c.EventLedger(); l != 0 {
			return l
		}
	}
	return streamLedger
}

// ProjectionGap is a ledger where the rows the decoder would emit from raw events (Expected)
// disagree with the rows present in the protocol table (Actual). Expected > Actual is a
// projection/persistence drop; Actual > Expected is a phantom row or duplicate. Either is a real
// discrepancy localized to one ledger (ADR-0033 Claim 2b).
//
// SCOPE: ROW-COUNT only. A clean ledger proves the right NUMBER of rows exist, not that their
// derived columns (amounts, prices, decoded fields) are correct; a decoder bug writing the right
// count with wrong values passes ReconcileCounts silently. A clean result must not be described as
// "complete" or "verified correct" for VALUE correctness.
type ProjectionGap struct {
	Ledger   uint32
	Expected int
	Actual   int
}

// BlindSpots records the raw rows a re-derive SKIPPED because it could not turn them into a
// comparable expectation.
//
// The skip mirrors the projector's: a row the projector could not decode produced no served row,
// and one the re-derive cannot decode produces no expected row, so ReconcileCounts sees
// expected == actual == 0 and reports the ledger CLEAN. Both sides fail for the same reason, so a
// decoder bug or truncated payload nets to zero and certifies `projection_ok` where rows were
// dropped.
//
// Row counts cannot detect this class; the blindness itself is the only honest signal, hence it is
// returned rather than logged. A caller certifying completeness must treat "could not evaluate
// this ledger" as NOT clean. The recognition audit misses it: an event the decoder claims
// (Matches == true) and then fails to Decode is a recognised event.
type BlindSpots struct {
	// Ledgers holds every ledger with at least one blind row, ascending and
	// deduped. Callers whose branch pins the watermark on problem ledgers
	// append these; callers whose branch flips a boolean use [BlindSpots.Any].
	Ledgers []uint32

	// UndecodableMatched counts rows the decoder CLAIMED (Matches == true) and then failed to
	// Decode: this source owned the event but could not produce its expectation.
	UndecodableMatched int

	// Unreconstructable counts stored soroban_events rows too broken to rebuild into an
	// events.Event (empty contract_id, missing topic_0_xdr, wrong-length tx_hash, undecodable
	// op_args). Matches is never evaluated, so no decoder is to blame, but the projector dropped
	// them for the same reason and the served tier is missing whatever they held; they pin too.
	// Counted separately so a verdict says which class fired. Always 0 on the ClickHouse path,
	// which streams events.Event with no Row round-trip.
	Unreconstructable int
}

// Merge returns the union of b and o: counts summed, ledgers deduped and
// sorted ascending.
func (b BlindSpots) Merge(o BlindSpots) BlindSpots {
	t := NewBlindTracker()
	for _, l := range b.Ledgers {
		t.mark(l)
	}
	for _, l := range o.Ledgers {
		t.mark(l)
	}
	out := t.Result()
	out.UndecodableMatched = b.UndecodableMatched + o.UndecodableMatched
	out.Unreconstructable = b.Unreconstructable + o.Unreconstructable
	return out
}

// Rows is the total blind-row count across both classes.
func (b BlindSpots) Rows() int { return b.UndecodableMatched + b.Unreconstructable }

// Any reports whether the re-derive was blind anywhere in the range — i.e.
// whether its expected counts are known-incomplete and a clean
// ReconcileCounts therefore proves nothing for the affected ledgers.
func (b BlindSpots) Any() bool { return b.Rows() > 0 }

// Detail renders the operator-facing summary for a verdict line. Empty
// string when there is nothing to report, so callers can append
// unconditionally.
func (b BlindSpots) Detail() string {
	if !b.Any() {
		return ""
	}
	return fmt.Sprintf(
		"re-derive blind on %d ledger(s): %d undecodable-but-matched, %d unreconstructable (first=%d) — expected counts are known-incomplete there, so a zero delta proves nothing",
		len(b.Ledgers), b.UndecodableMatched, b.Unreconstructable, b.Ledgers[0])
}

// BlindTracker accumulates [BlindSpots] during a stream, keeping the ledger set deduped.
//
// Exported because the re-derives here are not the only per-row soft-fail sites: the ContractCall
// census in internal/ops/chops skips malformed calls the same way, with the same nets-to-zero
// consequence. One tracker means one definition of "blind", so no site can invent a weaker one.
type BlindTracker struct {
	seen  map[uint32]bool
	spots BlindSpots
}

// NewBlindTracker returns a ready tracker.
func NewBlindTracker() *BlindTracker {
	return &BlindTracker{seen: make(map[uint32]bool)}
}

// Undecodable records a row the decoder CLAIMED and then failed to decode.
func (t *BlindTracker) Undecodable(ledger uint32) {
	t.spots.UndecodableMatched++
	t.mark(ledger)
}

// Unreconstructable records a stored row too broken to rebuild at all.
func (t *BlindTracker) Unreconstructable(ledger uint32) {
	t.spots.Unreconstructable++
	t.mark(ledger)
}

func (t *BlindTracker) mark(ledger uint32) {
	if t.seen[ledger] {
		return
	}
	t.seen[ledger] = true
	t.spots.Ledgers = append(t.spots.Ledgers, ledger)
}

// Result returns the accumulated blind spots with Ledgers sorted ascending,
// so callers can read Ledgers[0] as the first affected ledger.
func (t *BlindTracker) Result() BlindSpots {
	sort.Slice(t.spots.Ledgers, func(i, j int) bool { return t.spots.Ledgers[i] < t.spots.Ledgers[j] })
	return t.spots
}

// safeDecode runs dec.Decode under a per-event recover, turning a decoder PANIC into a returned
// error so the caller's decode-error path (blind.Undecodable + skip) handles it. This mirrors the
// projector's processEventSafely, where a recovered panic and a decode error have the same outcome:
// the row is dropped, the failure counted, the cursor advances.
//
// The projector recovers panics on historical / upgraded-WASM event shapes (backfill sees every
// prior version), silently dropping rows from the served tier, and the re-derive exists to make
// those drops VISIBLE. An escaped panic would crash compute-completeness before it wrote any
// snapshot, freezing EVERY source's verdict instead of flagging a blind spot. Sharing the
// decode-error accounting keeps `delta==0 && !blind.Any()` false.
func safeDecode(dec Decoder, ev events.Event) (outs []consumer.Event, err error) {
	if perr := Guard(func() { outs, err = dec.Decode(ev) }); perr != nil {
		return nil, fmt.Errorf("decoder panicked on event shape: %w", perr)
	}
	return outs, err
}

// Guard runs fn under a recover and returns a recovered panic as an error. It is the one panic
// guard every completeness oracle runs decoder code under (these re-derives and the chops census /
// preseed / prefilter walks), so a decoder the production dispatcher recovers per input
// (dispatcher.recordDecoderPanic) becomes a blind spot on the audit path, not a crash that freezes
// every remaining source's verdict (see [safeDecode]).
func Guard(fn func()) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("recovered: %v", rec)
		}
	}()
	fn()
	return nil
}

// safeMatches runs dec.Matches under the same per-event recover as [safeDecode]. Matches
// type-asserts topic vectors and reads body fields, so a malformed row panics there as readily as
// in Decode. A panic is treated like a decode failure: the row is skipped AND recorded as a blind
// spot, so a ledger the re-derive could not evaluate is never certified clean.
func safeMatches(dec Decoder, ev events.Event) (matched bool, err error) {
	if perr := Guard(func() { matched = dec.Matches(ev) }); perr != nil {
		return false, fmt.Errorf("decoder panicked matching event shape: %w", perr)
	}
	return matched, nil
}

// EventStreamer yields decoded-ready [events.Event] from the ClickHouse lake's
// contract_events. A clickhouse adapter over StreamContractEventsFiltered
// satisfies it structurally.
type EventStreamer interface {
	StreamContractEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms []string, fn func(events.Event) error) error
}

// ReDeriveOutputCountsByKindFromEvents re-runs the decoder over the lake's contract_events in
// [from, to] and returns output counts keyed by EventKind(), then ledger. One decoder routes kinds
// to different tables (soroswap "soroswap.trade" to trades, "soroswap.skim" to
// soroswap_skim_events; blend emits five kinds across four tables), so per-table reconciliation
// must count ONLY that table's kinds; [SumKinds] projects them.
//
// contractIDs / topic0Syms are the projector's prefilters for this source (empty = match by topic
// across all contracts). An event failing Decode is skipped and returned as a [BlindSpots] entry,
// so a clean reconcile over a range the decoder could not read is not certified complete.
// BlindSpots.Unreconstructable is always 0 here.
func ReDeriveOutputCountsByKindFromEvents(
	ctx context.Context,
	es EventStreamer,
	dec Decoder,
	contractIDs, topic0Syms []string,
	from, to uint32,
) (map[string]map[uint32]int, BlindSpots, error) {
	byKind := make(map[string]map[uint32]int)
	blind := NewBlindTracker()
	// Adjacent-duplicate skip: the stream is ORDER BY (ledger, tx_hash, op_index, event_index), so
	// un-merged ReplacingMergeTree duplicates arrive consecutively. Comparing identity with the
	// previous event is O(1) memory and keeps the read no-FINAL (gentle) while counting correctly.
	var (
		haveLast bool
		lL       uint32
		lTx      string
		lOp, lEv int
	)
	err := es.StreamContractEvents(ctx, from, to, contractIDs, topic0Syms,
		func(ev events.Event) error {
			if haveLast && ev.Ledger == lL && ev.OperationIndex == lOp && ev.EventIndex == lEv && ev.TxHash == lTx {
				return nil // exact-identity duplicate part; count once
			}
			haveLast, lL, lTx, lOp, lEv = true, ev.Ledger, ev.TxHash, ev.OperationIndex, ev.EventIndex
			matched, merr := safeMatches(dec, ev)
			if merr != nil {
				blind.Undecodable(ev.Ledger)
				return nil //nolint:nilerr // panicking Matches: skip the event, pin the verdict.
			}
			if !matched {
				return nil
			}
			outs, derr := safeDecode(dec, ev)
			if derr != nil {
				blind.Undecodable(ev.Ledger)
				return nil //nolint:nilerr // deterministically-broken event; skip the count, pin the verdict.
			}
			for _, out := range outs {
				k := out.EventKind()
				if byKind[k] == nil {
					byKind[k] = make(map[uint32]int)
				}
				byKind[k][countLedger(out, ev.Ledger)]++
			}
			return nil
		})
	if err != nil {
		return nil, BlindSpots{}, err
	}
	return byKind, blind.Result(), nil
}

// SumKinds projects a by-kind count map down to a single per-ledger
// count over the given EventKinds — the expected row count for the
// table those kinds route to.
func SumKinds(byKind map[string]map[uint32]int, kinds ...string) map[uint32]int {
	out := make(map[uint32]int)
	for _, k := range kinds {
		for ledger, c := range byKind[k] {
			out[ledger] += c
		}
	}
	return out
}

// ReconcileCounts diffs expected (decoder re-derive) against actual (protocol-table rows) per
// ledger and returns every disagreeing ledger, ascending. Empty means Claim 2b's ROW-COUNT
// invariant holds for the range.
//
// COUNT-ONLY (see ProjectionGap): an empty result does not certify the rows' derived VALUES, so
// never label a range "complete" for value correctness on this alone.
func ReconcileCounts(expected, actual map[uint32]int) []ProjectionGap {
	var gaps []ProjectionGap
	seen := make(map[uint32]bool, len(expected))
	for ledger, exp := range expected {
		seen[ledger] = true
		if act := actual[ledger]; act != exp {
			gaps = append(gaps, ProjectionGap{Ledger: ledger, Expected: exp, Actual: act})
		}
	}
	for ledger, act := range actual {
		if seen[ledger] {
			continue
		}
		// Expected this ledger to produce nothing, but the table has
		// rows — a phantom (row with no backing captured event).
		if act != 0 {
			gaps = append(gaps, ProjectionGap{Ledger: ledger, Expected: 0, Actual: act})
		}
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Ledger < gaps[j].Ledger })
	return gaps
}
