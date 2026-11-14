package coinbase

import (
	"errors"
	"testing"
)

// TestParseFrame_MissingTradeID: an absent trade id must not parse as id 0, where every same-timestamp trade would share one row.
func TestParseFrame_MissingTradeID(t *testing.T) {
	raw := []byte(`{"type":"match","id":7,"side":"buy","size":"10","price":"0.5","product_id":"XLM-USD","sequence":1,"time":"2026-04-24T00:00:00Z"}`)
	if _, _, err := parseFrame(raw, mustPairs(t)); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("err = %v, want ErrMalformedFrame for a match with no trade_id", err)
	}
}
