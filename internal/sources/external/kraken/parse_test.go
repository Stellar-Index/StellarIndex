package kraken

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

func buildPairMap(t *testing.T) map[string]canonical.Pair {
	t.Helper()
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	return m
}

func TestParseFrame_TradeUpdate_HappyPath(t *testing.T) {
	// Real-shape v2 trade update — XLM/USD at 0.17582 for 100 XLM.
	raw := []byte(`{
      "channel": "trade",
      "type": "update",
      "data": [
        {
          "symbol": "XLM/USD",
          "side": "buy",
          "qty": 100.00000000,
          "price": 0.17582,
          "ord_type": "market",
          "trade_id": 987654321,
          "timestamp": "2026-04-24T12:34:56.789000Z"
        }
      ]
    }`)
	res, err := parseFrame(raw, buildPairMap(t))
	trades := res.Trades
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(trades))
	}
	tr := trades[0]
	if tr.Source != "kraken" {
		t.Errorf("Source = %q want kraken", tr.Source)
	}
	if tr.Timestamp.Format(time.RFC3339) != "2026-04-24T12:34:56Z" {
		t.Errorf("Timestamp = %v (RFC3339: %q)", tr.Timestamp, tr.Timestamp.Format(time.RFC3339))
	}
	// Base at 10^8 = 100 × 10^8 = 10000000000
	wantBase := big.NewInt(10_000_000_000)
	if tr.BaseAmount.BigInt().Cmp(wantBase) != 0 {
		t.Errorf("BaseAmount = %s want %s", tr.BaseAmount, wantBase)
	}
	// Quote = 0.17582 × 100 = 17.582 → 10^8 = 1,758,200,000
	wantQuote := big.NewInt(1_758_200_000)
	if tr.QuoteAmount.BigInt().Cmp(wantQuote) != 0 {
		t.Errorf("QuoteAmount = %s want %s", tr.QuoteAmount, wantQuote)
	}
	// TxHash must be 64 hex chars.
	if len(tr.TxHash) != 64 {
		t.Errorf("TxHash len = %d want 64", len(tr.TxHash))
	}
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	if !tr.Pair.Base.Equal(xlm) || !tr.Pair.Quote.Equal(usd) {
		t.Errorf("Pair = %+v want XLM/USD", tr.Pair)
	}
}

func TestParseFrame_TradeSnapshot_MultipleEntries(t *testing.T) {
	// Snapshot delivers several trades at once on subscribe.
	raw := []byte(`{
      "channel": "trade",
      "type": "snapshot",
      "data": [
        {"symbol":"XLM/USD","side":"buy","qty":50,"price":0.17570,"ord_type":"market","trade_id":1,"timestamp":"2026-04-24T00:00:00Z"},
        {"symbol":"XLM/USD","side":"sell","qty":75,"price":0.17572,"ord_type":"limit","trade_id":2,"timestamp":"2026-04-24T00:00:01Z"}
      ]
    }`)
	res, err := parseFrame(raw, buildPairMap(t))
	trades := res.Trades
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if len(trades) != 2 {
		t.Fatalf("want 2 trades from snapshot, got %d", len(trades))
	}
	if trades[0].TxHash == trades[1].TxHash {
		t.Error("snapshot trades collided on tx_hash")
	}
}

func TestParseFrame_HeartbeatIgnored(t *testing.T) {
	// Kraken sends periodic heartbeats — parseFrame must not
	// treat these as errors.
	raw := []byte(`{"channel":"heartbeat"}`)
	res, err := parseFrame(raw, buildPairMap(t))
	trades := res.Trades
	if err != nil {
		t.Errorf("heartbeat should return nil err, got %v", err)
	}
	if trades != nil {
		t.Errorf("heartbeat should return nil trades, got %v", trades)
	}
}

func TestParseFrame_StatusIgnored(t *testing.T) {
	raw := []byte(`{"channel":"status","data":[{"system":"online","api_version":"v2"}]}`)
	res, err := parseFrame(raw, buildPairMap(t))
	trades := res.Trades
	if err != nil {
		t.Errorf("status should return nil err, got %v", err)
	}
	if trades != nil {
		t.Errorf("status should return nil trades, got %v", trades)
	}
}

func TestParseFrame_SubscribeAckAccepted(t *testing.T) {
	// After our subscribe request, Kraken replies per symbol with:
	//   {"method":"subscribe","result":{...},"success":true,"time_in":"...","time_out":"..."}
	// No channel field: no trades, but the verdict is reported.
	raw := []byte(`{"method":"subscribe","success":true,"result":{"channel":"trade","symbol":"XLM/USD"}}`)
	res, err := parseFrame(raw, buildPairMap(t))
	if err != nil {
		t.Fatalf("subscribe-ack should return nil err, got %v", err)
	}
	if res.Trades != nil {
		t.Errorf("subscribe-ack should return nil trades, got %v", res.Trades)
	}
	want := subscribeAck{Symbol: "XLM/USD", Accepted: true}
	if res.Ack == nil || *res.Ack != want {
		t.Errorf("Ack = %+v, want %+v", res.Ack, want)
	}
}

