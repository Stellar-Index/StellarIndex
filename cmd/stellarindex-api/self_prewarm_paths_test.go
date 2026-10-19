// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/url"
	"slices"
	"testing"
)

// TestSelfPrewarmPaths_CoversDetailAndExplorerListings: a pass warms each asset's detail variants and every explorer listing read,
// and each listing path is a /v1/assets query the listing handler keys on.
func TestSelfPrewarmPaths_CoversDetailAndExplorerListings(t *testing.T) {
	got := selfPrewarmPaths([]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"})
	for _, want := range []string{
		"/v1/assets/native",
		"/v1/assets/native?include_thin=true",
		"/v1/assets/native/entry-changes",
		"/v1/assets/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN?include_thin=true",
		"/v1/assets?asset_class=all&limit=100&include=sparkline7d&include_thin=true",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("selfPrewarmPaths missing %q", want)
		}
	}
	for _, p := range explorerAssetListingPaths {
		u, err := url.Parse(p)
		if err != nil || u.Path != "/v1/assets" || u.RawQuery == "" {
			t.Errorf("listing path %q is not a /v1/assets query (err %v)", p, err)
		}
	}
}
