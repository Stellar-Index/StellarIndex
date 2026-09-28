// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package forex

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
)

// net/http keeps Authorization across a redirect that changes only the
// port or scheme of the same hostname.
func TestClient_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "Authorization")
	c := NewClient("probe").WithBase(trap.URL)
	if _, _, err := c.LatestUSDRates(context.Background()); err == nil {
		t.Error("LatestUSDRates succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}
