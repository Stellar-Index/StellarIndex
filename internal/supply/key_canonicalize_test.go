// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package supply

import "testing"

// TestCanonicalizeWatchedClassic pins the 2026-07-02 production bug:
// the config documents CODE-ISSUER (dash) but the observers match on
// CODE:ISSUER (colon). The raw config strings went straight into the
// watched sets, so the trustline/claimable/LP observers matched
// NOTHING and every classic asset's served supply degraded to its
// SAC-held slice (USDC 40M vs ~266M).
func TestCanonicalizeWatchedClassic(t *testing.T) {
	const usdcDash = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const usdcColon = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	got, err := CanonicalizeWatchedClassic([]string{usdcDash})
	if err != nil {
		t.Fatalf("dash-form (the documented config form) must canonicalize: %v", err)
	}
	if got[0] != usdcColon {
		t.Fatalf("canonicalized = %q, want %q", got[0], usdcColon)
	}

	// Colon form passes through (compatibility).
	got, err = CanonicalizeWatchedClassic([]string{usdcColon})
	if err != nil || got[0] != usdcColon {
		t.Fatalf("colon-form passthrough: got %v, %v", got, err)
	}

	// Garbage fails LOUDLY — a typo must never silently zero a
	// supply component again.
	if _, err := CanonicalizeWatchedClassic([]string{"not-an-asset"}); err == nil {
		t.Fatal("unparseable entry must error, not silently no-match")
	}
	// Non-classic canonical forms are config errors here too.
	if _, err := CanonicalizeWatchedClassic([]string{"native"}); err == nil {
		t.Fatal("native is not a watchable classic asset")
	}
	if _, err := CanonicalizeWatchedClassic([]string{""}); err == nil {
		t.Fatal("empty entry must error")
	}
}
