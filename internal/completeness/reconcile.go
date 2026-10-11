package completeness

import (
	"context"
	"fmt"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the per-source decode surface re-run over the lake's contract_events. Matches and
// Decode are the SAME functions the projector uses, so the re-derive cannot disagree with it
// through a parallel implementation.
type Decoder interface {
	Matches(events.Event) bool
	Decode(events.Event) ([]consumer.Event, error)
}

// eventLedgerCarrier is optional for a decoder whose correlation buffer can emit an output while
// processing a LATER stream event (the phoenix sweep rescue). The re-derive then counts it at its
// own ledger, where the served row lives; otherwise each rescued trade shows as a +/- per-ledger
// shift pair in a strict reconcile. 1:1 decoders need not implement it.
type eventLedgerCarrier interface{ EventLedger() uint32 }

// countLedger buckets an output at its own declared ledger when non-zero, else at the stream
// event it was decoded from.
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
// A row the projector could not decode produced no served row, and one the re-derive cannot
// decode produces no expected row, so ReconcileCounts sees expected == actual == 0 and reports
// the ledger CLEAN. Row counts cannot detect this class; the blindness is the only honest signal,
// hence it is returned rather than logged. A caller certifying completeness must treat "could not
// evaluate this ledger" as NOT clean. The recognition audit misses it too: a claimed event that
// fails to Decode is still a recognised event.
type BlindSpots struct {
	// Ledgers holds every ledger with at least one blind row, ascending and deduped. Callers
	// that pin the watermark append these; others use [BlindSpots.Any].
	Ledgers []uint32

	// UndecodableMatched counts rows the decoder CLAIMED (Matches == true) and then failed to
	// Decode.
	UndecodableMatched int

	// Unreconstructable counts stored soroban_events rows too broken to rebuild into an
	// events.Event. Matches is never evaluated, so no decoder is to blame, but the projector
	// dropped them too and the served tier is missing what they held, so they pin as well.
	// Counted separately so a verdict says which class fired. Always 0 on the ClickHouse path.
	Unreconstructable int
}

// Merge returns the union of b and o: counts summed, ledgers deduped and ascending.
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

// Any reports whether the re-derive was blind anywhere, so a clean ReconcileCounts proves
// nothing for the affected ledgers.
func (b BlindSpots) Any() bool { return b.Rows() > 0 }

// Detail renders the verdict-line summary; empty when there is nothing to report.
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
// Exported because the ContractCall census in internal/ops/chops soft-fails per row the same way.
// One tracker means one definition of "blind", so no site can invent a weaker one.
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

// Result returns the blind spots with Ledgers ascending, so Ledgers[0] is the first affected.
func (t *BlindTracker) Result() BlindSpots {
	sort.Slice(t.spots.Ledgers, func(i, j int) bool { return t.spots.Ledgers[i] < t.spots.Ledgers[j] })
	return t.spots
}

// safeDecode runs dec.Decode under a per-event recover, turning a decoder PANIC into an error for
// the decode-error path (blind.Undecodable + skip), mirroring the projector's processEventSafely.
//
// The projector recovers panics on historical / upgraded-WASM event shapes and silently drops
// those rows; the re-derive exists to make the drops VISIBLE. An escaped panic would crash
// compute-completeness before any snapshot, freezing EVERY source's verdict instead of flagging
// a blind spot.
func safeDecode(dec Decoder, ev events.Event) (outs []consumer.Event, err error) {
	if perr := Guard(func() { outs, err = dec.Decode(ev) }); perr != nil {
		return nil, fmt.Errorf("decoder panicked on event shape: %w", perr)
	}
	return outs, err
}

// Guard runs fn under a recover and returns a recovered panic as an error. Every completeness
// oracle runs decoder code under it, so a panic the dispatcher recovers per input becomes a blind
// spot on the audit path, not a crash that freezes every remaining verdict (see [safeDecode]).
func Guard(fn func()) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("recovered: %v", rec)
		}
	}()
	fn()
	return nil
}

// safeMatches runs dec.Matches under the same recover as [safeDecode]; Matches type-asserts topics
// and bodies, so a malformed row can panic there too. The row is skipped AND recorded as a blind
// spot, so the ledger is never certified clean.
func safeMatches(dec Decoder, ev events.Event) (matched bool, err error) {
	if perr := Guard(func() { matched = dec.Matches(ev) }); perr != nil {
		return false, fmt.Errorf("decoder panicked matching event shape: %w", perr)
	}
	return matched, nil
}

// EventStreamer yields [events.Event] from the ClickHouse lake's contract_events.
type EventStreamer interface {
	StreamContractEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms []string, fn func(events.Event) error) error
}

// ReDeriveOutputCountsByKindFromEvents re-runs the decoder over the lake's contract_events in
// [from, to] and returns output counts keyed by EventKind(), then ledger. One decoder routes kinds
// to different tables (blend emits five kinds across four tables), so per-table reconciliation
// must count ONLY that table's kinds; [SumKinds] projects them.
//
// contractIDs / topic0Syms are the projector's prefilters (empty = match by topic across all
// contracts). An event failing Decode is skipped and returned as a [BlindSpots] entry, so a clean
// reconcile over unreadable events is not certified complete. Unreconstructable is always 0 here.
func ReDeriveOutputCountsByKindFromEvents(
	ctx context.Context,
	es EventStreamer,
	dec Decoder,
	contractIDs, topic0Syms []string,
	from, to uint32,
) (map[string]map[uint32]int, BlindSpots, error) {
	byKind := make(map[string]map[uint32]int)
	blind := NewBlindTracker()
	// Adjacent-duplicate skip: the stream is ordered by event identity, so un-merged
	// ReplacingMergeTree duplicates arrive consecutively. Comparing with the previous event is
	// O(1) memory and keeps the read no-FINAL while counting correctly.
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

// SumKinds sums a by-kind count map over the given EventKinds: the expected per-ledger row count
// for the table those kinds route to.
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
		// Expected nothing but the table has rows: a phantom with no backing event.
		if act != 0 {
			gaps = append(gaps, ProjectionGap{Ledger: ledger, Expected: 0, Actual: act})
		}
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Ledger < gaps[j].Ledger })
	return gaps
}
