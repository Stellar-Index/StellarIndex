package projector

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// RLT-133: the projector skips a failed row and advances the cursor past it,
// so the failure itself must reach the operator — the decode error text in the
// log, and a panic in the stellarindex_decoder_panicked page counter.
func TestProcessEventSafely_DecodeErrorIsLoggedWithRowCoordinate(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	src := Source{Name: "rlt133_err", Decoder: &fakeDecoder{matches: true, err: errors.New("unknown map field amount_v9")}}
	_, decodeFail, _ := processEventSafely(src, events.Event{Ledger: 61234567, TxHash: "abc"},
		func(consumer.Event) error { return nil }, log)
	if !decodeFail {
		t.Fatal("decode error must still be a soft-fail")
	}
	out := buf.String()
	for _, want := range []string{"unknown map field amount_v9", "source=rlt133_err", "ledger=61234567", "tx=abc"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

func TestProcessEventSafely_PanicCountsDecoderPanicsTotal(t *testing.T) {
	const name = "rlt133_panic"
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(name))
	src := Source{Name: name, Decoder: &fakeDecoder{matches: true, panics: true}}
	_, decodeFail, _ := processEventSafely(src, events.Event{Ledger: 42},
		func(consumer.Event) error { return nil }, discardLog())
	if !decodeFail {
		t.Fatal("decode panic must still be a soft-fail")
	}
	if got := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(name)) - before; got != 1 {
		t.Errorf("DecoderPanicsTotal{source=%q} delta = %v, want 1", name, got)
	}
}
