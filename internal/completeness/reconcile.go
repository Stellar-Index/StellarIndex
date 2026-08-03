package completeness

import (
	"context"
	"fmt"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// Decoder is the per-source decode surface the reconciler re-runs over
// soroban_events. Satisfied by every internal/sources/<venue> decoder
// (and by dispatcher routing). Matches + Decode are the SAME functions
// the projector uses, so the re-derive is the deterministic
// recomputation of what the projector should have written — not a
// parallel reimplementation that could disagree for the wrong reason.
type Decoder interface {
	Matches(events.Event) bool
	Decode(events.Event) ([]consumer.Event, error)
}

// SorobanEventStreamer is the read side the reconciler needs.
// *timescale.Store satisfies it via StreamSorobanEvents.
type SorobanEventStreamer interface {
	StreamSorobanEvents(
		ctx context.Context,
		from, to uint32,
		contractIDs []string,
		topic0Syms []string,
		excludeTopic0Syms []string,
		fn func(sorobanevents.Row) error,
	) error
}

// ProjectionGap is a ledger where the number of rows the decoder would
// emit from soroban_events (Expected) disagrees with the number
// actually present in the protocol table (Actual). Expected > Actual is
// a projection/persistence drop; Actual > Expected is a phantom row
// (or a pre-event_index-fix duplicate). Either is a real discrepancy
// localized to one ledger (ADR-0033 Claim 2b).
//
// SCOPE (DAT-15): this is a ROW-COUNT check only. A ledger with zero
// gaps proves the RIGHT NUMBER of rows exist for that ledger — it does
// NOT prove those rows' derived columns (amounts, prices, decoded
// fields, …) are correct. A decoder bug that writes the right row
// count with wrong derived values passes ReconcileCounts silently. Any
// consumer of a clean ReconcileCounts result must not describe the
// range as "complete" or "verified correct" for VALUE correctness on
// the strength of this check alone.
type ProjectionGap struct {
	Ledger   uint32
	Expected int
	Actual   int
}

// BlindSpots records the raw rows a re-derive could not turn into a
// comparable expectation at all — the rows it SKIPPED rather than counted.
//
// C4-059 (audit-2026-07-23). The skip is a soft-fail that deliberately
// mirrors the projector's: a row the projector could not decode produced no
// served row, and a row the re-derive cannot decode produces no expected
// row, so ReconcileCounts sees expected == actual == 0 and reports the
// ledger CLEAN. That symmetry is the defect. The two sides fail for the SAME
// reason — the same Decode, on the same bytes — so a decoder bug or a
// truncated payload silently nets to zero and certifies `projection_ok` on a
// ledger where rows were provably dropped from the served tier.
//
// Row counts alone cannot detect this class by construction: the check is
// "expected == actual" and both sides are blind in the same place. The only
// honest signal is the blindness itself, which is why it is returned rather
// than logged — a caller certifying completeness must treat "I could not
// evaluate this ledger" as NOT-clean, never as clean.
//
// The old comment claimed "the recognition audit catches shape issues". That
// is true for UNRECOGNISED contract/topic shapes — events no decoder claims.
// It is NOT true for this class: an event the decoder DOES claim
// (Matches == true) and then fails to Decode is, to the recognition audit, a
// recognised event. Nothing else looks at it.
type BlindSpots struct {
	// Ledgers holds every ledger with at least one blind row, ascending and
	// deduped. Callers whose branch pins the watermark on problem ledgers
	// append these; callers whose branch flips a boolean use [BlindSpots.Any].
	Ledgers []uint32

	// UndecodableMatched counts rows the decoder CLAIMED (Matches == true)
	// and then failed to Decode. The precisely-attributed half: this source
	// owned the event and could not produce its expectation.
	UndecodableMatched int

	// Unreconstructable counts stored soroban_events rows too structurally
	// broken to rebuild into an events.Event at all (empty contract_id,
	// missing topic_0_xdr, wrong-length tx_hash, undecodable op_args). Matches
	// is never evaluated for these, so they cannot be attributed to a decoder
	// — but the projector dropped them for the identical reason, so the served
	// tier is genuinely missing whatever they would have produced. Blind is
	// blind; they pin too. Counted separately so a verdict says which class
	// fired. Always 0 on the ClickHouse path, which streams events.Event
	// directly with no Row round-trip.
	Unreconstructable int
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

// BlindTracker accumulates [BlindSpots] during a stream, keeping the ledger
// set deduped without holding a second copy of it.
//
// Exported because the re-derives in this package are NOT the only per-row
// soft-fail sites: the ContractCall census in internal/ops/chops skips a
// malformed call exactly the way the ch-rebuild writer does, with the same
// nets-to-zero consequence. One tracker implementation means one definition
// of "blind", and a new soft-fail site cannot quietly invent a weaker one.
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

// safeDecode runs dec.Decode under a per-event recover, converting a decoder
// PANIC into a returned error so the caller's existing decode-error path
// (blind.Undecodable + skip) handles it unchanged. It mirrors the projector's
// processEventSafely, where a recovered panic and a returned decode error are
// the SAME outcome — the row is dropped, the failure is counted, the cursor
// advances.
//
// This matters because the projector RECOVERS panics on historical /
// upgraded-WASM event shapes — a designed-for input ("backfill sees every
// prior version") — so a panicking decoder silently drops rows from the served
// tier. The completeness re-derive exists to make those drops VISIBLE
// (projection_ok=false / a BlindSpots entry). If it let the panic escape, the
// whole compute-completeness run would crash before writing any
// completeness_snapshot: no verdict would flip false, EVERY source's verdict
// would freeze, and the loss would surface only as a crash-looping ops job
// instead of the blind spot it is. Funnelling the panic into the same error
// path a returned decode error takes feeds it the SAME blind-spot accounting,
// so `delta==0 && !blind.Any()` correctly stays false.
func safeDecode(dec Decoder, ev events.Event) (outs []consumer.Event, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			outs, err = nil, fmt.Errorf("decoder panicked on event shape: %v", rec)
		}
	}()
	return dec.Decode(ev)
}

