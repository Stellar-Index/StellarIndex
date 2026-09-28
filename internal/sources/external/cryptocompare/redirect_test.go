// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package cryptocompare

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
)

// net/http keeps Authorization across a redirect that changes only the
// port or scheme of the same hostname.
func TestPollOnce_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "Authorization")
	p, err := NewPoller("probe")
	if err != nil {
		t.Fatal(err)
	}
	p.Endpoint = trap.URL
	if _, _, err := p.PollOnce(context.Background(), buildPairs(t)); err == nil {
		t.Error("PollOnce succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}
