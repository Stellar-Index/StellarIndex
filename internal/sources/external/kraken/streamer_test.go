package kraken

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// newTestKrakenServer plays back a scripted Kraken v2 session:
//
//  1. Sends a status frame on connect.
//  2. Expects a subscribe request; reads it to validate the symbols
//     the Streamer sent.
//  3. Replies with a subscribe-ack.
//  4. Sends trade-channel updates.
//
// Exposed via httptest so we exercise the full JSON-subscribe
// handshake without hitting real Kraken infra.
func newTestKrakenServer(t *testing.T, tradeFrames []string, capturedSub *subscribeReq) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "bye") }()

		// 1. Status frame.
		_ = conn.Write(r.Context(), websocket.MessageText,
			[]byte(`{"channel":"status","data":[{"system":"online","api_version":"v2"}]}`))

		// 2. Read the subscribe request the client sent.
		_, subRaw, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if capturedSub != nil {
			_ = json.Unmarshal(subRaw, capturedSub)
		}

		// 3. Ack.
		_ = conn.Write(r.Context(), websocket.MessageText,
			[]byte(`{"method":"subscribe","success":true,"result":{"channel":"trade"}}`))

		// 4. Trade frames.
		for _, f := range tradeFrames {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(f)); err != nil {
				return
			}
		}

		// Hold the connection open until client cancels.
		<-r.Context().Done()
	}))
}

func replaceScheme(httpURL string) string {
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	}
	return "ws://" + strings.TrimPrefix(httpURL, "http://")
}

func TestStreamer_EndToEnd(t *testing.T) {
	frames := []string{
		`{"channel":"trade","type":"update","data":[{"symbol":"XLM/USD","side":"buy","qty":100,"price":0.17582,"ord_type":"market","trade_id":1,"timestamp":"2026-04-24T00:00:00Z"}]}`,
		`{"channel":"heartbeat"}`,
		`{"channel":"trade","type":"update","data":[{"symbol":"XLM/EUR","side":"sell","qty":50,"price":0.16,"ord_type":"limit","trade_id":2,"timestamp":"2026-04-24T00:00:01Z"}]}`,
	}
	var capturedSub subscribeReq
	srv := newTestKrakenServer(t, frames, &capturedSub)
	defer srv.Close()

	s := NewStreamer(mustPairMap(t))
	s.Endpoint = replaceScheme(srv.URL)

	// Scope the test to a couple of pairs the fixture emits — the
	// real DefaultPairList has 8 symbols, overkill for this test.
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	eur, _ := canonical.NewFiatAsset("EUR")
	xlmUsd, _ := canonical.NewPair(xlm, usd)
	xlmEur, _ := canonical.NewPair(xlm, eur)
	pairs := []canonical.Pair{xlmUsd, xlmEur}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := s.Start(ctx, pairs)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := []canonical.Trade{}
loop:
	for len(got) < 2 {
		select {
		case tr, ok := <-out:
			if !ok {
				break loop
			}
			got = append(got, tr)
		case <-ctx.Done():
			t.Fatalf("timeout waiting for trades; got %d", len(got))
		}
	}
	cancel()

	if len(got) != 2 {
		t.Fatalf("expected 2 trades (heartbeat suppressed), got %d", len(got))
	}
	if got[0].Source != "kraken" {
		t.Errorf("source = %q want kraken", got[0].Source)
	}
	// Subscribe request should have contained both symbols.
	if capturedSub.Method != "subscribe" {
		t.Errorf("subscribe method = %q want subscribe", capturedSub.Method)
	}
	if capturedSub.Params.Channel != "trade" {
		t.Errorf("subscribe channel = %q want trade", capturedSub.Params.Channel)
	}
	seen := map[string]bool{}
	for _, sym := range capturedSub.Params.Symbol {
		seen[sym] = true
	}
	if !seen["XLM/USD"] || !seen["XLM/EUR"] {
		t.Errorf("subscribe symbols missing XLM/USD or XLM/EUR; got %v",
			capturedSub.Params.Symbol)
	}
}

