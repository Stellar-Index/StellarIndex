package dispatcher

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

type rowDecoder struct {
	matches, panicMatches, panicDecode bool
	err                                error
}

func (rowDecoder) Name() string { return "row" }

func (d rowDecoder) Matches(events.Event) bool {
	if d.panicMatches {
		panic("matches boom")
	}
	return d.matches
}

func (d rowDecoder) Decode(events.Event) ([]consumer.Event, error) {
	if d.panicDecode {
		panic("decode boom")
	}
	return nil, d.err
}

func TestDecodeRow_Outcomes(t *testing.T) {
	bad := errors.New("bad row")
	cases := []struct {
		name        string
		dec         rowDecoder
		wantMatched bool
		wantErr     error
		wantPanics  float64
	}{
		{"no_match", rowDecoder{}, false, nil, 0},
		{"ok", rowDecoder{matches: true}, true, nil, 0},
		{"decode_error", rowDecoder{matches: true, err: bad}, true, bad, 0},
		{"decode_panic", rowDecoder{matches: true, panicDecode: true}, true, ErrDecoderPanic, 1},
		{"matches_panic", rowDecoder{panicMatches: true}, true, ErrDecoderPanic, 1},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label := "decoderow_" + tc.name
			before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(label))
			outs, matched, err := DecodeRow(label, tc.dec, events.Event{Ledger: 7}, log)
			if matched != tc.wantMatched || outs != nil {
				t.Errorf("matched=%v outs=%v, want %v/nil", matched, outs, tc.wantMatched)
			}
			if (err == nil) != (tc.wantErr == nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if got := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(label)) - before; got != tc.wantPanics {
				t.Errorf("DecoderPanicsTotal delta = %v, want %v", got, tc.wantPanics)
			}
		})
	}
}
