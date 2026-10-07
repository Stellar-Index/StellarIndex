package coinbase

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// coinbaseCandleToTrade has three early-exit error branches the
// existing backfill_test.go's happy-path TestCoinbaseCandleToTrade_LHOC_Ordering
// doesn't reach. They guard against malformed upstream rows
// landing in the trades hypertable as zero-volume or zero-price
// observations.

func makePair(t *testing.T) canonical.Pair {
	t.Helper()
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset XLM: %v", err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset USD: %v", err)
	}
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

func TestCoinbaseCandleToTrade_missingTimeRejected(t *testing.T) {
	// First slot is the open-time epoch; an empty slice fails
	// openTimeSec.
	row := coinbaseCandle{}
	_, err := coinbaseCandleToTrade(row, "XLM-USD", makePair(t), 3600)
	if err == nil {
		t.Error("expected \"missing time\" error, got nil")
	}
	if !strings.Contains(err.Error(), "time") {
		t.Errorf("error %q missing \"time\" fragment", err.Error())
	}
}

func TestCoinbaseCandleToTrade_zeroVolumeRejected(t *testing.T) {
	// Volume=0 is treated as missing — it would translate into a
	// zero-amount Trade that breaks downstream VWAP weighting.
	row := coinbaseCandle{
		json.Number("1700000000"), // time
		json.Number("0.17500"),    // low
		json.Number("0.17600"),    // high
		json.Number("0.17582"),    // open
		json.Number("0.17582"),    // close
		json.Number("0.0"),        // volume = 0 → reject
	}
	_, err := coinbaseCandleToTrade(row, "XLM-USD", makePair(t), 3600)
	if err == nil {
		t.Error("expected \"zero volume\" error, got nil")
	}
	if !strings.Contains(err.Error(), "volume") {
		t.Errorf("error %q missing \"volume\" fragment", err.Error())
	}
}

func TestCoinbaseCandleToTrade_missingVolumeRejected(t *testing.T) {
	// volume slot is the wrong type — volumeFloat returns ok=false.
	row := coinbaseCandle{
		json.Number("1700000000"), json.Number("0.17500"), json.Number("0.17600"), json.Number("0.17582"), json.Number("0.17582"),
		"100.0", // string, not a JSON number — volumeStr rejects
	}
	_, err := coinbaseCandleToTrade(row, "XLM-USD", makePair(t), 3600)
	if err == nil {
		t.Error("expected error for non-number volume, got nil")
	}
}

func TestCoinbaseCandleToTrade_zeroCloseRejected(t *testing.T) {
	// Close=0 is treated as missing — would yield a zero-quote-
	// amount Trade that downstream callers would mistake for a
	// free trade.
	row := coinbaseCandle{
		json.Number("1700000000"), json.Number("0.17500"), json.Number("0.17600"), json.Number("0.17582"),
		json.Number("0.0"),   // close = 0 → reject
		json.Number("100.0"), // volume
	}
	_, err := coinbaseCandleToTrade(row, "XLM-USD", makePair(t), 3600)
	if err == nil {
		t.Error("expected \"zero close\" error, got nil")
	}
	if !strings.Contains(err.Error(), "close") {
		t.Errorf("error %q missing \"close\" fragment", err.Error())
	}
}

func TestCoinbaseCandleToTrade_missingCloseRejected(t *testing.T) {
	row := coinbaseCandle{
		json.Number("1700000000"), json.Number("0.17500"), json.Number("0.17600"), json.Number("0.17582"),
		"0.18", // string, not a JSON number
		json.Number("100.0"),
	}
	_, err := coinbaseCandleToTrade(row, "XLM-USD", makePair(t), 3600)
	if err == nil {
		t.Error("expected error for non-number close, got nil")
	}
}
