package binance

import (
	"testing"
	"time"
)

// NewStreamer's default backoff is 5 s: large enough not to hammer Binance in a venue-wide
// outage, small enough to keep the per-cycle data-loss window short on a healthy connection.
// Without the healthy-connection reset in run(), backoff would drift to MaxBackoff (60 s) and
// stay there; this test pins the defaults against silent regression.
func TestNewStreamer_DefaultInitialBackoffIs5s(t *testing.T) {
	s := NewStreamer(nil)
	if got, want := s.InitialBackoff, 5*time.Second; got != want {
		t.Errorf("InitialBackoff = %v, want %v (F-0029)", got, want)
	}
	if got, want := s.MaxBackoff, 60*time.Second; got != want {
		t.Errorf("MaxBackoff = %v, want %v", got, want)
	}
}
