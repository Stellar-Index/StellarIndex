package exchangeratesapi

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// Name and Class must agree with internal/sources/external/registry.go, or the aggregator's class-based VWAP filter treats
// this venue inconsistently.

func TestPoller_NameAndClass(t *testing.T) {
	p, err := NewPoller("key")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if got := p.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
	if got := p.Class(); got != external.ClassExchange {
		t.Errorf("Class() = %v, want ClassExchange (registry treats paid FX feeds as exchange-equivalent)", got)
	}
}

func TestPoller_PollInterval_defaultAndOverride(t *testing.T) {
	p, err := NewPoller("k")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Interval = 0
	if got := p.PollInterval(); got != DefaultPollInterval {
		t.Errorf("PollInterval(zero) = %v, want %v", got, DefaultPollInterval)
	}
	p.Interval = 7 * time.Second
	if got := p.PollInterval(); got != 7*time.Second {
		t.Errorf("PollInterval(7s) = %v, want 7s", got)
	}
}
