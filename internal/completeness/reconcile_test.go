package completeness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── fakes ──────────────────────────────────────────────────────

type fakeOutput struct{}

func (fakeOutput) Source() string    { return "fake" }
func (fakeOutput) EventKind() string { return "trade" }

// twoKindDecoder emits one "x.trade" and one "x.liquidity" output per
// matched event — the multi-table shape (one decoder, two destinations).
type twoKindDecoder struct{}

func (twoKindDecoder) Matches(ev events.Event) bool { return ev.ContractID == "MATCH" }
func (twoKindDecoder) Decode(events.Event) ([]consumer.Event, error) {
	return []consumer.Event{kindOutput("x.trade"), kindOutput("x.liquidity")}, nil
}

type kindOutput string

func (k kindOutput) Source() string    { return "x" }
func (k kindOutput) EventKind() string { return string(k) }

func TestReDeriveOutputCountsByKindFromEvents(t *testing.T) {
	es := fakeEventStreamer{evs: []events.Event{
		evAt(100, "MATCH", 0),   // → x.trade@100, x.liquidity@100
		evAt(100, "MATCH", 1),   // → x.trade@100, x.liquidity@100 (each kind = 2 @100)
		evAt(100, "MATCH", 1),   // un-merged duplicate part of the event above → counted once
		evAt(101, "NOMATCH", 0), // skipped
		evAt(102, "MATCH", 0),   // → x.trade@102, x.liquidity@102
		evAt(999, "MATCH", 0),   // out of range → skipped
	}}
	byKind, blind, err := ReDeriveOutputCountsByKindFromEvents(context.Background(), es, twoKindDecoder{}, nil, nil, 100, 102)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKindFromEvents: %v", err)
	}
	if blind.Any() {
		t.Errorf("clean stream reported blind spots: %+v", blind)
	}
	if byKind["x.trade"][100] != 2 || byKind["x.trade"][102] != 1 {
		t.Errorf("x.trade counts = %v, want {100:2,102:1}", byKind["x.trade"])
	}
	if byKind["x.liquidity"][100] != 2 || byKind["x.liquidity"][102] != 1 {
		t.Errorf("x.liquidity counts = %v, want {100:2,102:1}", byKind["x.liquidity"])
	}

	// SumKinds projects to the table's kinds only — trades table sees
	// only x.trade, not x.liquidity (the overcount bug this fixes).
	trades := SumKinds(byKind, "x.trade")
	if trades[100] != 2 || trades[102] != 1 || len(trades) != 2 {
		t.Errorf("SumKinds(x.trade) = %v, want {100:2,102:1}", trades)
	}
	both := SumKinds(byKind, "x.trade", "x.liquidity")
	if both[100] != 4 || both[102] != 2 {
		t.Errorf("SumKinds(both) = %v, want {100:4,102:2}", both)
	}
	if len(SumKinds(byKind, "nonexistent.kind")) != 0 {
		t.Error("SumKinds(unknown kind) should be empty")
	}
}

