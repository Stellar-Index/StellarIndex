package kraken

import (
	"testing"
	"time"
)

// G10-03 (F-0029 port): Kraken shares the same healthy-connection
// backoff drift that Binance/Bitstamp were fixed for. NewStreamer's
// default backoff is 5 s — large enough to avoid hammering Kraken on a
// venue-wide outage, small enough that the per-cycle data-loss window
// is ~5 s on a healthy connection. This test pins the defaults so a
// drive-by edit can't silently regress them.
func TestNewStreamer_DefaultInitialBackoffIs5s(t *testing.T) {
	s := NewStreamer(nil)
	if got, want := s.InitialBackoff, 5*time.Second; got != want {
		t.Errorf("InitialBackoff = %v, want %v (F-0029/G10-03)", got, want)
	}
	if got, want := s.MaxBackoff, 60*time.Second; got != want {
		t.Errorf("MaxBackoff = %v, want %v", got, want)
	}
}
