package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/url"
	"testing"
	"time"

	v1 "github.com/RatesEngine/rates-engine/internal/api/v1"
	"github.com/RatesEngine/rates-engine/internal/canonical"
)

// stubHistoryReader implements v1.HistoryReader with a static slice.
type stubHistoryReader struct {
	trades   []canonical.Trade
	lastCall struct {
		from, to time.Time
		limit    int
	}
	err error
}

func (r *stubHistoryReader) TradesInRange(_ context.Context, _ canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	r.lastCall.from = from
	r.lastCall.to = to
	r.lastCall.limit = limit
	if r.err != nil {
		return nil, r.err
	}
	return r.trades, nil
}

func mkHistTrade(price int64) canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source: "soroswap", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1)),
		QuoteAmount: canonical.NewAmount(big.NewInt(price)),
	}
}

func TestHistory_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD")
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503 (no reader wired)", resp.StatusCode)
	}
}

func TestHistory_MissingBase400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?quote=fiat:USD")
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHistory_InvalidTime400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&from=yesterday")
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHistory_FromAfterTo400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	q := url.Values{}
	q.Set("base", "native")
	q.Set("quote", "fiat:USD")
	q.Set("from", "2026-04-23T12:00:00Z")
	q.Set("to", "2026-04-23T11:00:00Z")
	resp := mustGet(t, ts.URL+"/v1/history?"+q.Encode())
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHistory_InvalidLimit400(t *testing.T) {
	srv := v1.New(v1.Options{History: &stubHistoryReader{}})
	ts := httpTestServer(t, srv)

	for _, bad := range []string{"0", "10001", "-5", "abc"} {
		resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit="+bad)
		if resp.StatusCode != 400 {
			t.Errorf("limit=%q: status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestHistory_ReturnsTrades(t *testing.T) {
	reader := &stubHistoryReader{
		trades: []canonical.Trade{mkHistTrade(100), mkHistTrade(101)},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD&limit=50")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.TradeRow `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 2 {
		t.Fatalf("got %d rows, want 2", len(env.Data))
	}
	if env.Data[0].Source != "soroswap" {
		t.Errorf("source = %q", env.Data[0].Source)
	}
	if env.Data[0].BaseAsset != "native" || env.Data[0].QuoteAsset != "fiat:USD" {
		t.Errorf("pair fields wrong: %+v", env.Data[0])
	}
	if env.Data[0].Price == "" {
		t.Error("price missing")
	}
	if reader.lastCall.limit != 50 {
		t.Errorf("limit threaded to reader = %d, want 50", reader.lastCall.limit)
	}
}

func TestHistory_DefaultWindowIs1Hour(t *testing.T) {
	// When neither from nor to is set, the handler should compute a
	// 1-hour window ending ~now. Check the window duration rather
	// than absolute times (to minimize test-clock flakiness).
	reader := &stubHistoryReader{trades: nil}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	_ = mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD")

	if reader.lastCall.from.IsZero() || reader.lastCall.to.IsZero() {
		t.Fatal("handler didn't pass from/to to reader")
	}
	dur := reader.lastCall.to.Sub(reader.lastCall.from)
	if dur != time.Hour {
		t.Errorf("default window = %v, want 1h", dur)
	}
}

func TestHistory_EmptyListReturnsEmptyArray(t *testing.T) {
	// No trades in the window → empty array, not null.
	srv := v1.New(v1.Options{History: &stubHistoryReader{trades: nil}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?base=native&quote=fiat:USD")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	var parsed struct {
		Data []v1.TradeRow `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if parsed.Data == nil {
		t.Error("empty result should be [] not null")
	}
}
