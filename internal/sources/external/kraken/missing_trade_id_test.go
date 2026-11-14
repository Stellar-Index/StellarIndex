package kraken

import (
	"errors"
	"testing"
)

// TestParseFrame_MissingTradeID: an absent trade id must not parse as id 0, where every same-timestamp trade would share one row.
func TestParseFrame_MissingTradeID(t *testing.T) {
	raw := []byte(`{"channel":"trade","type":"update","data":[{"symbol":"XLM/USD","side":"buy","qty":100,"price":0.17582,"id":7,"timestamp":"2026-04-24T12:34:56.789000Z"}]}`)
	res, err := parseFrame(raw, buildPairMap(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Trades) != 0 || len(res.Skips) != 1 {
		t.Fatalf("trades=%d skips=%d, want the entry skipped", len(res.Trades), len(res.Skips))
	}
	if !errors.Is(res.Skips[0].Err, ErrMalformedFrame) {
		t.Errorf("skip err = %v, want ErrMalformedFrame", res.Skips[0].Err)
	}
}
