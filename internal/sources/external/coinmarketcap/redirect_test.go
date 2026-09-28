// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package coinmarketcap

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
)

// The paid key travels in a custom header, which net/http re-sends on
// every redirect hop regardless of host.
func TestPollOnce_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, APIKeyHeader)
	p, err := NewPoller("probe")
	if err != nil {
		t.Fatal(err)
	}
	p.Endpoint = trap.URL
	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))
	trap.AssertKeyStayedOnOrigin(t)
}
