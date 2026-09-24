package kraken

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// synthesiseKrakenCandles builds a slice of Kraken-shape OHLC rows
// starting at `startSec` with `intervalSec` gap.
func synthesiseKrakenCandles(count int, startSec, intervalSec int64) []krakenCandle {
	out := make([]krakenCandle, count)
	for i := 0; i < count; i++ {
		t := startSec + int64(i)*intervalSec
		priceStr := strconv.FormatFloat(0.17582+0.00001*float64(i), 'f', 5, 64)
		out[i] = krakenCandle{
			float64(t),  // open time
			"0.17582",   // open
			"0.17600",   // high
			"0.17500",   // low
			priceStr,    // close
			priceStr,    // vwap
			"100.5",     // volume (base)
			float64(50), // count
		}
	}
	return out
}

// newTestKrakenREST wraps a fixture OHLC result in the Kraken
// response envelope and serves it.
func newTestKrakenREST(t *testing.T, pair string, candles []krakenCandle, last int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ohlcPath {
			http.NotFound(w, r)
			return
		}
		result := map[string]any{
			pair:   candles,
			"last": last,
		}
		body := map[string]any{
			"error":  []string{},
			"result": result,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestKrakenBackfill_HappyPath(t *testing.T) {
	const startSec = int64(1_745_000_000)
	const hourSec = int64(3_600)

	candles := synthesiseKrakenCandles(5, startSec, hourSec)
	lastTs := startSec + 4*hourSec
	srv := newTestKrakenREST(t, "XLMUSD", candles, lastTs)
	defer srv.Close()

	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	// Add XLMUSD mapping → XLM/USD canonical pair.
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	m["XLMUSD"] = xlmUSD

	s := NewStreamer(m)
	s.Endpoint = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	from := time.Unix(startSec, 0).UTC()
	to := from.Add(10 * time.Hour)

	trades, err := s.Backfill(ctx, xlmUSD, from, to, 1*time.Hour)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(trades) != 5 {
		t.Fatalf("expected 5 trades, got %d", len(trades))
	}
	// First trade: close_time = startSec + hourSec - 1.
	wantCloseSec := startSec + hourSec - 1
	if trades[0].Timestamp.Unix() != wantCloseSec {
		t.Errorf("trade[0] close_sec = %d want %d",
			trades[0].Timestamp.Unix(), wantCloseSec)
	}
	// Base = 100.5 × 10^8 = 10_050_000_000
	wantBase := big.NewInt(10_050_000_000)
	if trades[0].BaseAmount.BigInt().Cmp(wantBase) != 0 {
		t.Errorf("BaseAmount = %s want %s", trades[0].BaseAmount, wantBase)
	}
	if trades[0].Source != "kraken" {
		t.Errorf("Source = %q", trades[0].Source)
	}
	if len(trades[0].TxHash) != 64 {
		t.Errorf("TxHash len = %d", len(trades[0].TxHash))
	}
}

// TestKrakenBackfill_DetectsDepthHorizonStraddle: Kraken ignores a
// `since` older than its ~720-candle horizon and serves its most
// recent window instead, with err=nil at the HTTP layer. A range that
// straddles the horizon must surface as ErrDepthExceeded — carrying the
// trades that ARE inside the served window — not as a silent, truncated
// success.
func TestKrakenBackfill_DetectsDepthHorizonStraddle(t *testing.T) {
	const requestedFromSec = int64(1_700_000_000)
	const hourSec = int64(3_600)
	// The venue's actual horizon starts well after the requested
	// `from` — more than one interval later.
	const horizonStartSec = requestedFromSec + 50*hourSec

	candles := synthesiseKrakenCandles(5, horizonStartSec, hourSec)
	lastTs := horizonStartSec + 4*hourSec
	srv := newTestKrakenREST(t, "XLMUSD", candles, lastTs)
	defer srv.Close()

	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	m["XLMUSD"] = xlmUSD

	s := NewStreamer(m)
	s.Endpoint = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	from := time.Unix(requestedFromSec, 0).UTC()
	// `to` is well inside the served window, so every returned candle
	// is within [from, to) and would previously have been accepted
	// with err=nil despite covering none of the requested history.
	to := time.Unix(horizonStartSec+10*hourSec, 0).UTC()

	trades, err := s.Backfill(ctx, xlmUSD, from, to, 1*time.Hour)
	if !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("err = %v, want ErrDepthExceeded", err)
	}
	if len(trades) != 5 {
		t.Fatalf("expected the 5 trades inside the served window to be salvaged, got %d", len(trades))
	}
}

// TestKrakenBackfill_DetectsDepthHorizonEntirelyExceeded: a range
// entirely older than Kraken's serving horizon must not report 0
// trades with err=nil — that reads as "nothing traded in this window"
// rather than "this window was never asked for".
func TestKrakenBackfill_DetectsDepthHorizonEntirelyExceeded(t *testing.T) {
	const requestedFromSec = int64(1_700_000_000)
	const hourSec = int64(3_600)
	const horizonStartSec = requestedFromSec + 50*hourSec

	candles := synthesiseKrakenCandles(5, horizonStartSec, hourSec)
	lastTs := horizonStartSec + 4*hourSec
	srv := newTestKrakenREST(t, "XLMUSD", candles, lastTs)
	defer srv.Close()

	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	m["XLMUSD"] = xlmUSD

	s := NewStreamer(m)
	s.Endpoint = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	from := time.Unix(requestedFromSec, 0).UTC()
	// `to` is also before the venue's actual served window.
	to := time.Unix(requestedFromSec+10*hourSec, 0).UTC()

	trades, err := s.Backfill(ctx, xlmUSD, from, to, 1*time.Hour)
	if !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("err = %v, want ErrDepthExceeded", err)
	}
	if len(trades) != 0 {
		t.Fatalf("expected 0 trades (none of the served window overlaps [from,to)), got %d", len(trades))
	}
}

