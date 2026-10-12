package ecb

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// ECB is a sanity-check feed, not in VWAP; classing it ClassExchange would double-count central-bank rates against retail
// liquidity.

func TestPoller_NameAndClass(t *testing.T) {
	p := NewPoller()
	if got := p.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
	if got := p.Class(); got != external.ClassAuthoritySanity {
		t.Errorf("Class() = %v, want ClassAuthoritySanity (ECB rates are reference, not retail)", got)
	}
}

func TestPoller_PollInterval_defaultAndOverride(t *testing.T) {
	p := NewPoller()
	p.Interval = 0
	if got := p.PollInterval(); got != DefaultPollInterval {
		t.Errorf("PollInterval(zero) = %v, want %v", got, DefaultPollInterval)
	}
	p.Interval = 5 * time.Minute
	if got := p.PollInterval(); got != 5*time.Minute {
		t.Errorf("PollInterval(5m) = %v, want 5m", got)
	}
}
