package tiingo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

const testKey = "fixture-tiingo-token" // gitleaks:allow

var _ external.Poller = (*Poller)(nil)

// fixedNow puts the lookback start at 2026-09-22, the startDate the
// captured fixtures were fetched with.
func fixedNow() time.Time { return time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC) }

type fixtureServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
}

// newFixtureServer serves testdata/<ticker>_prices.json for each ticker
// in bodies, and the given status/body for any ticker mapped to a
// non-200 in failing.
func newFixtureServer(t *testing.T, failing map[string]string) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.requests = append(fs.requests, r.Clone(r.Context()))
		fs.mu.Unlock()
		ticker, ok := strings.CutPrefix(r.URL.Path, "/tiingo/daily/")
		ticker, ok2 := strings.CutSuffix(ticker, "/prices")
		if !ok || !ok2 {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Token "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if body, bad := failing[ticker]; bad {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(body))
			return
		}
		raw, err := os.ReadFile("testdata/" + strings.ToLower(ticker) + "_prices.json")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func newTestPoller(t *testing.T, endpoint string, tickers ...string) (*Poller, *bytes.Buffer) {
	t.Helper()
	p, err := NewPoller(testKey, tickers)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Endpoint = endpoint
	p.Now = fixedNow
	var logs bytes.Buffer
	p.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return p, &logs
}

func poll(t *testing.T, p *Poller) ([]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	trades, updates, err := p.PollOnce(ctx, nil)
	if len(trades) != 0 {
		t.Fatalf("tiingo emitted %d trades; it is oracle-only", len(trades))
	}
	var rows []string
	for _, u := range updates {
		if err := u.Validate(); err != nil {
			t.Fatalf("emitted invalid OracleUpdate %+v: %v", u, err)
		}
		rows = append(rows, u.Asset.String()+"|"+u.Quote.String()+"|"+
			u.Timestamp.Format(time.RFC3339)+"|"+u.Price.String())
	}
	return rows, err
}

func TestNewPoller_Refuses(t *testing.T) {
	if _, err := NewPoller("", []string{"WTTSX"}); !errors.Is(err, ErrAPIKeyRequired) {
		t.Errorf("empty key: got %v, want ErrAPIKeyRequired", err)
	}
	if _, err := NewPoller(testKey, nil); !errors.Is(err, ErrNoTickers) {
		t.Errorf("no tickers: got %v, want ErrNoTickers", err)
	}
}

func TestPollOnce_GoldenFixtures(t *testing.T) {
	srv := newFixtureServer(t, nil)
	p, _ := newTestPoller(t, srv.URL, "WTTSX", "FLTTX", "WTGXX")

	rows, err := poll(t, p)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	want := []string{
		"raw:WTTSX|fiat:USD|2026-09-22T00:00:00Z|9550000",
		"raw:WTTSX|fiat:USD|2026-09-23T00:00:00Z|9490000",
		"raw:WTTSX|fiat:USD|2026-09-24T00:00:00Z|9470000",
		"raw:WTTSX|fiat:USD|2026-09-25T00:00:00Z|9460000",
		"raw:WTTSX|fiat:USD|2026-09-28T00:00:00Z|9430000",
		"raw:WTTSX|fiat:USD|2026-09-29T00:00:00Z|9440000",
		"raw:FLTTX|fiat:USD|2026-09-22T00:00:00Z|1010000",
		"raw:FLTTX|fiat:USD|2026-09-23T00:00:00Z|1010000",
		"raw:FLTTX|fiat:USD|2026-09-24T00:00:00Z|1010000",
		"raw:FLTTX|fiat:USD|2026-09-25T00:00:00Z|1000000",
		"raw:FLTTX|fiat:USD|2026-09-28T00:00:00Z|1000000",
		"raw:FLTTX|fiat:USD|2026-09-29T00:00:00Z|1000000",
	}
	if strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
}

func TestPollOnce_RowShape(t *testing.T) {
	srv := newFixtureServer(t, nil)
	p, _ := newTestPoller(t, srv.URL, "WTTSX")
	_, updates, err := p.PollOnce(context.Background(), nil)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	for _, u := range updates {
		if u.Source != SourceName || u.Decimals != Decimals || u.Asset.IsMapped() {
			t.Errorf("row %+v: want source %q, decimals %d, unmapped raw asset", u, SourceName, Decimals)
		}
	}
}

