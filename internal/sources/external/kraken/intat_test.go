package kraken

import (
	"testing"
)

// intAt on a positional krakenCandle returns ok=false when out of range or for non-numeric types, and parses float64 or digit
// strings.

func TestKrakenCandle_intAt_outOfRange(t *testing.T) {
	c := krakenCandle{float64(1_745_000_000)}
	if _, ok := c.intAt(7); ok {
		t.Error("intAt(out-of-range) returned ok, want false")
	}
}

func TestKrakenCandle_intAt_float64(t *testing.T) {
	c := krakenCandle{float64(1_745_000_000)}
	got, ok := c.intAt(0)
	if !ok {
		t.Fatal("intAt(float64) returned !ok")
	}
	if got != 1_745_000_000 {
		t.Errorf("got %d, want 1745000000", got)
	}
}

func TestKrakenCandle_intAt_stringNumeric(t *testing.T) {
	c := krakenCandle{"1745000000"}
	got, ok := c.intAt(0)
	if !ok {
		t.Fatal("intAt(string-of-digits) returned !ok")
	}
	if got != 1_745_000_000 {
		t.Errorf("got %d, want 1745000000", got)
	}
}

func TestKrakenCandle_intAt_stringNonNumericRejected(t *testing.T) {
	// A string that fails ParseInt must return ok=false rather than 0, since downstream uses ok to decide whether the row is
	// trustworthy.
	c := krakenCandle{"not-a-number"}
	if _, ok := c.intAt(0); ok {
		t.Error("intAt(\"not-a-number\") returned ok, want false")
	}
}

func TestKrakenCandle_intAt_unsupportedTypeRejected(t *testing.T) {
	// JSON's `true` decodes to bool; bool→int coercion would be
	// silently lossy. The function must return ok=false.
	c := krakenCandle{true}
	if _, ok := c.intAt(0); ok {
		t.Error("intAt(bool) returned ok, want false")
	}
}

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
