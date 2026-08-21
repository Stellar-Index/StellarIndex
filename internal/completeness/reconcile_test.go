package completeness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// ─── fakes ──────────────────────────────────────────────────────

type fakeStreamer struct{ rows []sorobanevents.Row }

func (f fakeStreamer) StreamSorobanEvents(
	_ context.Context, from, to uint32, _ []string, _ []string, _ []string,
	fn func(sorobanevents.Row) error,
) error {
	for _, r := range f.rows {
		if r.Ledger < from || r.Ledger > to {
			continue
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// matchDecoder matches rows whose ContractID is "MATCH" and emits one
// output each — enough to exercise per-ledger output counting.
type matchDecoder struct{}

func (matchDecoder) Matches(ev events.Event) bool { return ev.ContractID == "MATCH" }
func (matchDecoder) Decode(events.Event) ([]consumer.Event, error) {
	return []consumer.Event{fakeOutput{}}, nil
}

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

func rowAt(ledger uint32, contractID string) sorobanevents.Row {
	return sorobanevents.Row{
		Ledger:          ledger,
		LedgerCloseTime: time.Unix(int64(ledger), 0).UTC(),
		TxHash:          make([]byte, 32),
		ContractID:      contractID,
		ContractIDHex:   make([]byte, 32),
		TopicCount:      1,
		Topic0XDR:       []byte{0x00, 0x00, 0x00, 0x01},
		BodyXDR:         []byte{0x00, 0x00, 0x00, 0x00},
	}
}

// ─── tests ──────────────────────────────────────────────────────

func TestReDeriveOutputCounts(t *testing.T) {
	s := fakeStreamer{rows: []sorobanevents.Row{
		rowAt(100, "MATCH"),   // → 1 output
		rowAt(100, "MATCH"),   // → 1 output (ledger 100 total 2)
		rowAt(101, "NOMATCH"), // Matches=false → 0
		rowAt(102, "MATCH"),   // → 1 output
		rowAt(999, "MATCH"),   // out of range → skipped
	}}
	got, blind, err := ReDeriveOutputCounts(context.Background(), s, matchDecoder{}, nil, nil, 100, 102)
	if err != nil {
		t.Fatalf("ReDeriveOutputCounts: %v", err)
	}
	if blind.Any() {
		t.Errorf("clean stream reported blind spots: %+v", blind)
	}
	want := map[uint32]int{100: 2, 102: 1}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for l, w := range want {
		if got[l] != w {
			t.Errorf("ledger %d = %d, want %d", l, got[l], w)
		}
	}
}

func TestReDeriveOutputCountsByKind(t *testing.T) {
	s := fakeStreamer{rows: []sorobanevents.Row{
		rowAt(100, "MATCH"),   // → x.trade@100, x.liquidity@100
		rowAt(100, "MATCH"),   // → x.trade@100, x.liquidity@100 (each kind = 2 @100)
		rowAt(101, "NOMATCH"), // skipped
		rowAt(102, "MATCH"),   // → x.trade@102, x.liquidity@102
	}}
	byKind, blind, err := ReDeriveOutputCountsByKind(context.Background(), s, twoKindDecoder{}, nil, nil, 100, 102)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKind: %v", err)
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

// TestReDeriveOutputCounts_UndecodableMatchedNetsToZero is the C4-059
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
func TestReDeriveOutputCounts_UndecodableMatchedNetsToZero(t *testing.T) {
	const badLedger uint32 = 101
	s := fakeStreamer{rows: []sorobanevents.Row{
		rowAt(100, "MATCH"),       // decodes → 1 output
		rowAt(badLedger, "MATCH"), // matched, Decode fails → dropped
		rowAt(badLedger, "MATCH"), // matched, Decode fails → dropped
		rowAt(102, "MATCH"),       // decodes → 1 output
	}}

	expected, blind, err := ReDeriveOutputCounts(context.Background(), s, brokenDecoder{badLedger: badLedger}, nil, nil, 100, 102)
	if err != nil {
		t.Fatalf("ReDeriveOutputCounts: %v", err)
	}

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

// TestReDeriveOutputCountsByKind_UndecodableMatched — the by-kind sibling
// carries the same accounting, since it is the entry point every
// multi-table source (and compute-completeness' Postgres branch) uses.
func TestReDeriveOutputCountsByKind_UndecodableMatched(t *testing.T) {
	const badLedger uint32 = 200
	s := fakeStreamer{rows: []sorobanevents.Row{
		rowAt(199, "MATCH"),
		rowAt(badLedger, "MATCH"),
	}}
	byKind, blind, err := ReDeriveOutputCountsByKind(context.Background(), s, brokenDecoder{badLedger: badLedger}, nil, nil, 199, 200)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKind: %v", err)
	}
	if byKind["trade"][badLedger] != 0 {
		t.Errorf("bad ledger contributed %d outputs, want 0 (Decode failed)", byKind["trade"][badLedger])
	}
	if got, want := blind.UndecodableMatched, 1; got != want {
		t.Errorf("UndecodableMatched = %d, want %d", got, want)
	}
	if len(blind.Ledgers) != 1 || blind.Ledgers[0] != badLedger {
		t.Errorf("Ledgers = %v, want [%d]", blind.Ledgers, badLedger)
	}
}

// TestReDeriveOutputCounts_UnreconstructableRow — a stored row too broken to
// rebuild into an events.Event is the OTHER symmetric drop: the projector
// could not reconstruct it either, so its outputs are missing from the served
// tier while the diff stays zero. Counted in its own class because it cannot
// be attributed to a decoder (Matches is never reached).
func TestReDeriveOutputCounts_UnreconstructableRow(t *testing.T) {
	broken := rowAt(300, "MATCH")
	broken.TxHash = make([]byte, 8) // Reconstruct requires exactly 32 bytes

	s := fakeStreamer{rows: []sorobanevents.Row{rowAt(299, "MATCH"), broken}}
	counts, blind, err := ReDeriveOutputCounts(context.Background(), s, matchDecoder{}, nil, nil, 299, 300)
	if err != nil {
		t.Fatalf("ReDeriveOutputCounts: %v", err)
	}
	if counts[300] != 0 {
		t.Errorf("unreconstructable row contributed %d outputs, want 0", counts[300])
	}
	if got, want := blind.Unreconstructable, 1; got != want {
		t.Errorf("Unreconstructable = %d, want %d", got, want)
	}
	if got, want := blind.UndecodableMatched, 0; got != want {
		t.Errorf("UndecodableMatched = %d, want %d", got, want)
	}
	if len(blind.Ledgers) != 1 || blind.Ledgers[0] != 300 {
		t.Errorf("Ledgers = %v, want [300]", blind.Ledgers)
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

// fakeEventStreamer feeds the ClickHouse-lake re-derive path
// (ReDeriveOutputCountsByKindFromEvents), which streams events.Event directly
// with no soroban_events Row round-trip.
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

// TestReDeriveOutputCounts_RecoversDecoderPanic proves the Postgres
// soroban_events re-derive RECOVERS a decoder panic and records it as a blind
// spot instead of propagating it. Without the safeDecode recover the panic
// escapes ReDeriveOutputCounts and crashes the compute-completeness run — no
// completeness_snapshot, projection_ok never flips false, the drop is invisible
// and every source's verdict freezes. With it, the panic feeds the SAME
// blind-spot path a returned decode error takes, so the loss is reported.
func TestReDeriveOutputCounts_RecoversDecoderPanic(t *testing.T) {
	const panicLedger uint32 = 101
	s := fakeStreamer{rows: []sorobanevents.Row{
		rowAt(100, "MATCH"),         // decodes normally → 1 output
		rowAt(panicLedger, "MATCH"), // Decode PANICS → recovered as a blind spot
		rowAt(panicLedger, "MATCH"), // Decode PANICS → recovered as a blind spot
		rowAt(102, "MATCH"),         // decodes normally → 1 output
	}}

	// The load-bearing assertion is that this call RETURNS. Without the
	// recover, the panic propagates out of ReDeriveOutputCounts and the test
	// aborts here before any assertion runs (prove-red).
	counts, blind, err := ReDeriveOutputCounts(
		context.Background(), s, panicDecoder{panicLedger: panicLedger}, nil, nil, 100, 102)
	if err != nil {
		t.Fatalf("ReDeriveOutputCounts returned error: %v", err)
	}

	// A normal event still decodes — the recover is per-event, not a
	// reconcile-wide abort.
	if counts[100] != 1 || counts[102] != 1 {
		t.Errorf("non-panicking ledgers = {100:%d,102:%d}, want {100:1,102:1}", counts[100], counts[102])
	}
	if counts[panicLedger] != 0 {
		t.Errorf("panicking ledger contributed %d outputs, want 0", counts[panicLedger])
	}

	// The panic feeds the SAME blind-spot accounting a returned decode error
	// does: two matched-but-undecodable rows on one ledger.
	if !blind.Any() {
		t.Fatal("recovered panic reported no blind spots: the drop would net to zero and certify the range complete")
	}
	if got, want := blind.UndecodableMatched, 2; got != want {
		t.Errorf("UndecodableMatched = %d, want %d", got, want)
	}
	if got, want := blind.Unreconstructable, 0; got != want {
		t.Errorf("Unreconstructable = %d, want %d", got, want)
	}
	if len(blind.Ledgers) != 1 || blind.Ledgers[0] != panicLedger {
		t.Errorf("Ledgers = %v, want [%d]", blind.Ledgers, panicLedger)
	}
}

// TestReDeriveOutputCountsByKind_RecoversDecoderPanic covers the multi-table
// Postgres entry point (the compute-completeness Postgres branch + the
// verify-reconciliation tool both call it), which had the same bare Decode.
func TestReDeriveOutputCountsByKind_RecoversDecoderPanic(t *testing.T) {
	const panicLedger uint32 = 200
	s := fakeStreamer{rows: []sorobanevents.Row{
		rowAt(199, "MATCH"),         // decodes normally → trade@199
		rowAt(panicLedger, "MATCH"), // Decode PANICS → recovered as a blind spot
	}}
	byKind, blind, err := ReDeriveOutputCountsByKind(
		context.Background(), s, panicDecoder{panicLedger: panicLedger}, nil, nil, 199, 200)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKind returned error: %v", err)
	}
	if byKind["trade"][199] != 1 {
		t.Errorf("ledger 199 = %d trade outputs, want 1", byKind["trade"][199])
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

// TestReDeriveOutputCountsByKindFromEvents_RecoversDecoderPanic covers the
// ClickHouse-lake entry point — the one compute-completeness uses by default
// (storage.clickhouse_projector_source). It had the same bare Decode.
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