// ReDeriveOutputCounts re-runs the decoder over the raw events in
// [from, to] and returns how many outputs it emits per ledger. Outputs
// are attributed to the triggering row's ledger; because every
// correlation group (Phoenix's 8 events, Soroswap's swap+sync) shares a
// single (ledger, tx, op), the group always completes within its own
// ledger and the per-ledger count is exact.
//
// contractIDs / topic0Syms are the same SQL prefilters the projector
// passes for this source (empty = match-by-topic across all contracts).
//
// Malformed / undecodable rows are still SKIPPED (mirroring the projector's
// soft-fail) but are no longer silent: they are returned as [BlindSpots], so
// a caller certifying completeness can refuse to call a ledger clean that the
// re-derive could not actually evaluate. See [BlindSpots] for why a row-count
// reconcile cannot detect this class on its own.
func ReDeriveOutputCounts(
	ctx context.Context,
	s SorobanEventStreamer,
	dec Decoder,
	contractIDs, topic0Syms []string,
	from, to uint32,
) (map[uint32]int, BlindSpots, error) {
	counts := make(map[uint32]int)
	blind := NewBlindTracker()
	err := s.StreamSorobanEvents(ctx, from, to, contractIDs, topic0Syms, nil,
		func(row sorobanevents.Row) error {
			ev, rerr := sorobanevents.Reconstruct(row)
			if rerr != nil {
				blind.Unreconstructable(row.Ledger)
				return nil //nolint:nilerr // soft-fail like the projector; recorded as a blind spot, not swallowed.
			}
			if !dec.Matches(ev) {
				return nil
			}
			outs, derr := safeDecode(dec, ev)
			if derr != nil {
				blind.Undecodable(row.Ledger)
				return nil //nolint:nilerr // deterministically-broken row; skip the count, pin the verdict.
			}
			if len(outs) > 0 {
				counts[row.Ledger] += len(outs)
			}
			return nil
		})
	if err != nil {
		return nil, BlindSpots{}, err
	}
	return counts, blind.Result(), nil
}

// EventStreamer yields decoded-ready [events.Event] directly — the ClickHouse
// lake path, where contract_events ARE events.Event (no soroban_events Row +
// Reconstruct round-trip). A clickhouse adapter over StreamContractEventsFiltered
// satisfies it structurally.
type EventStreamer interface {
	StreamContractEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms []string, fn func(events.Event) error) error
}

