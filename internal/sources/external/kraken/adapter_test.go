package kraken

import (
	"testing"
	"time"
)

func TestDefaultPairList_matchesDefaultPairs(t *testing.T) {
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	list, err := DefaultPairList()
	if err != nil {
		t.Fatalf("DefaultPairList: %v", err)
	}
	if len(list) != len(m) {
		t.Errorf("list len = %d, want %d", len(list), len(m))
	}
	if len(list) == 0 {
		t.Error("DefaultPairList returned empty slice")
	}
}

// krakenXLMWsnames is the set of XLM pairs Kraken's public AssetPairs
// endpoint lists by wsname. A symbol outside it is rejected at subscribe
// time with "Currency pair not supported" and never delivers a trade.
var krakenXLMWsnames = map[string]bool{
	"XLM/EUR": true,
	"XLM/GBP": true,
	"XLM/USD": true,
	"XLM/XBT": true,
}

// Kraken shares the healthy-connection backoff concern of Binance/Bitstamp. NewStreamer's default
// backoff is 5 s: large enough not to hammer Kraken during a venue-wide outage, small enough that
// the per-cycle data-loss window on a healthy connection stays ~5 s. This test pins the defaults.
func TestNewStreamer_DefaultInitialBackoffIs5s(t *testing.T) {
	s := NewStreamer(nil)
	if got, want := s.InitialBackoff, 5*time.Second; got != want {
		t.Errorf("InitialBackoff = %v, want %v (F-0029/G10-03)", got, want)
	}
	if got, want := s.MaxBackoff, 60*time.Second; got != want {
		t.Errorf("MaxBackoff = %v, want %v", got, want)
	}
}

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
	s := &Streamer{Endpoint: "http://127.0.0.1:9999"}
	if got := s.restBase(); got != "http://127.0.0.1:9999" {
		t.Errorf("restBase() = %q, want passthrough", got)
	}
}
