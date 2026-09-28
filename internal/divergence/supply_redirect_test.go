// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package divergence

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
)

// The paid key travels in a custom header, which net/http re-sends on
// every redirect hop regardless of host — for the default client and for
// an injected one that sets no redirect policy of its own.
func TestCoinGeckoSupplyReference_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	for name, injected := range map[string]*http.Client{
		"default":  nil,
		"injected": {Timeout: 5 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			trap := httpxtest.NewRedirectTrap(t, "x-cg-pro-api-key")
			ref := NewCoinGeckoSupplyReference(CoinGeckoSupplyOptions{
				HTTPClient: injected, BaseURL: trap.URL, APIKey: "probe",
			})
			if _, err := ref.LookupCirculatingSupply(context.Background(), canonical.NativeAsset()); err == nil {
				t.Error("lookup succeeded through a refused redirect")
			}
			trap.AssertKeyStayedOnOrigin(t)
			if injected != nil && injected.CheckRedirect != nil {
				t.Error("the caller's client was mutated; the policy belongs on a copy")
			}
		})
	}
}