// ReDeriveOutputCountsByKindFromEvents is [ReDeriveOutputCountsByKind] sourced
// from the CH lake instead of Postgres soroban_events: it streams events.Event
// straight from contract_events (no Reconstruct), decodes, and counts outputs
// by EventKind() per ledger. Same soft-fail semantics (an event that fails
// Decode is skipped, not fatal) and the same C4-059 accounting: every skipped
// match is returned as a [BlindSpots] entry so a clean reconcile over a range
// the decoder could not read does not certify as complete. Used by the
// CH-backed completeness verification so projection reconciliation reads the
// certified lake, off the serving DB.
//
// BlindSpots.Unreconstructable is always 0 here — this path has no
// soroban_events Row to rebuild.
func ReDeriveOutputCountsByKindFromEvents(
	ctx context.Context,
	es EventStreamer,
	dec Decoder,
	contractIDs, topic0Syms []string,
	from, to uint32,
) (map[string]map[uint32]int, BlindSpots, error) {
	byKind := make(map[string]map[uint32]int)
	blind := NewBlindTracker()
	// Adjacent-duplicate skip: the CH stream is ORDER BY (ledger, tx_hash,
	// op_index, event_index), so un-merged ReplacingMergeTree duplicate rows
	// (re-run partitions 25/45/62) arrive consecutively. Dedup by identity vs
	// the previous event — O(1) memory, and it lets the read stay no-FINAL
	// (gentle) while counting correctly.
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
			if !dec.Matches(ev) {
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
				byKind[k][ev.Ledger]++
			}
			return nil
		})
	if err != nil {
		return nil, BlindSpots{}, err
	}
	return byKind, blind.Result(), nil
}

// ReDeriveOutputCountsByKind is the multi-table generalization of
// ReDeriveOutputCounts: it returns the re-derived output counts keyed by
// the output's EventKind(), then by ledger. A single decoder routes
// different output kinds to different tables (soroswap emits
// "soroswap.trade" → trades AND "soroswap.skim" → soroswap_skim_events;
// blend emits five kinds across four tables), so reconciliation per
// table must count ONLY the kinds that land in that table — counting
// every output would overcount any table that receives a subset.
//
// Stream once per source; SumKinds then projects the kinds for each
// target table. Same soft-fail semantics as ReDeriveOutputCounts — and the
// same C4-059 [BlindSpots] accounting for the rows it soft-fails on.
func ReDeriveOutputCountsByKind(
	ctx context.Context,
	s SorobanEventStreamer,
	dec Decoder,
	contractIDs, topic0Syms []string,
	from, to uint32,
) (map[string]map[uint32]int, BlindSpots, error) {
	byKind := make(map[string]map[uint32]int)
	blind := NewBlindTracker()
	err := s.StreamSorobanEvents(ctx, from, to, contractIDs, topic0Syms, nil,
		func(row sorobanevents.Row) error {
			ev, rerr := sorobanevents.Reconstruct(row)
			if rerr != nil {
				blind.Unreconstructable(row.Ledger)
				return nil //nolint:nilerr // soft-fail like the projector; recorded as a blind spot.
			}
			if !dec.Matches(ev) {
				return nil
			}
			outs, derr := safeDecode(dec, ev)
			if derr != nil {
				blind.Undecodable(row.Ledger)
				return nil //nolint:nilerr // deterministically-broken row; skip the count, pin the verdict.
			}
			for _, out := range outs {
				k := out.EventKind()
				if byKind[k] == nil {
					byKind[k] = make(map[uint32]int)
				}
				byKind[k][row.Ledger]++
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

// ReconcileCounts diffs expected (decoder re-derive) against actual
// (protocol-table rows) per ledger and returns every ledger where they
// disagree, sorted ascending. An empty result means every ledger the
// decoder would have produced rows for has exactly those rows — Claim
// 2b's ROW-COUNT invariant holds for the range.
//
// This is COUNT-ONLY (see the ProjectionGap doc comment for the DAT-15
// caveat): an empty result does not certify that the rows' derived
// column VALUES are correct, only that the right number of rows exist
// per ledger. Do not label a range "complete" for value correctness on
// the strength of ReconcileCounts alone.
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
