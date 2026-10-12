package completeness

import (
	"context"
	"fmt"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the per-source decode surface re-run over the lake's contract_events; Matches and Decode are the SAME functions the projector uses, so the re-derive cannot disagree through a parallel implementation.
type Decoder interface {
	Matches(events.Event) bool
	Decode(events.Event) ([]consumer.Event, error)
}

// eventLedgerCarrier is optional for a decoder whose correlation buffer can emit an output while processing a LATER stream event
// (the phoenix sweep rescue). The re-derive then counts it at its own ledger, where the served row lives; otherwise a strict
// reconcile shows each rescued trade as a +/- per-ledger shift pair. 1:1 decoders need not implement it.
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

// ProjectionGap is a ledger where decoder-expected rows disagree with the protocol table (ADR-0033 Claim 2b). SCOPE: ROW-COUNT only; a clean ledger does not prove derived values are correct, so never call it "complete" or "verified correct" for VALUE correctness.
type ProjectionGap struct {
	Ledger   uint32
	Expected int
	Actual   int
}

// BlindSpots records the raw rows a re-derive SKIPPED because it could not make a comparable expectation; such a ledger reconciles expected == actual == 0 and looks CLEAN, so a caller certifying completeness must treat it as NOT clean.
type BlindSpots struct {
	// Ledgers holds every ledger with at least one blind row, ascending and deduped. Callers
	// that pin the watermark append these; others use [BlindSpots.Any].
	Ledgers []uint32

	// UndecodableMatched counts rows the decoder CLAIMED (Matches == true) and then failed to
	// Decode.
	UndecodableMatched int

	// Unreconstructable counts stored soroban_events rows too broken to rebuild into an
	// events.Event. No decoder is to blame, but the projector dropped them too, so they pin as
	// well; counted separately so a verdict says which class fired. Always 0 on the ClickHouse path.
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

// Any reports whether the re-derive was blind anywhere, so a clean ReconcileCounts proves nothing for the affected ledgers.
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

// BlindTracker accumulates BlindSpots during a stream, keeping the ledger set deduped; exported so internal/ops/chops shares one definition of "blind".
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

// safeDecode runs dec.Decode under a per-event recover, turning a decoder PANIC into an error for the decode-error path
// (blind.Undecodable + skip). The projector silently drops panicking historical rows; the re-derive makes the drops VISIBLE, and an
// escaped panic would crash compute-completeness and freeze EVERY source's verdict.
func safeDecode(dec Decoder, ev events.Event) (outs []consumer.Event, err error) {
	if perr := Guard(func() { outs, err = dec.Decode(ev) }); perr != nil {
		return nil, fmt.Errorf("decoder panicked on event shape: %w", perr)
	}
	return outs, err
}

// Guard runs fn under a recover and returns a recovered panic as an error, so a decoder panic becomes a blind spot on the audit path instead of a crash that freezes every remaining verdict.
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

// ReDeriveOutputCountsByKindFromEvents re-runs the decoder over the lake's contract_events in [from, to] and returns output counts by EventKind(), then ledger; per-table reconciliation must count ONLY that table's kinds (see SumKinds), and an event failing Decode becomes a BlindSpots entry, never a certified-clean result.
func ReDeriveOutputCountsByKindFromEvents(
	ctx context.Context,
	es EventStreamer,
	dec Decoder,
	contractIDs, topic0Syms []string,
	from, to uint32,
) (map[string]map[uint32]int, BlindSpots, error) {
	byKind := make(map[string]map[uint32]int)
	blind := NewBlindTracker()
	// The stream is ordered by event identity, so un-merged ReplacingMergeTree duplicates arrive
	// consecutively; comparing with the previous event dedupes in O(1) memory without FINAL.
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

// SumKinds sums a by-kind count map over the given EventKinds: the expected per-ledger row count for the table those kinds route to.
func SumKinds(byKind map[string]map[uint32]int, kinds ...string) map[uint32]int {
	out := make(map[uint32]int)
	for _, k := range kinds {
		for ledger, c := range byKind[k] {
			out[ledger] += c
		}
	}
	return out
}

// ReconcileCounts diffs expected (decoder re-derive) against actual (protocol-table rows) per ledger and returns every disagreeing ledger, ascending; COUNT-ONLY, so an empty result does not certify derived VALUES.
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
