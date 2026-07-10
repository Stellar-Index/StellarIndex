// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import "testing"

// TestSep1FetchDomain pins the curated-override semantics (board
// #47): Circle's on-chain home_domain 404s its TOML; the override
// redirects the FETCH while the on-chain value stays authoritative
// for identity display.
func TestSep1FetchDomain(t *testing.T) {
	if got := sep1FetchDomain("GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "circle.com"); got != "centre.io" {
		t.Errorf("USDC issuer fetch domain = %q, want centre.io", got)
	}
	if got := sep1FetchDomain("GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", "aqua.network"); got != "aqua.network" {
		t.Errorf("non-overridden issuer = %q, want on-chain domain", got)
	}
}