// TestParseFrame_SubscribeRejectedReported pins GH-995 (2): a
// success:false ack is the venue saying the pair does not exist, and
// must reach the streamer with the venue's own error text.
func TestParseFrame_SubscribeRejectedReported(t *testing.T) {
	raw := []byte(`{"error":"Currency pair not supported XLM/GBP","method":"subscribe","success":false,` +
		`"symbol":"XLM/GBP","time_in":"2026-09-24T00:00:00.000000Z","time_out":"2026-09-24T00:00:00.000100Z"}`)
	res, err := parseFrame(raw, buildPairMap(t))
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	want := subscribeAck{Symbol: "XLM/GBP", Accepted: false, Message: "Currency pair not supported XLM/GBP"}
	if res.Ack == nil || *res.Ack != want {
		t.Fatalf("Ack = %+v, want %+v", res.Ack, want)
	}
	if len(res.Trades) != 0 || len(res.Skips) != 0 {
		t.Errorf("rejection carried trades/skips: %+v", res)
	}
}

// TestParseFrame_SkippedEntriesReported pins GH-995 (1): every entry
// buildTrade refuses inside a well-formed frame is reported with its
// reason, the good entries still come through, and dust is not a skip.
func TestParseFrame_SkippedEntriesReported(t *testing.T) {
	raw := []byte(`{"channel":"trade","type":"update","data":[
      {"symbol":"XLM/USD","qty":10,"price":0.175,"trade_id":1,"timestamp":"2026-04-24T00:00:01Z"},
      {"symbol":"XLMUSD","qty":10,"price":0.175,"trade_id":2,"timestamp":"2026-04-24T00:00:01Z"},
      {"symbol":"XLM/USD","qty":1e3,"price":0.175,"trade_id":3,"timestamp":"2026-04-24T00:00:01Z"},
      {"symbol":"XLM/USD","qty":10,"price":1.75e-1,"trade_id":4,"timestamp":"2026-04-24T00:00:01Z"},
      {"symbol":"XLM/USD","qty":10,"price":0.175,"trade_id":5,"timestamp":"1714000000"},
      {"symbol":"XLM/USD","qty":0.00000001,"price":0.16,"trade_id":6,"timestamp":"2026-04-24T00:00:01Z"}
    ]}`)
	res, err := parseFrame(raw, buildPairMap(t))
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("want 1 trade, got %d", len(res.Trades))
	}
	got := make([]string, 0, len(res.Skips))
	for _, sk := range res.Skips {
		got = append(got, sk.Reason)
		if sk.Err == nil {
			t.Errorf("skip %q carries no error", sk.Reason)
		}
	}
	want := []string{skipUnknownSymbol, skipBadQty, skipBadPrice, skipBadTimestamp}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("skip reasons = %v, want %v", got, want)
	}
}

// TestSkipReasons_MatchObsEnum keeps the parser's reason labels and the
// metric's documented label set in lockstep.
func TestSkipReasons_MatchObsEnum(t *testing.T) {
	if strings.Join(skipReasons, ",") != strings.Join(obs.CEXStreamEntrySkipReasons, ",") {
		t.Errorf("kraken skip reasons %v != obs.CEXStreamEntrySkipReasons %v",
			skipReasons, obs.CEXStreamEntrySkipReasons)
	}
}

func TestParseFrame_UnknownSymbolSkipped(t *testing.T) {
	// A trade on MATIC/USD (in the ADR-0014 allow-list, but not in
	// DefaultPairs — see binance/start_errors_test.go for rationale)
	// mixed with one on XLM/USD: unknown entry is dropped, known
	// one lands.
	raw := []byte(`{
      "channel": "trade",
      "type": "update",
      "data": [
        {"symbol":"MATIC/USD","side":"buy","qty":5,"price":0.5,"ord_type":"market","trade_id":100,"timestamp":"2026-04-24T00:00:00Z"},
        {"symbol":"XLM/USD","side":"buy","qty":10,"price":0.175,"ord_type":"market","trade_id":101,"timestamp":"2026-04-24T00:00:01Z"}
      ]
    }`)
	res, err := parseFrame(raw, buildPairMap(t))
	trades := res.Trades
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("want 1 trade (XLM/USD only), got %d", len(trades))
	}
	xlm, _ := canonical.NewCryptoAsset("XLM")
	if !trades[0].Pair.Base.Equal(xlm) {
		t.Errorf("got base %+v want XLM", trades[0].Pair.Base)
	}
}