func TestReconcileCounts(t *testing.T) {
	tests := []struct {
		name             string
		expected, actual map[uint32]int
		want             []ProjectionGap
	}{
		{
			name:     "all match",
			expected: map[uint32]int{100: 2, 101: 1},
			actual:   map[uint32]int{100: 2, 101: 1},
			want:     nil,
		},
		{
			name:     "projection drop",
			expected: map[uint32]int{100: 2, 101: 1},
			actual:   map[uint32]int{100: 2, 101: 0},
			want:     []ProjectionGap{{Ledger: 101, Expected: 1, Actual: 0}},
		},
		{
			name:     "phantom row",
			expected: map[uint32]int{100: 2},
			actual:   map[uint32]int{100: 2, 200: 3},
			want:     []ProjectionGap{{Ledger: 200, Expected: 0, Actual: 3}},
		},
		{
			name:     "sorted multi-gap",
			expected: map[uint32]int{300: 5, 100: 1, 200: 2},
			actual:   map[uint32]int{300: 4, 100: 1, 200: 0},
			want:     []ProjectionGap{{Ledger: 200, Expected: 2, Actual: 0}, {Ledger: 300, Expected: 5, Actual: 4}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ReconcileCounts(tc.expected, tc.actual)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("gap[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ─── C4-059: the symmetric-soft-fail blind spot ─────────────────

// brokenDecoder MATCHES every "MATCH" row and then fails Decode on rows in
// one specific ledger — the shape of a real decoder bug (a payload variant
// the decoder claims by topic and then cannot parse). The projector running
// the SAME decoder over the SAME bytes drops those rows too, which is what
// makes the defect invisible to a row-count reconcile.
type brokenDecoder struct{ badLedger uint32 }

func (brokenDecoder) Matches(ev events.Event) bool { return ev.ContractID == "MATCH" }

func (d brokenDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if ev.Ledger == d.badLedger {
		return nil, errors.New("decode: unsupported payload variant")
	}
	return []consumer.Event{fakeOutput{}}, nil
}

// TestReDeriveOutputCountsByKindFromEvents_UndecodableMatchedNetsToZero is the C4-059
// regression (audit-2026-07-23).
//
// Scenario: ledger 101 holds two events the decoder CLAIMS (Matches == true)
// and then fails to Decode. The projector failed on the identical rows when
// it ran, so the served table holds zero rows for ledger 101 too.
//
// The row-count reconcile therefore reports ledger 101 CLEAN — expected 0,
// actual 0 — even though two rows were provably dropped from the served tier.
// That netting is structural: the check is "expected == actual" and both
// sides are blind in the same place, so no assertion over the counts can
// ever catch it.
//
// The only signal is the blindness itself. This asserts the re-derive
// reports it, with the exact affected ledger and the correct per-class
// counts, so the caller can refuse to certify the range.
func TestReDeriveOutputCountsByKindFromEvents_UndecodableMatchedNetsToZero(t *testing.T) {
	const badLedger uint32 = 101
	es := fakeEventStreamer{evs: []events.Event{
		evAt(100, "MATCH", 0),       // decodes → 1 output
		evAt(badLedger, "MATCH", 0), // matched, Decode fails → dropped
		evAt(badLedger, "MATCH", 1), // matched, Decode fails → dropped
		evAt(102, "MATCH", 0),       // decodes → 1 output
	}}

	byKind, blind, err := ReDeriveOutputCountsByKindFromEvents(context.Background(), es, brokenDecoder{badLedger: badLedger}, nil, nil, 100, 102)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKindFromEvents: %v", err)
	}
	expected := SumKinds(byKind, "trade")

	// Pre-condition: the defect really is invisible to the counts. The
	// projector dropped the same two rows, so actual matches expected
	// exactly and ReconcileCounts sees a clean range.
	actual := map[uint32]int{100: 1, 102: 1}
	if gaps := ReconcileCounts(expected, actual); len(gaps) != 0 {
		t.Fatalf("precondition failed: symmetric drop should net to zero gaps, got %+v", gaps)
	}

	// The fix: the blindness is reported.
	if !blind.Any() {
		t.Fatal("re-derive reported no blind spots: two matched-but-undecodable rows netted to zero and the range would certify as complete")
	}
	if got, want := blind.UndecodableMatched, 2; got != want {
		t.Errorf("UndecodableMatched = %d, want %d", got, want)
	}
	if got, want := blind.Unreconstructable, 0; got != want {
		t.Errorf("Unreconstructable = %d, want %d", got, want)
	}
	if got, want := blind.Rows(), 2; got != want {
		t.Errorf("Rows() = %d, want %d", got, want)
	}
	if got, want := len(blind.Ledgers), 1; got != want {
		t.Fatalf("Ledgers = %v, want exactly %d entry", blind.Ledgers, want)
	}
	if blind.Ledgers[0] != badLedger {
		t.Errorf("Ledgers[0] = %d, want %d (the ledger whose rows both sides dropped)", blind.Ledgers[0], badLedger)
	}
	if d := blind.Detail(); !strings.Contains(d, "undecodable-but-matched") || !strings.Contains(d, "101") {
		t.Errorf("Detail() = %q, want it to name the class and the first affected ledger", d)
	}
}

// ─── recovered decoder PANIC == a blind spot, never a crash ─────

// panicDecoder MATCHES every "MATCH" row and PANICS (rather than returning an
// error) on rows in one specific ledger — the shape of a decoder that hits an
// upgraded / historical WASM event shape it cannot parse. The projector
// RECOVERS this exact panic in processEventSafely, drops the row, and advances
// the cursor ("backfill sees every prior version"), so the served tier is
// missing whatever that event would have produced. The re-derive — the
// mechanism that exists to make such drops VISIBLE — must recover it the SAME
// way (a blind spot); if it let the panic escape, the whole
// compute-completeness run would crash before writing any completeness_snapshot
// and EVERY source's verdict would freeze, hiding the loss behind a
// crash-looping ops job.
type panicDecoder struct{ panicLedger uint32 }

func (panicDecoder) Matches(ev events.Event) bool { return ev.ContractID == "MATCH" }

func (d panicDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if ev.Ledger == d.panicLedger {
		panic("decode: nil-map read on upgraded WASM event shape")
	}
	return []consumer.Event{fakeOutput{}}, nil
}

// fakeEventStreamer feeds ReDeriveOutputCountsByKindFromEvents.
type fakeEventStreamer struct{ evs []events.Event }

func (f fakeEventStreamer) StreamContractEvents(
	_ context.Context, from, to uint32, _ []string, _ []string,
	fn func(events.Event) error,
) error {
	for _, ev := range f.evs {
		if ev.Ledger < from || ev.Ledger > to {
			continue
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

func evAt(ledger uint32, contractID string, eventIndex int) events.Event {
	return events.Event{
		Ledger:         ledger,
		ContractID:     contractID,
		TxHash:         "tx",
		OperationIndex: 0,
		EventIndex:     eventIndex, // distinct so the CH-path adjacent-dup skip keeps each
	}
}

// TestReDeriveOutputCountsByKindFromEvents_RecoversDecoderPanic proves the
// re-derive RECOVERS a decoder panic and records it as a blind spot instead of
// letting it crash the compute-completeness run, which would write no
// completeness_snapshot and freeze every source's verdict.
func TestReDeriveOutputCountsByKindFromEvents_RecoversDecoderPanic(t *testing.T) {
	const panicLedger uint32 = 300
	es := fakeEventStreamer{evs: []events.Event{
		evAt(299, "MATCH", 0),         // decodes normally → trade@299
		evAt(panicLedger, "MATCH", 0), // Decode PANICS → recovered as a blind spot
	}}
	byKind, blind, err := ReDeriveOutputCountsByKindFromEvents(
		context.Background(), es, panicDecoder{panicLedger: panicLedger}, nil, nil, 299, 300)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKindFromEvents returned error: %v", err)
	}
	if byKind["trade"][299] != 1 {
		t.Errorf("ledger 299 = %d trade outputs, want 1", byKind["trade"][299])
	}
	if byKind["trade"][panicLedger] != 0 {
		t.Errorf("panicking ledger contributed %d outputs, want 0", byKind["trade"][panicLedger])
	}
	if got, want := blind.UndecodableMatched, 1; got != want {
		t.Errorf("UndecodableMatched = %d, want %d", got, want)
	}
	if len(blind.Ledgers) != 1 || blind.Ledgers[0] != panicLedger {
		t.Errorf("Ledgers = %v, want [%d]", blind.Ledgers, panicLedger)
	}
}

// TestBlindSpots_LedgersSortedAndDeduped — Ledgers[0] is read as "the first
// affected ledger" by the verdict detail, so ordering is load-bearing even
// when the stream arrives out of order.
func TestBlindSpots_LedgersSortedAndDeduped(t *testing.T) {
	tr := NewBlindTracker()
	tr.Undecodable(500)
	tr.Unreconstructable(400)
	tr.Undecodable(500)
	tr.Undecodable(450)

	got := tr.Result()
	want := []uint32{400, 450, 500}
	if len(got.Ledgers) != len(want) {
		t.Fatalf("Ledgers = %v, want %v", got.Ledgers, want)
	}
	for i := range want {
		if got.Ledgers[i] != want[i] {
			t.Fatalf("Ledgers = %v, want %v", got.Ledgers, want)
		}
	}
	if got.Rows() != 4 {
		t.Errorf("Rows() = %d, want 4", got.Rows())
	}
}

// sweptOutput models a correlation-buffer rescue output: it carries its OWN
// ledger (the group's first-field ledger) and is emitted while the decoder is
// processing a LATER stream event — the phoenix 7-field-era sweep shape.
type sweptOutput struct{ ledger uint32 }

func (sweptOutput) Source() string        { return "swept" }
func (sweptOutput) EventKind() string     { return "swept.trade" }
func (o sweptOutput) EventLedger() uint32 { return o.ledger }

// sweepDecoder buffers its first matched event and, on the second, emits both
// the second event's own output (no carrier) and the FIRST event's rescued
// output (carrier, first event's ledger) — mirroring absorb()'s evict+rescue.
type sweepDecoder struct{ pending []uint32 }

func (*sweepDecoder) Matches(ev events.Event) bool { return ev.ContractID == "MATCH" }
func (d *sweepDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	if len(d.pending) == 0 {
		d.pending = append(d.pending, ev.Ledger)
		return nil, nil // group incomplete — sits in the buffer
	}
	rescued := sweptOutput{ledger: d.pending[0]}
	d.pending = d.pending[:0]
	return []consumer.Event{fakeOutput{}, rescued}, nil
}

// TestReDeriveOutputCountsByKindFromEvents_SweptOutputCountsAtOwnLedger pins
// the eventLedgerCarrier contract: a sweep-rescued output is counted at ITS
// ledger (where the served row lives), not at the sweep-trigger event's —
// while a plain output still counts at the stream event's ledger. Without the
// carrier, the rescued trade lands at the trigger ledger and a strict
// per-ledger reconcile reports a ± shift pair (missing at 100 / phantom at
// 160) against a served tier that is perfectly correct — the CS-084 noise
// that forced phoenix onto aggregate netting.
func TestReDeriveOutputCountsByKindFromEvents_SweptOutputCountsAtOwnLedger(t *testing.T) {
	es := fakeEventStreamer{evs: []events.Event{
		evAt(100, "MATCH", 0), // first field of the buffered group
		evAt(160, "MATCH", 1), // the sweep trigger, 60 ledgers later
	}}
	byKind, _, err := ReDeriveOutputCountsByKindFromEvents(
		context.Background(), es, &sweepDecoder{}, nil, nil, 1, 200)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKindFromEvents: %v", err)
	}
	if got, want := byKind["swept.trade"][100], 1; got != want {
		t.Errorf("swept.trade at OWN ledger 100 = %d, want %d (eventLedgerCarrier ignored?)", got, want)
	}
	if got := byKind["swept.trade"][160]; got != 0 {
		t.Errorf("swept.trade at trigger ledger 160 = %d, want 0 — counted at the sweep trigger, the exact CS-084 shift", got)
	}
	if got, want := byKind["trade"][160], 1; got != want {
		t.Errorf("plain trade at stream ledger 160 = %d, want %d", got, want)
	}
}

// panickingMatcher panics in Matches — the half of the Decoder pair that
// safeDecode never covered. Matches type-asserts topic vectors and reads
// body fields to decide ownership, so a malformed row panics there just as
// readily as in Decode.
type panickingMatcher struct{ panicOnMatch bool }

func (p panickingMatcher) Matches(events.Event) bool {
	if p.panicOnMatch {
		panic("malformed topic vector")
	}
	return true
}

func (p panickingMatcher) Decode(events.Event) ([]consumer.Event, error) {
	panic("decode should not be reached when Matches panics")
}

// TestSafeMatches_PanicBecomesAnError pins the ops-path twin of the
// dispatcher's poison-ledger class (#371 F1): a decoder that panics while
// DECIDING OWNERSHIP must not take the reconciling binary down with it. It
// must read as an error so the caller records a blind spot — a ledger the
// re-derive could not evaluate must never be certified clean.
func TestSafeMatches_PanicBecomesAnError(t *testing.T) {
	matched, err := safeMatches(panickingMatcher{panicOnMatch: true}, events.Event{})
	if err == nil {
		t.Fatal("a panicking Matches returned no error — the panic escaped, or was swallowed silently")
	}
	if matched {
		t.Error("a panicking Matches must never report a match")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("error should name the panic, got %q", err)
	}

	matched, err = safeMatches(panickingMatcher{}, events.Event{})
	if err != nil || !matched {
		t.Errorf("a well-behaved Matches must pass through unchanged: matched=%v err=%v", matched, err)
	}
}
