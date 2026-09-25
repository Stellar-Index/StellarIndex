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

// TestCanonicalizeWatchedClassicRejectsInvalidColonForm: a colon-form
// entry is parsed like any other, so a CODE:ISSUER typo or an off-chain
// prefixed id fails loudly instead of joining the watched set verbatim
// and matching nothing.
func TestCanonicalizeWatchedClassicRejectsInvalidColonForm(t *testing.T) {
	for _, e := range []string{
		// Last character altered: the issuer strkey CRC16 fails.
		"USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVM",
		"USDC:not-a-strkey",
		"TOOLONGASSETCODE:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"fiat:USD",
		"crypto:BTC",
		"rwa:XAU",
		"raw:anything",
		":GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"USDC:",
	} {
		if got, err := CanonicalizeWatchedClassic([]string{e}); err == nil {
			t.Errorf("CanonicalizeWatchedClassic(%q) = %v, want a loud error", e, got)
		}
	}
	// A valid colon-form entry still round-trips, code case preserved.
	const yxlm = "yXLM:GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	if got, err := CanonicalizeWatchedClassic([]string{yxlm}); err != nil || len(got) != 1 || got[0] != yxlm {
		t.Fatalf("valid colon form: got %v, %v; want [%s]", got, err, yxlm)
	}
}

// TestCanonicalizePolicyKeys: the classic and SEP-41 computers look
// per_asset_locked_sets / max_supply_overrides up by exact AssetKey
// match, so the dash spelling watched_classic_assets documents must be
// re-keyed to colon form, and a key nothing reads must fail loudly.
func TestCanonicalizePolicyKeys(t *testing.T) {
	const (
		usdcDash      = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcColon     = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		sep41Contract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	)
	got, err := CanonicalizePolicyKeys(map[string]string{usdcDash: "1", sep41Contract: "2"})
	if err != nil {
		t.Fatalf("CanonicalizePolicyKeys: %v", err)
	}
	if len(got) != 2 || got[usdcColon] != "1" || got[sep41Contract] != "2" {
		t.Fatalf("CanonicalizePolicyKeys = %v, want {%s:1 %s:2}", got, usdcColon, sep41Contract)
	}
	for name, in := range map[string]map[string]string{
		"XLM":            {"XLM": "1"},
		"native":         {"native": "1"},
		"garbage":        {"USDC_GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": "1"},
		"off-chain":      {"fiat:USD": "1"},
		"two spellings":  {usdcDash: "1", usdcColon: "2"},
		"bad issuer CRC": {"USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVM": "1"},
	} {
		if out, err := CanonicalizePolicyKeys(in); err == nil {
			t.Errorf("%s: CanonicalizePolicyKeys(%v) = %v, want an error", name, in, out)
		}
	}
}
