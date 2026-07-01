package binance

import (
	"testing"
	"time"
)

// F-0029: NewStreamer's default backoff is 5 s — large enough to
// avoid hammering Binance on a venue-wide outage, small enough that
// the per-cycle data-loss window is ~5 s on a healthy connection.
// Pre-fix it was 1 s (defaults) but with no reset path, so in
// production the running value drifted to MaxBackoff (60 s) and
// stayed there. The defaults change + healthy-connection reset in
// run() are the actual fix; this test pins the defaults so a future
// drive-by edit can't silently regress them.
func TestNewStreamer_DefaultInitialBackoffIs5s(t *testing.T) {
	s := NewStreamer(nil)
	if got, want := s.InitialBackoff, 5*time.Second; got != want {
		t.Errorf("InitialBackoff = %v, want %v (F-0029)", got, want)
	}
	if got, want := s.MaxBackoff, 60*time.Second; got != want {
		t.Errorf("MaxBackoff = %v, want %v", got, want)
	}
}
