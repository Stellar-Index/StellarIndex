package dispatcher

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ─── #371 F1: a poison ledger must not crash-loop the indexer ────────
//
// Before the guard, a panic anywhere in a decoder's Matches or Decode
// unwound through ProcessLedger and was caught only at LEDGER
// granularity in pipeline.ProcessLedger, which returned it as an error
// — discarding EVERY source's outputs for that ledger, refusing the
// cursor advance, and (via the indexer's realMain) exiting the process.
// systemd restarted, the same ledger was re-read from the same cursor,
// the same event panicked, and StartLimitBurst restarts later the unit
// parked in `failed`. These tests pin the replacement contract: the ONE
// input is skipped and counted, the sibling decoders and the rest of
// the ledger complete, and the panic is loud (counter + Error log).
//
// Proven red: with the recover()/defer blocks removed from the four
// dispatch seams, every test below dies with the raw panic instead of
// reaching an assertion.

// panickyDecoder panics in Matches or Decode for events emitted by
// `contract`, and behaves like an ordinary non-matching decoder for
// everything else — so a ProcessLedger test can prove the SIBLING
// event still routes normally through the same decoder chain.
type panickyDecoder struct {
	name          string
	contract      string
	panicInMatch  bool
	panicInDecode bool
	decodeCalls   int
}

func (p *panickyDecoder) Name() string { return p.name }

func (p *panickyDecoder) Matches(ev events.Event) bool {
	if ev.ContractID != p.contract {
		return false
	}
	if p.panicInMatch {
		panic("simulated Matches fault: index out of range [3] with length 2")
	}
	return true
}

func (p *panickyDecoder) Decode(events.Event) ([]consumer.Event, error) {
	p.decodeCalls++
	if p.panicInDecode {
		panic("simulated Decode fault: nil map write")
	}
	return nil, nil
}

// counters reads the three signals a recovered panic must move.
func counters(t *testing.T, d *Dispatcher, source string) (panics float64, seen, decodeErrs int) {
	t.Helper()
	s := d.Stats()
	return testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(source)),
		s.EventsSeen[source], s.DecodeErrors[source]
}

func TestDispatchOne_DecodePanicBecomesDecodeError(t *testing.T) {
	dec := &panickyDecoder{name: "panic-decode-src", contract: "CPOISON", panicInDecode: true}
	disp := New(dec)
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name))

	// Reaching the next line at all is the containment proof: without
	// the seam's recover the panic escapes into this test function.
	outs, err := disp.dispatchOne(events.Event{ContractID: "CPOISON", Ledger: 63_332_650, TxHash: "deadbeef", OperationIndex: 2})

	if !errors.Is(err, ErrDecoderPanic) {
		t.Fatalf("err = %v, want an ErrDecoderPanic-wrapped error", err)
	}
	if !strings.Contains(err.Error(), "nil map write") {
		t.Errorf("err = %q, want the recovered panic value carried through", err)
	}
	if outs != nil {
		t.Errorf("outputs = %v, want nil (a crashed decoder emits nothing)", outs)
	}
	panics, seen, decodeErrs := counters(t, disp, dec.name)
	if panics-before != 1 {
		t.Errorf("stellarindex_decoder_panics_total{source=%s} rose by %v, want 1", dec.name, panics-before)
	}
	if decodeErrs != 1 {
		t.Errorf("Stats().DecodeErrors[%s] = %d, want 1 — a panic IS a decode error", dec.name, decodeErrs)
	}
	// Denominator honesty: exactly one input attempted per error, so
	// the decoder error-rate stays a rate (see SourceMatchedEventsTotal).
	if seen != 1 {
		t.Errorf("Stats().EventsSeen[%s] = %d, want 1", dec.name, seen)
	}
}

func TestDispatchOne_MatchesPanicBecomesDecodeError(t *testing.T) {
	dec := &panickyDecoder{name: "panic-match-src", contract: "CPOISON", panicInMatch: true}
	disp := New(dec)
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name))

	_, err := disp.dispatchOne(events.Event{ContractID: "CPOISON", Ledger: 7, TxHash: "abc", OperationIndex: 0})

	if !errors.Is(err, ErrDecoderPanic) {
		t.Fatalf("err = %v, want an ErrDecoderPanic-wrapped error", err)
	}
	if dec.decodeCalls != 0 {
		t.Errorf("Decode ran %d times, want 0 — the panic happened in Matches", dec.decodeCalls)
	}
	panics, seen, decodeErrs := counters(t, disp, dec.name)
	if panics-before != 1 {
		t.Errorf("panic counter rose by %v, want 1", panics-before)
	}
	if decodeErrs != 1 {
		t.Errorf("Stats().DecodeErrors[%s] = %d, want 1", dec.name, decodeErrs)
	}
	// Matches panicked BEFORE bumpEventsSeen ran, so the guard has to
	// supply the missing input count itself or error-rate exceeds 100%.
	if seen != 1 {
		t.Errorf("Stats().EventsSeen[%s] = %d, want 1 (guard back-fills the denominator)", dec.name, seen)
	}
}

