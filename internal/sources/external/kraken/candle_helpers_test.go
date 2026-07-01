package kraken

import (
	"testing"
)

// closeStr extracts position 4 of the krakenCandle positional array
// as a string. Kraken's OHLC payload is positional, not keyed, so a
// shape change in their API (extra prefix field, type swap to
// number) would silently mis-attribute the close — pin both the
// happy and the missing-position branch.

func TestKrakenCandle_closeStr(t *testing.T) {
	c := krakenCandle{
		float64(1_745_000_000),
		"0.17582",
		"0.17600",
		"0.17500",
		"0.17590", // close
		"0.17585", // vwap
		"100.5",
		float64(50),
	}
	got, ok := c.closeStr()
	if !ok {
		t.Fatal("closeStr returned !ok on a well-formed candle")
	}
	if got != "0.17590" {
		t.Errorf("closeStr = %q, want \"0.17590\"", got)
	}
}

func TestKrakenCandle_closeStr_shortRow(t *testing.T) {
	short := krakenCandle{float64(1), "0.1", "0.2", "0.3"} // no index 4
	if _, ok := short.closeStr(); ok {
		t.Error("closeStr on short row returned ok, want false")
	}
}
