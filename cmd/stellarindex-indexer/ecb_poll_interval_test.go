package main

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// TestNewECBPoller_UsesConfiguredInterval is the regression test for `[external.ecb] poll_interval` being validated and
// then ignored, so the poller always ran at its 6h default.
func TestNewECBPoller_UsesConfiguredInterval(t *testing.T) {
	t.Parallel()

	p := newECBPoller(config.ExternalVenueConfig{PollInterval: 30 * time.Minute})
	if got, want := p.PollInterval(), 30*time.Minute; got != want {
		t.Errorf("PollInterval() = %v, want %v (config override was not applied)", got, want)
	}
}

func TestNewECBPoller_DefaultsWhenUnset(t *testing.T) {
	t.Parallel()

	p := newECBPoller(config.ExternalVenueConfig{})
	if got, want := p.PollInterval(), 6*time.Hour; got != want {
		t.Errorf("PollInterval() = %v, want connector default %v", got, want)
	}
}