func TestDispatchOne_PanicLogsLedgerCoordinateAndStack(t *testing.T) {
	var buf bytes.Buffer
	disp := New(&panickyDecoder{name: "loud-src", contract: "CPOISON", panicInDecode: true})
	disp.SetLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))

	_, _ = disp.dispatchOne(events.Event{ContractID: "CPOISON", Ledger: 63_332_650, TxHash: "abc123", OperationIndex: 4})

	out := buf.String()
	// The runbook's first step is "pull the raw event out of the lake";
	// that needs (ledger, tx_hash, op_index, decoder) in the log line.
	for _, want := range []string{"decoder panicked", "loud-src", "63332650", "abc123", "\"op_index\":4", "nil map write", "\"stack\""} {
		if !strings.Contains(out, want) {
			t.Errorf("panic log is missing %q\nfull log: %s", want, out)
		}
	}
}

// TestProcessLedger_PanickingDecoderLosesOnlyItsOwnEvent is the
// end-to-end shape of the incident: ProcessLedger must return nil (so
// the caller persists the cursor and the process keeps running) while
// the SIBLING source's event still decodes.
func TestProcessLedger_PanickingDecoderLosesOnlyItsOwnEvent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		panicInMatch  bool
		panicInDecode bool
	}{
		{name: "panic_in_Decode", panicInDecode: true},
		{name: "panic_in_Matches", panicInMatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, _ := provInvokeOp(t, provContractA, "x")
			lcm := provLedger(t, op, []xdr.ContractEvent{
				provEvent("REDSTONE", provContractA), // routed to the broken decoder
				provEvent("REDSTONE", provContractB), // routed to the healthy one
			}, nil)

			strA, err := contractIDToStrkey(provContractA)
			if err != nil {
				t.Fatal(err)
			}
			strB, err := contractIDToStrkey(provContractB)
			if err != nil {
				t.Fatal(err)
			}

			broken := &panickyDecoder{
				name:          "broken-" + tc.name,
				contract:      strA,
				panicInMatch:  tc.panicInMatch,
				panicInDecode: tc.panicInDecode,
			}
			healthy := &provSpyDecoder{name: "healthy-" + tc.name, contract: strB}
			disp := New(broken, healthy) // broken registered FIRST
			before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(broken.name))

			outs, err := disp.ProcessLedger(lcm, testPassphrase)
			if err != nil {
				t.Fatalf("ProcessLedger = %v, want nil — one decoder's panic must not reject the whole ledger (that is the crash-loop)", err)
			}
			_ = outs
			if len(healthy.got) != 1 {
				t.Errorf("sibling decoder saw %d events, want 1 — the ledger walk must continue past the panic", len(healthy.got))
			}
			panics, _, decodeErrs := counters(t, disp, broken.name)
			if panics-before != 1 {
				t.Errorf("panic counter rose by %v, want 1", panics-before)
			}
			if decodeErrs != 1 {
				t.Errorf("Stats().DecodeErrors[%s] = %d, want 1 — the skip must be recorded, not silent", broken.name, decodeErrs)
			}
		})
	}
}

// ─── the other three seams (the four-seam sibling trap) ──────────────

type panickyOpDecoder struct{ name string }

func (p *panickyOpDecoder) Name() string               { return p.name }
func (p *panickyOpDecoder) Matches(xdr.Operation) bool { return true }
func (p *panickyOpDecoder) Decode(OpContext) ([]consumer.Event, error) {
	panic("simulated op-decoder fault")
}

func TestDispatchOp_PanicBecomesDecodeError(t *testing.T) {
	dec := &panickyOpDecoder{name: "panic-op-src"}
	disp := New()
	disp.AddOpDecoder(dec)
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name))

	outs, err := disp.RouteOp(OpContext{Ledger: 11, TxHash: "t", OpIndex: 1})

	if !errors.Is(err, ErrDecoderPanic) {
		t.Fatalf("err = %v, want ErrDecoderPanic", err)
	}
	if outs != nil {
		t.Errorf("outputs = %v, want nil", outs)
	}
	if got := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name)) - before; got != 1 {
		t.Errorf("panic counter rose by %v, want 1", got)
	}
	if n := disp.Stats().DecodeErrors[dec.name]; n != 1 {
		t.Errorf("Stats().DecodeErrors[%s] = %d, want 1", dec.name, n)
	}
}

type panickyEntryDecoder struct{ name string }

func (p *panickyEntryDecoder) Name() string { return p.name }
func (p *panickyEntryDecoder) Matches(xdr.LedgerEntryChange) bool {
	panic("simulated entry-decoder Matches fault")
}

func (p *panickyEntryDecoder) Decode(LedgerEntryChangeContext) ([]consumer.Event, error) {
	return nil, nil
}

