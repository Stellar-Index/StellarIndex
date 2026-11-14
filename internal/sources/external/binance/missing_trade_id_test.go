package binance

import (
	"errors"
	"testing"
)

// TestParseAggTradeFrame_MissingTradeID: an absent trade id must not parse as id 0, where every same-timestamp trade would share one row.
func TestParseAggTradeFrame_MissingTradeID(t *testing.T) {
	withID := []byte(`{"stream":"xlmusdt@aggTrade","data":{"e":"aggTrade","s":"XLMUSDT","a":0,"p":"0.17582","q":"152.34","T":1745000000100,"m":true}}`)
	if _, err := parseAggTradeFrame(withID, buildPairMap(t)); err != nil {
		t.Fatalf("an explicit id 0 was refused: %v", err)
	}
	renamed := []byte(`{"stream":"xlmusdt@aggTrade","data":{"e":"aggTrade","s":"XLMUSDT","agg_id":7,"p":"0.17582","q":"152.34","T":1745000000100,"m":true}}`)
	if _, err := parseAggTradeFrame(renamed, buildPairMap(t)); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("err = %v, want ErrMalformedFrame for a frame with no aggregate trade id", err)
	}
}