// lockedBuffer is a goroutine-safe log sink for the streamer's logger.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStreamer_SurfacesRejectionAndSkips is the GH-995 end-to-end
// guard: a rejected subscription and an unparsable trade entry each
// leave a metric and a log line instead of vanishing on a healthy
// socket.
func TestStreamer_SurfacesRejectionAndSkips(t *testing.T) {
	frames := []string{
		`{"error":"Currency pair not supported XLM/GBP","method":"subscribe","success":false,"symbol":"XLM/GBP"}`,
		`{"method":"subscribe","success":true,"result":{"channel":"trade","symbol":"XLM/USD"}}`,
		`{"channel":"trade","type":"update","data":[` +
			`{"symbol":"XLM/USDX","qty":5,"price":0.5,"trade_id":1,"timestamp":"2026-04-24T00:00:00Z"},` +
			`{"symbol":"XLM/USD","qty":10,"price":0.175,"trade_id":2,"timestamp":"2026-04-24T00:00:01Z"}]}`,
	}
	srv := newTestKrakenServer(t, frames, nil)
	defer srv.Close()

	skips := obs.CEXStreamEntrySkipsTotal.WithLabelValues(SourceName, "unknown_symbol")
	skipsBefore := testutil.ToFloat64(skips)
	obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, "XLM/USD").Set(1)

	var logs lockedBuffer
	s := NewStreamer(mustPairMap(t))
	s.Endpoint = replaceScheme(srv.URL)
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	gbp, _ := canonical.NewFiatAsset("GBP")
	xlmUsd, _ := canonical.NewPair(xlm, usd)
	xlmGbp, _ := canonical.NewPair(xlm, gbp)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := s.Start(ctx, []canonical.Pair{xlmUsd, xlmGbp})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case tr := <-out:
		if tr.Pair.String() != xlmUsd.String() {
			t.Fatalf("trade pair = %s, want %s", tr.Pair, xlmUsd)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for the XLM/USD trade")
	}
	got := logs.String()
	cancel()

	if v := testutil.ToFloat64(obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, "XLM/GBP")); v != 1 {
		t.Errorf("subscription_rejected{symbol=XLM/GBP} = %v, want 1", v)
	}
	if v := testutil.ToFloat64(obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, "XLM/USD")); v != 0 {
		t.Errorf("subscription_rejected{symbol=XLM/USD} = %v, want 0 after the accepted ack", v)
	}
	if d := testutil.ToFloat64(skips) - skipsBefore; d != 1 {
		t.Errorf("entry_skips{reason=unknown_symbol} delta = %v, want 1", d)
	}
	if !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "Currency pair not supported XLM/GBP") {
		t.Errorf("no ERROR log carrying the venue's rejection text; logs:\n%s", got)
	}
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "reason=unknown_symbol") {
		t.Errorf("no WARN log for the skipped entry; logs:\n%s", got)
	}
}

func TestStreamer_RejectsEmptyPairs(t *testing.T) {
	s := NewStreamer(map[string]canonical.Pair{})
	_, err := s.Start(context.Background(), nil)
	if err == nil {
		t.Error("expected error on empty pairs")
	}
}

func TestStreamer_RejectsUnconfiguredPair(t *testing.T) {
	s := NewStreamer(mustPairMap(t))
	// MATIC/USD isn't in DefaultPairs (see allow-list comment in
	// binance/start_errors_test.go for the placeholder rationale).
	matic, _ := canonical.NewCryptoAsset("MATIC")
	usd, _ := canonical.NewFiatAsset("USD")
	p, _ := canonical.NewPair(matic, usd)
	_, err := s.Start(context.Background(), []canonical.Pair{p})
	if err == nil {
		t.Error("expected error for pair not in PairMap")
	}
}

func mustPairMap(t *testing.T) map[string]canonical.Pair {
	t.Helper()
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	return m
}
