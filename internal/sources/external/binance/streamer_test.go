package binance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// newTestWSServer accepts one WebSocket connection, writes the given aggTrade frames, then holds it open until ctx cancellation.
func newTestWSServer(t *testing.T, frames []string) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	var connectionCount int

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		mu.Lock()
		connectionCount++
		mu.Unlock()
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "bye") }()

		// Dump the fixture frames.
		for _, f := range frames {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(f)); err != nil {
				return
			}
		}
		// Hold the connection open until the client cancels, as real Binance does, so Start is driven by ctx and not server EOF.
		<-r.Context().Done()
	}))
}

// replaceScheme swaps http → ws (or https → wss) since httptest
// serves on http:// but coder/websocket expects ws://.
func replaceScheme(httpURL string) string {
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	}
	return "ws://" + strings.TrimPrefix(httpURL, "http://")
}

func TestStreamer_EndToEnd(t *testing.T) {
	// Two fixture frames simulate a busy second on XLMUSDT.
	frames := []string{
		`{"stream":"xlmusdt@aggTrade","data":{"e":"aggTrade","E":1745000000000,"s":"XLMUSDT","a":1,"p":"0.17582","q":"152.34","f":1,"l":2,"T":1745000000100,"m":true}}`,
		`{"stream":"xlmusdt@aggTrade","data":{"e":"aggTrade","E":1745000001000,"s":"XLMUSDT","a":2,"p":"0.17590","q":"200.00","f":3,"l":4,"T":1745000001100,"m":false}}`,
	}
	srv := newTestWSServer(t, frames)
	defer srv.Close()

	s := NewStreamer(buildPairMap(t))
	s.Endpoint = replaceScheme(srv.URL)

	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	pair, _ := canonical.NewPair(xlm, usdt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := s.Start(ctx, []canonical.Pair{pair})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Read two trades, then cancel.
	got := []canonical.Trade{}
loop:
	for len(got) < 2 {
		select {
		case trade, ok := <-out:
			if !ok {
				break loop
			}
			got = append(got, trade)
		case <-ctx.Done():
			t.Fatalf("timeout waiting for trades; got %d", len(got))
		}
	}
	cancel()

	if len(got) != 2 {
		t.Fatalf("expected 2 trades, got %d", len(got))
	}
	// Verify trade ordering + stamped fields.
	if got[0].Timestamp.UnixMilli() != 1745000000100 {
		t.Errorf("trade[0] ts = %d want 1745000000100", got[0].Timestamp.UnixMilli())
	}
	if got[1].Timestamp.UnixMilli() != 1745000001100 {
		t.Errorf("trade[1] ts = %d want 1745000001100", got[1].Timestamp.UnixMilli())
	}
	if got[0].Source != "binance" {
		t.Errorf("source = %q want binance", got[0].Source)
	}
	if !got[0].Pair.Base.Equal(xlm) {
		t.Errorf("trade[0] base = %+v want XLM", got[0].Pair.Base)
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
	s := NewStreamer(buildPairMap(t))
	// Construct a pair that's NOT in the map (e.g. DOGE/USDT —
	// DOGE is allow-listed as a crypto ticker but we didn't add
	// DOGEUSDT to buildPairMap).
	doge, _ := canonical.NewCryptoAsset("DOGE")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	pair, _ := canonical.NewPair(doge, usdt)
	_, err := s.Start(context.Background(), []canonical.Pair{pair})
	if err == nil {
		t.Error("expected error for pair not in PairMap")
	}
}

func TestStreamer_BuildStreamURL(t *testing.T) {
	s := NewStreamer(buildPairMap(t))
	s.Endpoint = "wss://stream.binance.com:9443/stream"
	u, err := s.buildStreamURL([]string{"XLMUSDT", "BTCUSDT"})
	if err != nil {
		t.Fatalf("buildStreamURL: %v", err)
	}
	// Order of the streams query param is preserved; case lowered.
	if !strings.Contains(u, "streams=xlmusdt%40aggTrade%2Fbtcusdt%40aggTrade") {
		t.Errorf("URL missing expected streams query: %s", u)
	}
}

// Accessor tests: these values feed per-source metric labels and the source-class filter, so a rename would mislabel every
// Binance trade.

func TestStreamer_Name(t *testing.T) {
	s := NewStreamer(map[string]canonical.Pair{})
	if got := s.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestStreamer_Class(t *testing.T) {
	s := NewStreamer(map[string]canonical.Pair{})
	if got := s.Class(); got != external.ClassExchange {
		t.Errorf("Class() = %q, want %q", got, external.ClassExchange)
	}
}

// Start has two pre-network reject paths, empty pairs and a pair not in PairMap; silent fallthrough would dial Binance with no
// subscription.

func TestStreamer_Start_emptyPairsRejected(t *testing.T) {
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	if _, err := s.Start(context.Background(), nil); err == nil {
		t.Error("expected error on empty pairs, got nil")
	}
}

func TestStreamer_Start_unknownPairRejected(t *testing.T) {
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	// MATIC is in the ADR-0014 allow-list but intentionally not in
	// DefaultPairs (skipped pending MATIC→POL migration). Stable
	// "known unknown" placeholder.
	matic, _ := canonical.NewCryptoAsset("MATIC")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	missing, err := canonical.NewPair(matic, usdt)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	_, err = s.Start(context.Background(), []canonical.Pair{missing})
	if err == nil {
		t.Fatal("expected error for unknown MATIC/USDT pair, got nil")
	}
	if !strings.Contains(err.Error(), "MATIC") {
		t.Errorf("error %q should cite the offending asset", err.Error())
	}
}

// TestNewStreamer_DefaultInitialBackoffIs5s: the 5 s default avoids hammering Binance in an outage; without run()'s
// healthy-connection reset, backoff would sit at MaxBackoff (60 s).
func TestNewStreamer_DefaultInitialBackoffIs5s(t *testing.T) {
	s := NewStreamer(nil)
	if got, want := s.InitialBackoff, 5*time.Second; got != want {
		t.Errorf("InitialBackoff = %v, want %v (F-0029)", got, want)
	}
	if got, want := s.MaxBackoff, 60*time.Second; got != want {
		t.Errorf("MaxBackoff = %v, want %v", got, want)
	}
}

// restBase resolves the backfill REST URL, swapping a ws:// or wss:// Endpoint for the REST base so HTTP is never sent to the
// websocket listener.

func TestRestBase_emptyEndpointFallsBackToREST(t *testing.T) {
	s := &Streamer{}
	if got := s.restBase(); got != RESTEndpoint {
		t.Errorf("restBase() = %q, want %q", got, RESTEndpoint)
	}
}

func TestRestBase_wsEndpointFallsBackToREST(t *testing.T) {
	for _, prefix := range []string{"ws://example.com", "wss://example.com"} {
		s := &Streamer{Endpoint: prefix}
		if got := s.restBase(); got != RESTEndpoint {
			t.Errorf("restBase(%q) = %q, want %q", prefix, got, RESTEndpoint)
		}
	}
}

func TestRestBase_httpEndpointPassesThrough(t *testing.T) {
	// Tests override Endpoint with an http:// value to redirect at
	// an httptest.Server — the function must NOT swallow it.
	s := &Streamer{Endpoint: "http://127.0.0.1:9999"}
	if got := s.restBase(); got != "http://127.0.0.1:9999" {
		t.Errorf("restBase() = %q, want passthrough", got)
	}
}
