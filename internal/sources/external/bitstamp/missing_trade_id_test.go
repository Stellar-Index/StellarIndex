package bitstamp

import (
	"errors"
	"testing"
)

// TestParseFrame_MissingTradeID: an absent trade id must not parse as id 0, where every same-timestamp trade would share one row.
func TestParseFrame_MissingTradeID(t *testing.T) {
	raw := []byte(`{"event":"trade","channel":"live_trades_xlmusd","data":{"trade_id":7,"timestamp":"1745000000","microtimestamp":"1745000000123456","amount_str":"100.5","price_str":"0.17582","type":0}}`)
	if _, _, err := parseFrame(raw, mustPairs(t)); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("err = %v, want ErrMalformedFrame for a trade with no id", err)
	}
}
