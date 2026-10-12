package binance

import (
	"testing"
	"time"
)

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
