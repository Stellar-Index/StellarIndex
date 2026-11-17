package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type explorerDecStub struct {
	d     uint32
	found bool
	err   error
}

func (e explorerDecStub) TokenDecimals(context.Context, string) (uint32, bool, error) {
	return e.d, e.found, e.err
}

// TestExplorerHandler_TokenDecimalsNeverGuessesSeven: the wired explorer seam must report ok=false for a scale the lake
// could not read, or movements and lending positions publish decimals:7 as if it had been read.
func TestExplorerHandler_TokenDecimalsNeverGuessesSeven(t *testing.T) {
	for _, c := range []struct {
		name   string
		reader TokenDecimalsReader
		want   int
		wantOK bool
	}{
		{"read 18", explorerDecStub{d: 18, found: true}, 18, true},
		{"not found", explorerDecStub{}, 0, false},
		{"read failed", explorerDecStub{err: errors.New("ch down")}, 0, false},
		{"no reader", nil, 0, false},
	} {
		s := &Server{Options: Options{TokenDecimals: c.reader}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		h := explorerHandlerFor(s, s.Options, s.logger)
		got, ok := h.TokenDecimals(context.Background(), "CTOKEN")
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("%s: TokenDecimals = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}