func TestDispatchEntryChange_MatchesPanicBecomesDecodeError(t *testing.T) {
	dec := &panickyEntryDecoder{name: "panic-entry-src"}
	disp := New()
	disp.AddEntryDecoder(dec)
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name))

	_, err := disp.RouteEntryChange(LedgerEntryChangeContext{Ledger: 12, TxHash: "t", OpIndex: 0, Change: makeAccountChange(5)})

	if !errors.Is(err, ErrDecoderPanic) {
		t.Fatalf("err = %v, want ErrDecoderPanic", err)
	}
	if got := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name)) - before; got != 1 {
		t.Errorf("panic counter rose by %v, want 1", got)
	}
	if n := disp.Stats().DecodeErrors[dec.name]; n != 1 {
		t.Errorf("Stats().DecodeErrors[%s] = %d, want 1", dec.name, n)
	}
}

type panickyCCDecoder struct{ name string }

func (p *panickyCCDecoder) Name() string             { return p.name }
func (p *panickyCCDecoder) Matches(_, _ string) bool { return true }
func (p *panickyCCDecoder) Decode(ContractCallContext) ([]consumer.Event, error) {
	panic("simulated contract-call decoder fault")
}

func TestDispatchContractCall_PanicBecomesDecodeError(t *testing.T) {
	dec := &panickyCCDecoder{name: "panic-cc-src"}
	disp := New()
	disp.AddContractCallDecoder(dec)
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name))

	_, err := disp.RouteContractCall(ContractCallContext{
		Ledger:       13,
		ClosedAt:     time.Unix(1_770_000_000, 0).UTC(),
		TxHash:       "t",
		OpIndex:      0,
		ContractID:   "CBAND",
		FunctionName: "relay",
	})

	if !errors.Is(err, ErrDecoderPanic) {
		t.Fatalf("err = %v, want ErrDecoderPanic", err)
	}
	if got := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name)) - before; got != 1 {
		t.Errorf("panic counter rose by %v, want 1", got)
	}
	if n := disp.Stats().DecodeErrors[dec.name]; n != 1 {
		t.Errorf("Stats().DecodeErrors[%s] = %d, want 1", dec.name, n)
	}
}

// ─── the guard's deliberate boundary ─────────────────────────────────

type panickyRawSink struct{}

func (panickyRawSink) PushEvent(events.Event) { panic("lake sink fault") }

// TestDispatchOne_RawEventSinkPanicIsNotSwallowed pins the scope of the
// guard. The lake sink is the durable record that makes "skip the
// event" an acceptable trade at all — if IT is broken, skipping and
// carrying on would drop the event from the substrate too. So the guard
// is installed AFTER the raw-event hook and a sink fault keeps its
// pre-existing ledger-level handling (pipeline.ProcessLedger's recover
// → the ledger is refused → the cursor does not advance).
func TestDispatchOne_RawEventSinkPanicIsNotSwallowed(t *testing.T) {
	disp := New(&panickyDecoder{name: "unused-src", contract: "CX"})
	disp.SetRawEventSink(panickyRawSink{})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("raw-event-sink panic was swallowed by the decoder guard — it must still reach the ledger-level recover")
		}
	}()
	_, _ = disp.dispatchOne(events.Event{ContractID: "CX"})
}

// TestRecognize_PanicResolvesToUnrecognised covers the seam the #371 F1
// fix originally missed: Recognize walks every decoder's Matches, and it
// is called from the completeness recogniser and two ops subcommands,
// none of which recovered — so one malformed row took the whole
// verification run down with it.
//
// The DIRECTION of the answer is the load-bearing part. A panic must
// resolve to "not recognised", which pushes the shape into the
// unrecognised-on-unowned-contract bucket and turns the recognition axis
// RED. Naming the panicking decoder as the owner would certify a shape
// that nothing can actually decode — a false green on the one axis whose
// whole job is to catch shapes we do not understand.
func TestRecognize_PanicResolvesToUnrecognised(t *testing.T) {
	dec := &panickyDecoder{name: "panic-recognise-src", contract: "CPOISON", panicInMatch: true}
	disp := New(dec)
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name))

	name, ok := disp.Recognize(events.Event{ContractID: "CPOISON", Ledger: 9, TxHash: "def", OperationIndex: 1})

	if ok {
		t.Errorf("Recognize reported ok=true for a decoder that panicked — a shape nothing can decode must never be certified as owned")
	}
	if name != "" {
		t.Errorf("Recognize returned owner %q after a panic, want empty", name)
	}
	if got := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(dec.name)) - before; got != 1 {
		t.Errorf("panic counter rose by %v, want 1 — an ops-path panic must be as visible as an ingest one", got)
	}

	// A healthy decoder still recognises normally after the guard.
	good := &panickyDecoder{name: "healthy-src", contract: "CGOOD"}
	if n, ok := New(good).Recognize(events.Event{ContractID: "CGOOD"}); !ok || n != "healthy-src" {
		t.Errorf("healthy decoder: got (%q, %v), want (healthy-src, true)", n, ok)
	}
}