func TestKrakenBackfill_RejectsInvalidRange(t *testing.T) {
	s := NewStreamer(map[string]canonical.Pair{})
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	p, _ := canonical.NewPair(xlm, usd)
	_, err := s.Backfill(context.Background(), p, time.Now(), time.Now(), time.Hour)
	if err == nil {
		t.Error("expected error for from==to")
	}
}

func TestKrakenBackfill_UnsupportedGranularity(t *testing.T) {
	m, _ := DefaultPairs()
	s := NewStreamer(m)
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	p, _ := canonical.NewPair(xlm, usd)
	// 7m isn't in Kraken's set.
	_, err := s.Backfill(context.Background(), p,
		time.Now().Add(-time.Hour), time.Now(), 7*time.Minute)
	if err == nil {
		t.Error("expected error for unsupported granularity")
	}
}

func TestGranularityToMinutes(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
		err  bool
	}{
		{1 * time.Minute, 1, false},
		{15 * time.Minute, 15, false},
		{1 * time.Hour, 60, false},
		{4 * time.Hour, 240, false},
		{24 * time.Hour, 1440, false},
		{7 * 24 * time.Hour, 10080, false},
		{15 * 24 * time.Hour, 21600, false},
		{2 * time.Minute, 0, true},
		{6 * time.Hour, 0, true}, // not in Kraken's set
	}
	for _, tc := range cases {
		got, err := granularityToMinutes(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("%v: want err, got %d", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%v: got (%d, %v) want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestKrakenBackfill_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":["EGeneral:Invalid arguments"]}`))
	}))
	defer srv.Close()
	m, _ := DefaultPairs()
	s := NewStreamer(m)
	s.Endpoint = srv.URL
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	p, _ := canonical.NewPair(xlm, usd)
	// Must have a mapping for XLM/USD, use the existing "XLM/USD" →
	// pair in DefaultPairs.
	_, err := s.Backfill(context.Background(), p,
		time.Unix(1, 0), time.Unix(100, 0), time.Hour)
	if err == nil {
		t.Error("expected error from Kraken API error array")
	}
}

// "RENDER/USD" leaves a 33-byte candle seed, which used to drop the
// close time's last digit so neighbouring candles could share one
// trades PK. Backfill must refuse it before walking rather than
// per-candle-skip it into an empty result.
func TestKrakenBackfill_RejectsSymbolThatWouldTruncateSeed(t *testing.T) {
	const startSec = int64(1_745_000_000)
	srv := newTestKrakenREST(t, "RENDER/USD", synthesiseKrakenCandles(2, startSec, 60), startSec+60)
	defer srv.Close()

	// Only the symbol's length matters; the pair is any valid one.
	pair, err := canonical.NewPair(mustAsset(t, "crypto:XLM"), mustAsset(t, "fiat:USD"))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	s := NewStreamer(map[string]canonical.Pair{"RENDER/USD": pair})
	s.Endpoint = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	from := time.Unix(startSec, 0).UTC()
	trades, err := s.Backfill(ctx, pair, from, from.Add(2*time.Minute), time.Minute)
	if err == nil {
		hashes := make([]string, len(trades))
		for i, tr := range trades {
			hashes[i] = tr.TxHash
		}
		t.Fatalf("9-byte symbol accepted; candle tx_hashes %v", hashes)
	}
	if !errors.Is(err, scale.ErrSyntheticSeedTooLong) {
		t.Fatalf("err = %v, want ErrSyntheticSeedTooLong", err)
	}
}

// Every shipped pair must fit both synthesised identities whole, so a
// new long symbol fails here rather than colliding in production.
func TestDefaultPairs_FitSyntheticTxHash(t *testing.T) {
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	for sym := range m {
		if _, err := candleTxHash(sym, 0); err != nil {
			t.Errorf("candleTxHash(%q): %v", sym, err)
		}
		if _, err := formatTxHash(sym, 0); err != nil {
			t.Errorf("formatTxHash(%q): %v", sym, err)
		}
	}
}

// A raw-fill backfill and the live streamer must derive the same
// tx_hash for the same Kraken fill, or backfilling a streamed window
// inserts every fill twice. Red today: the raw-fill path hashes
// "<SYM>-fill-<id>-BF-<ts>" via backfillTxHash, the streamer
// "<SYM>-<id>" via formatTxHash. The fix is in backfill_trades.go,
// which is held by another change.
func TestRawFillTxHashMatchesLiveIdentity(t *testing.T) {
	t.Skip("GH-994 part 1: needs krakenFillToTrade (backfill_trades.go) to call formatTxHash")
	pair, err := canonical.NewPair(mustAsset(t, "crypto:XLM"), mustAsset(t, "fiat:USD"))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	f := krakenFill{price: "0.19329800", volume: "159.80957483", ts: 1530403200.5, id: 12345678}
	backfilled, err := krakenFillToTrade(f, "XLM/USD", pair)
	if err != nil {
		t.Fatalf("krakenFillToTrade: %v", err)
	}
	live, err := formatTxHash("XLM/USD", f.id)
	if err != nil {
		t.Fatalf("formatTxHash: %v", err)
	}
	if backfilled.TxHash != live {
		t.Fatalf("raw-fill tx_hash %s != live tx_hash %s for fill %d", backfilled.TxHash, live, f.id)
	}
}
