package kraken

import (
	"testing"
	"time"
)

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
