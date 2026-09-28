// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package coingecko

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
)

// The paid key travels in a custom header, which net/http re-sends on
// every redirect hop regardless of host.
func TestPollOnce_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "x-cg-pro-api-key")
	p := NewPoller()
	p.Endpoint = trap.URL
	p.APIKey = "probe"
	if _, _, err := p.PollOnce(context.Background(), buildPairs(t)); err == nil {
		t.Error("PollOnce succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}

func TestBackfillRange_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "x-cg-demo-api-key")
	p := NewPoller()
	p.Endpoint = trap.URL
	p.DemoAPIKey = "probe"
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := p.BackfillRange(context.Background(), testPair(t), from, from.AddDate(0, 0, 7)); err == nil {
		t.Error("BackfillRange succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}