func TestParseFrame_MalformedJSON(t *testing.T) {
	_, err := parseFrame([]byte(`{bad json`), buildPairMap(t))
	if !errors.Is(err, ErrMalformedFrame) {
		t.Errorf("expected ErrMalformedFrame, got %v", err)
	}
}

func TestBuildTrade_UnknownSymbol(t *testing.T) {
	// Direct buildTrade call with an unknown symbol yields
	// ErrUnknownSymbol — the frame-level helper swallows it, but
	// the per-entry error path is its own test.
	_, err := buildTrade(tradePayload{
		Symbol:    "FAKE/USD",
		Qty:       "1",
		Price:     "1",
		TradeID:   1,
		Timestamp: "2026-04-24T00:00:00Z",
	}, buildPairMap(t))
	if !errors.Is(err, ErrUnknownSymbol) {
		t.Errorf("expected ErrUnknownSymbol, got %v", err)
	}
}

// TestBuildTrade_DustReturnsTypedSentinel pins the regression for
// the bitstamp/coinbase/binance dust-trade family extended to
// Kraken. 1e-8 XLM at $0.16 → base × price floor-
// divides to 0 at 10^8 integer scale → canonical validator would
// reject with "quote_amount must be positive, got 0" → indexer
// logs ERROR per frame. With ErrDustTrade the streamer's error-
// skip branch silently absorbs it.
func TestBuildTrade_DustReturnsTypedSentinel(t *testing.T) {
	_, err := buildTrade(tradePayload{
		Symbol:    "XLM/USD",
		Qty:       "0.00000001",
		Price:     "0.16000000",
		TradeID:   1,
		Timestamp: "2026-04-24T00:00:00Z",
	}, buildPairMap(t))
	if !errors.Is(err, ErrDustTrade) {
		t.Errorf("expected ErrDustTrade, got %v", err)
	}
}

func TestDecimalStringToScaledInt_KrakenPrecision(t *testing.T) {
	// Kraken publishes at most 8 decimal places for prices, often
	// fewer. Verify the scaling matches what Binance produces for
	// equivalent numbers — aggregator sums across sources should
	// be integer-safe.
	cases := []struct {
		in   string
		want *big.Int
	}{
		{"0.17582", big.NewInt(17_582_000)},
		{"100", big.NewInt(10_000_000_000)},
		{"0.00001", big.NewInt(1000)},
		{"12345.12345678", big.NewInt(1234512345678)},
	}
	for _, tc := range cases {
		got, err := scale.DecimalStringToScaledInt(tc.in, externalAmountDecimals)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		if got.Cmp(tc.want) != 0 {
			t.Errorf("%q → %s, want %s", tc.in, got, tc.want)
		}
	}
}

// A 12-byte symbol pushes the live seed one byte past the hash, which
// used to drop trade_id's last digit so ten consecutive fills shared
// one trades PK. It must be refused.
func TestBuildTrade_RejectsSymbolThatWouldTruncateSeed(t *testing.T) {
	// Only the symbol's length matters; the pair is any valid one.
	pm := map[string]canonical.Pair{"FARTCOIN/USDT": buildPairMap(t)["XLM/USD"]}
	fill := func(id int64) tradePayload {
		return tradePayload{Symbol: "FARTCOIN/USDT", Qty: "10", Price: "1", TradeID: id, Timestamp: "2026-04-24T00:00:00Z"}
	}
	a, errA := buildTrade(fill(987654321), pm)
	b, errB := buildTrade(fill(987654322), pm)
	if errA == nil || errB == nil {
		t.Fatalf("12-byte symbol accepted: trade_ids 987654321/987654322 -> tx_hash %s / %s (identical=%v)",
			a.TxHash, b.TxHash, a.TxHash == b.TxHash)
	}
	if !errors.Is(errA, scale.ErrSyntheticSeedTooLong) {
		t.Fatalf("err = %v, want ErrSyntheticSeedTooLong", errA)
	}
}

func TestFormatTxHash_SymbolNormalised(t *testing.T) {
	// formatTxHash strips "/" so "XLM/USD" and "XLMUSD" (future
	// alias) produce the same hash for the same trade_id —
	// prevents dedupe failure on venue symbol renames.
	a, errA := formatTxHash("XLM/USD", 42)
	b, errB := formatTxHash("XLMUSD", 42)
	if errA != nil || errB != nil {
		t.Fatalf("formatTxHash: %v / %v", errA, errB)
	}
	if a != b {
		t.Errorf("slash-normalised formatTxHash differs: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("len = %d want 64", len(a))
	}
	// Lowercase check for a couple of leading chars — we build hex
	// manually so "XLM" → 58, 4C, 4D — uppercase A-F avoided by %02x.
	if strings.ToLower(a) != a {
		t.Errorf("formatTxHash not lowercase: %s", a)
	}
}
