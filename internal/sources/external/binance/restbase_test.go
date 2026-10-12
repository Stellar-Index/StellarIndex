package binance

import "testing"

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
