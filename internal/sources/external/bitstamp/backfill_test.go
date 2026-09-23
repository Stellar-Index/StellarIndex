package bitstamp

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func synthesiseBitstampCandles(count int, startSec, stepSec int64) []bitstampCandle {
	out := make([]bitstampCandle, count)
	for i := 0; i < count; i++ {
		ts := startSec + int64(i)*stepSec
		closePrice := strconv.FormatFloat(0.17582+0.00001*float64(i), 'f', 5, 64)
		out[i] = bitstampCandle{
			Timestamp: strconv.FormatInt(ts, 10),
			Open:      "0.17582",
			High:      "0.17600",
			Low:       "0.17500",
			Close:     closePrice,
			Volume:    "250",
		}
	}
	return out
}

func newTestBitstampREST(t *testing.T, pair string, candles []bitstampCandle) *httptest.Server {
	t.Helper()
	expectedPath := fmt.Sprintf(ohlcPathTemplate, pair)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v2/ohlc/") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != expectedPath {
			t.Errorf("path = %q want %q", r.URL.Path, expectedPath)
		}
		body := ohlcResponse{}
		body.Data.Pair = pair
		body.Data.OHLC = candles
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestBitstampBackfill_HappyPath(t *testing.T) {
	const startSec = int64(1_745_000_000)
	const hourSec = int64(3_600)

	candles := synthesiseBitstampCandles(5, startSec, hourSec)
	srv := newTestBitstampREST(t, "xlmusd", candles)
	defer srv.Close()

	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(m)
	s.Endpoint = srv.URL

	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	pair, _ := canonical.NewPair(xlm, usd)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	trades, err := s.Backfill(ctx, pair,
		time.Unix(startSec, 0).UTC(),
		time.Unix(startSec+10*hourSec, 0).UTC(),
		1*time.Hour)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(trades) != 5 {
		t.Fatalf("expected 5 trades, got %d", len(trades))
	}
	// First trade: close = open + step - 1
	wantCloseSec := startSec + hourSec - 1
	if trades[0].Timestamp.Unix() != wantCloseSec {
		t.Errorf("close ts = %d want %d", trades[0].Timestamp.Unix(), wantCloseSec)
	}
	// Base volume = 250 × 10^8 = 25_000_000_000
	wantBase := big.NewInt(25_000_000_000)
	if trades[0].BaseAmount.BigInt().Cmp(wantBase) != 0 {
		t.Errorf("BaseAmount = %s want %s", trades[0].BaseAmount, wantBase)
	}
	if trades[0].Source != "bitstamp" {
		t.Errorf("Source = %q", trades[0].Source)
	}
}

func TestBitstampBackfill_UnsupportedGranularity(t *testing.T) {
	m, _ := DefaultPairs()
	s := NewStreamer(m)
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	p, _ := canonical.NewPair(xlm, usd)
	_, err := s.Backfill(context.Background(), p,
		time.Unix(1, 0), time.Unix(1000, 0), 45*time.Minute)
	if err == nil {
		t.Error("expected error for 45m (not in Bitstamp step set)")
	}
}

func TestBitstampGranularity(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
		err  bool
	}{
		{60 * time.Second, 60, false},
		{1 * time.Hour, 3600, false},
		{24 * time.Hour, 86400, false},
		{3 * 24 * time.Hour, 259200, false},
		{2 * time.Hour, 7200, false},
		{90 * time.Second, 0, true},
	}
	for _, tc := range cases {
		got, err := granularityToSeconds(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("%v: want err", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%v: (%d, %v) want %d", tc.in, got, err, tc.want)
		}
	}
}

// newVendorFaithfulOHLC emulates Bitstamp's documented OHLC paging:
// when both `start` and `end` are sent, `end` wins and the `limit`
// candles at or before it are returned; with `start` alone, the
// `limit` candles from `start` onward are returned.
func newVendorFaithfulOHLC(t *testing.T, series []bitstampCandle) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit <= 0 || limit > bitstampMaxLimit {
			t.Errorf("bad limit %q", q.Get("limit"))
			http.Error(w, "bad limit", http.StatusBadRequest)
			return
		}
		var page []bitstampCandle
		if endStr := q.Get("end"); endStr != "" {
			end, _ := strconv.ParseInt(endStr, 10, 64)
			hi := 0
			for hi < len(series) {
				ts, _ := strconv.ParseInt(series[hi].Timestamp, 10, 64)
				if ts > end {
					break
				}
				hi++
			}
			page = series[max(0, hi-limit):hi]
		} else {
			start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
			lo := 0
			for lo < len(series) {
				ts, _ := strconv.ParseInt(series[lo].Timestamp, 10, 64)
				if ts >= start {
					break
				}
				lo++
			}
			page = series[lo:min(len(series), lo+limit)]
		}
		body := ohlcResponse{}
		body.Data.Pair = "XLM/USD"
		body.Data.OHLC = page
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

// A range longer than one page must be walked in full, not collapse to
// the last 1000 candles before `to` (the venue honours `end` over `start`).
func TestBitstampBackfill_PaginatesPastOnePage(t *testing.T) {
	const startSec = int64(1_577_836_800) // 2020-01-01T00:00:00Z
	const hourSec = int64(3_600)
	const want = 2_500
	// The venue holds candles beyond `to` too; they must not leak in.
	srv := newVendorFaithfulOHLC(t, synthesiseBitstampCandles(want+300, startSec, hourSec))
	defer srv.Close()

	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(m)
	s.Endpoint = srv.URL
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	pair, _ := canonical.NewPair(xlm, usd)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	trades, err := s.Backfill(ctx, pair,
		time.Unix(startSec, 0).UTC(),
		time.Unix(startSec+want*hourSec, 0).UTC(),
		time.Hour)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(trades) != want {
		t.Fatalf("got %d trades, want %d (one per hourly candle in [from, to))", len(trades), want)
	}
	for i, tr := range trades {
		wantClose := startSec + int64(i)*hourSec + hourSec - 1
		if got := tr.Timestamp.Unix(); got != wantClose {
			t.Fatalf("trade %d close ts = %d want %d", i, got, wantClose)
		}
	}
}

// A venue that ignores the `start` cursor must fail the backfill, not
// end it early with a truncated result and a nil error.
func TestBitstampBackfill_CursorIgnoredFailsClosed(t *testing.T) {
	const startSec = int64(1_577_836_800)
	const hourSec = int64(3_600)
	srv := newTestBitstampREST(t, "xlmusd", synthesiseBitstampCandles(bitstampMaxLimit, startSec, hourSec))
	defer srv.Close()

	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(m)
	s.Endpoint = srv.URL
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	pair, _ := canonical.NewPair(xlm, usd)

	trades, err := s.Backfill(context.Background(), pair,
		time.Unix(startSec, 0).UTC(),
		time.Unix(startSec+2*bitstampMaxLimit*hourSec, 0).UTC(),
		time.Hour)
	if err == nil || !strings.Contains(err.Error(), "cursor not honoured") {
		t.Fatalf("Backfill err = %v (%d trades), want cursor-not-honoured error", err, len(trades))
	}
}
