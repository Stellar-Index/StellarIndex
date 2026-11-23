package coingecko

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Synthetic fixture in the documented market_chart/range shape; the values are invented, chosen
// to sit on the window edges and to need more precision than float64 carries.
const historyFixture = `{
  "prices": [
    [1763222399999, 0.25],
    [1763222400000, 98765432109876.123456789],
    [1763226000123, 8.123456789e-06],
    [1763233199999, 0.000000005],
    [1763233200000, 0.31]
  ],
  "market_caps": [[1763222400000, 1]],
  "total_volumes": [[1763222400000, 1]]
}`

var (
	histFrom = time.Date(2025, 11, 15, 16, 0, 0, 0, time.UTC) // 1763222400
	histTo   = time.Date(2025, 11, 15, 19, 0, 0, 0, time.UTC) // 1763233200
)

func xlmUSDPair(t *testing.T) canonical.Pair {
	t.Helper()
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pr, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func noCallGap(t *testing.T) {
	t.Helper()
	prev := historyCallGap
	historyCallGap = 0
	t.Cleanup(func() { historyCallGap = prev })
}

func TestDecimalToScaled_ExactHalfUp(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.1", "10000000"},
		// float64 renders this .12500000; the exact value rounds to .12345679.
		{"98765432109876.123456789", "9876543210987612345679"},
		{"8.123456789e-06", "812"},
		{"0.000000005", "1"},
		{"0.0000000049", "0"},
		{"1E+2", "10000000000"},
	}
	for _, c := range cases {
		got, err := decimalToScaled(c.in, 8)
		if err != nil {
			t.Fatalf("decimalToScaled(%q): %v", c.in, err)
		}
		if got.String() != c.want {
			t.Errorf("decimalToScaled(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"-1", "1e999999999", "abc", ""} {
		if _, err := decimalToScaled(bad, 8); err == nil {
			t.Errorf("decimalToScaled(%q) accepted", bad)
		}
	}
}

func TestHistory_FetchesWindowExactlyOverHTTP(t *testing.T) {
	noCallGap(t)
	var gotPath, gotQuery, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotKey = r.URL.Path, r.URL.RawQuery, r.Header.Get("x-cg-pro-api-key")
		_, _ = w.Write([]byte(historyFixture))
	}))
	defer srv.Close()

	p := &Poller{Endpoint: srv.URL, APIKey: "test-key-not-real", TickerToID: map[string]string{"XLM": "stellar"}}
	pair := xlmUSDPair(t)
	got, err := p.History(context.Background(), []canonical.Pair{pair}, histFrom, histTo)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if gotPath != "/api/v3/coins/stellar/market_chart/range" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "from=1763222400&precision=full&to=1763233200&vs_currency=usd" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotKey != "test-key-not-real" {
		t.Errorf("pro key header = %q", gotKey)
	}

	want := []struct {
		at    time.Time
		price string
	}{
		{histFrom, "9876543210987612345679"},
		{time.UnixMilli(1763226000123).UTC(), "812"},
		{time.UnixMilli(1763233199999).UTC(), "1"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d updates, want %d (window is [from, to)): %+v", len(got), len(want), got)
	}
	for i, w := range want {
		u := got[i]
		if !u.Timestamp.Equal(w.at) || u.Price.String() != w.price {
			t.Errorf("update %d = (%s, %s), want (%s, %s)", i, u.Timestamp, u.Price, w.at, w.price)
		}
		if u.Source != SourceName || u.Decimals != DefaultDecimals || u.Ledger != 0 || u.OpIndex != 0 || u.Observer != "" ||
			u.Asset != pair.Base || u.Quote != pair.Quote {
			t.Errorf("update %d shape differs from a live row: %+v", i, u)
		}
		if !strings.HasPrefix(u.TxHash, HistoryTxHashPrefix) || len(u.TxHash) != 64 {
			t.Errorf("update %d tx_hash %q lacks the history prefix", i, u.TxHash)
		}
		if err := u.Validate(); err != nil {
			t.Errorf("update %d invalid: %v", i, err)
		}
	}

	again, err := p.History(context.Background(), []canonical.Pair{pair}, histFrom, histTo)
	if err != nil {
		t.Fatalf("History re-run: %v", err)
	}
	for i := range got {
		if again[i].TxHash != got[i].TxHash || !again[i].Timestamp.Equal(got[i].Timestamp) {
			t.Fatalf("re-run identity differs at %d: %s@%s vs %s@%s", i, again[i].TxHash, again[i].Timestamp, got[i].TxHash, got[i].Timestamp)
		}
	}
}

func TestHistoryTxHash_NeverEqualsALiveHash(t *testing.T) {
	at := time.Unix(1763222400, 0).UTC()
	for _, tk := range []string{"XLM", "BTC", "PYUSD", "HIST"} {
		h := historyTxHash(tk, "USD", at)
		live := syntheticTxHash(tk, "usd", at.Unix())
		if h == live || h[18:20] == live[18:20] {
			t.Errorf("%s: history hash %s shares its marker byte with live %s", tk, h, live)
		}
	}
	if historyTxHash("XLM", "USD", at) == historyTxHash("XLM", "EUR", at) {
		t.Error("history hash ignores the quote currency")
	}
}

func TestHistory_RefusesBadWindows(t *testing.T) {
	p := &Poller{TickerToID: map[string]string{"XLM": "stellar"}}
	pairs := []canonical.Pair{xlmUSDPair(t)}
	if _, err := p.History(context.Background(), pairs, histTo, histFrom); err == nil {
		t.Error("inverted window accepted")
	}
	if _, err := p.History(context.Background(), pairs, histFrom, histFrom.Add(MaxHistoryWindow+time.Hour)); err == nil {
		t.Error("window past the hourly limit accepted")
	}
}

func TestHistory_429ArmsCooldownInsteadOfHammering(t *testing.T) {
	noCallGap(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := &Poller{Endpoint: srv.URL, TickerToID: map[string]string{"XLM": "stellar"}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := p.History(ctx, []canonical.Pair{xlmUSDPair(t)}, histFrom, histTo)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the walk to wait out the cooldown until the deadline", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("venue hit %d times inside the cooldown, want 1", n)
	}
	if p.CooldownRemaining() < MinBackoff-time.Second {
		t.Errorf("cooldown %v, want at least MinBackoff", p.CooldownRemaining())
	}
}