func TestPollOnce_EmptyPriceArrayIsNoRowNoError(t *testing.T) {
	srv := newFixtureServer(t, nil)
	p, _ := newTestPoller(t, srv.URL, "WTGXX")
	trades, updates, err := p.PollOnce(context.Background(), nil)
	if err != nil || trades != nil || updates != nil {
		t.Fatalf("WTGXX: got (%v, %v, %v), want (nil, nil, nil)", trades, updates, err)
	}
}

func TestPollOnce_AuthIsHeaderOnly(t *testing.T) {
	srv := newFixtureServer(t, nil)
	p, _ := newTestPoller(t, srv.URL, "WTTSX", "FLTTX")
	if _, err := poll(t, p); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(srv.requests) != 2 {
		t.Fatalf("got %d requests, want one per ticker", len(srv.requests))
	}
	for _, r := range srv.requests {
		if got := r.Header.Get("Authorization"); got != "Token "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		if strings.Contains(r.URL.String(), testKey) {
			t.Errorf("key leaked into URL %q", r.URL.String())
		}
		if got := r.URL.RawQuery; got != "startDate=2026-09-22" {
			t.Errorf("query = %q, want only startDate", got)
		}
	}
}

func TestPollOnce_KeyNeverInErrorOrLog(t *testing.T) {
	echo := `{"detail":"Invalid token ` + testKey + `"}`
	srv := newFixtureServer(t, map[string]string{"FLTTX": echo, "WTTSX": echo})

	// Partial failure: the failure is logged, the good ticker's rows survive.
	p, logs := newTestPoller(t, srv.URL, "FLTTX", "WTGXX")
	if _, err := poll(t, p); err != nil {
		t.Fatalf("partial failure returned %v; want nil (logged)", err)
	}
	if !strings.Contains(logs.String(), "FLTTX") {
		t.Fatalf("partial failure not logged: %q", logs.String())
	}

	// Total failure: the joined error is returned.
	p2, logs2 := newTestPoller(t, srv.URL, "FLTTX", "WTTSX")
	_, err := poll(t, p2)
	if !errors.Is(err, ErrAPIRejected) {
		t.Fatalf("total failure: got %v, want ErrAPIRejected", err)
	}

	for name, s := range map[string]string{"log": logs.String() + logs2.String(), "error": err.Error()} {
		if strings.Contains(s, testKey) {
			t.Errorf("API key leaked into %s: %q", name, s)
		}
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("echoed key not redacted in %q", err.Error())
	}
}

func TestPollOnce_TransportErrorOmitsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	p, _ := newTestPoller(t, url, "WTTSX")
	_, err := poll(t, p)
	if err == nil {
		t.Fatal("closed server: want an error")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("API key leaked into transport error %q", err.Error())
	}
}

// A re-poll re-reads bars already stored; each must hit the same
// (source, tx_hash, op_index, ts) key so the insert is a no-op.
func TestPollOnce_RepollIsIdempotent(t *testing.T) {
	srv := newFixtureServer(t, nil)
	p, _ := newTestPoller(t, srv.URL, "WTTSX", "FLTTX")
	ctx := context.Background()
	_, first, err := p.PollOnce(ctx, nil)
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	p.Now = func() time.Time { return fixedNow().Add(3 * time.Hour) }
	_, second, err := p.PollOnce(ctx, nil)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("row counts differ: %d vs %d", len(first), len(second))
	}
	seen := map[string]bool{}
	for i := range first {
		a, b := first[i], second[i]
		if a.TxHash != b.TxHash || !a.Timestamp.Equal(b.Timestamp) || a.OpIndex != b.OpIndex || a.Ledger != b.Ledger {
			t.Errorf("row %d key moved between polls: %+v vs %+v", i, a, b)
		}
		key := a.TxHash + a.Timestamp.String()
		if seen[key] {
			t.Errorf("row %d collides with another (ticker, date): %s", i, key)
		}
		seen[key] = true
	}
}

func TestBarsToUpdates_MalformedAndMissing(t *testing.T) {
	rows, err := barsToUpdates("WTTSX", []priceBar{{Date: "2026-09-29T00:00:00.000Z"}})
	if err != nil || len(rows) != 0 {
		t.Errorf("bar without close: got (%v, %v), want no row, no error", rows, err)
	}
	if _, err := barsToUpdates("WTTSX", []priceBar{{Date: "29/09/2026", Close: "9.44"}}); !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("bad date: got %v, want ErrMalformedResponse", err)
	}
	if _, err := barsToUpdates("WTTSX", []priceBar{{Date: "2026-09-29T00:00:00.000Z", Close: "9.44e0"}}); !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("exponent close: got %v, want ErrMalformedResponse", err)
	}
}
