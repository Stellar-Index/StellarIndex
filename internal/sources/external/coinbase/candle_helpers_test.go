package coinbase

import (
	"encoding/json"
	"testing"
)

// coinbaseCandle is "[time, low, high, open, close, volume]" per
// the Coinbase /products/<id>/candles JSON shape. The accessors
// intAt / closeStr / volumeStr handle the type-uncertainty
// quirks of upstream sometimes serialising numbers as strings vs
// JSON numbers.

func TestCoinbaseCandle_intAt(t *testing.T) {
	cases := []struct {
		name string
		row  coinbaseCandle
		idx  int
		want int64
		ok   bool
	}{
		{"float64 in range", coinbaseCandle{1.7e9}, 0, 1_700_000_000, true},
		{"string parses", coinbaseCandle{"42"}, 0, 42, true},
		{"json.Number parses", coinbaseCandle{json.Number("1700000000")}, 0, 1_700_000_000, true},
		{"string parse failure", coinbaseCandle{"not-a-number"}, 0, 0, false},
		{"unsupported type", coinbaseCandle{true}, 0, 0, false},
		{"index out of range", coinbaseCandle{1.0}, 5, 0, false},
		{"empty row", coinbaseCandle{}, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.row.intAt(tc.idx)
			if got != tc.want || ok != tc.ok {
				t.Errorf("intAt(%d) = (%d, %v), want (%d, %v)", tc.idx, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestCoinbaseCandle_closeStr(t *testing.T) {
	t.Run("json.Number close", func(t *testing.T) {
		row := coinbaseCandle{json.Number("1700000000"), json.Number("0.10"), json.Number("0.20"), json.Number("0.11"), json.Number("0.18"), json.Number("12345.67")}
		got, ok := row.closeStr()
		if !ok || got != "0.18" {
			t.Errorf("closeStr() = (%q, %v), want (\"0.18\", true)", got, ok)
		}
	})
	t.Run("short row", func(t *testing.T) {
		row := coinbaseCandle{json.Number("1700000000"), json.Number("0.10")}
		if _, ok := row.closeStr(); ok {
			t.Error("closeStr() ok = true, want false (row too short)")
		}
	})
	t.Run("float64 close refused", func(t *testing.T) {
		row := coinbaseCandle{1.7e9, 0.10, 0.20, 0.11, 0.18, 12345.67}
		if _, ok := row.closeStr(); ok {
			t.Error("closeStr() ok = true, want false (float64 has already lost precision)")
		}
	})
}

func TestCoinbaseCandle_volumeStr(t *testing.T) {
	t.Run("json.Number volume", func(t *testing.T) {
		row := coinbaseCandle{json.Number("1700000000"), json.Number("0.10"), json.Number("0.20"), json.Number("0.11"), json.Number("0.18"), json.Number("12345.67")}
		got, ok := row.volumeStr()
		if !ok || got != "12345.67" {
			t.Errorf("volumeStr() = (%q, %v), want (\"12345.67\", true)", got, ok)
		}
	})
	t.Run("short row", func(t *testing.T) {
		row := coinbaseCandle{json.Number("1700000000"), json.Number("0.10"), json.Number("0.20"), json.Number("0.11"), json.Number("0.18")}
		if _, ok := row.volumeStr(); ok {
			t.Error("volumeStr() ok = true, want false (row too short)")
		}
	})
	t.Run("float64 volume refused", func(t *testing.T) {
		row := coinbaseCandle{1.7e9, 0.10, 0.20, 0.11, 0.18, 12345.67}
		if _, ok := row.volumeStr(); ok {
			t.Error("volumeStr() ok = true, want false (float64 has already lost precision)")
		}
	})
}

func TestCoinbaseCandle_openTimeSec(t *testing.T) {
	row := coinbaseCandle{1_770_000_000.0, 0.10, 0.20, 0.11, 0.18, 12345.67}
	got, ok := row.openTimeSec()
	if !ok || got != 1_770_000_000 {
		t.Errorf("openTimeSec() = (%v, %v), want (1770000000, true)", got, ok)
	}
}
