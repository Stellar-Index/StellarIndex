// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── tier 3a: the alias-complete direct leg (#372 F4) ────────────────

// The single declared USD peg on r1, its SAC wrapper, and one wrapper for
// a classic that is NOT a peg — so a test can tell "expanded the peg set"
// from "swept in every wrapper the operator declared".
var (
	usdcClassicPeg = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdcSAC        = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	aquaSAC        = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"

	pegSACWrappersFixture = map[string]string{
		usdcSAC: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		aquaSAC: "AQUA:GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
	}
)

// TestVWAPUSDFXResolver_XLMAnchorResolvesThroughItsSorobanForms is the
// #372-F4 regression: the tier-3/4 XLM/USD anchor was bound to ONE
// spelling of XLM (`native`) against ONE spelling of the peg (the
// classic), and XLM's USD markets are split across its three canonical
// identities BY VENUE. Measured on r1 2026-09-03, prices_1m buckets
// clearing this query's own dust floor:
//
//	native      / USDC-GA5Z… (classic)  215,790 buckets, from 2026-03-12
//	CAS3J7…SAC  / CCW67T…SAC (the SAC)  291,883 buckets, from 2024-03-12
//	every other combination of those forms                0
//
// so the pre-fix predicate could not price ANY on-chain XLM trade before
// 2026-03-12 — 28,534 sdex/AMM XLM-base rows in 2025-06 alone, moving
// 19,174,885 XLM, all stored with usd_volume NULL and served as $0.00.
//
// The scripted driver replays that exact shape: `native` misses,
// `crypto:XLM` misses, the SAC form hits. The assertion is the RESOLVED
// RATE, not merely that something came back.
func TestVWAPUSDFXResolver_XLMAnchorResolvesThroughItsSorobanForms(t *testing.T) {
	t.Parallel()
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	bucket := at.Add(-3 * time.Minute)
	// A real XLM/USDC VWAP shape: NUMERIC::text arrives fully scaled.
	const storedVWAP = "0.24170000000000000000"
	const wantRate = "0.2417"

	store, conn := newScriptedStore(t,
		scriptedResult{cols: []string{"bucket", "vwap"}}, // native     → miss
		scriptedResult{cols: []string{"bucket", "vwap"}}, // crypto:XLM → miss
		scriptedResult{ // the SAC form → hit
			cols: []string{"bucket", "vwap"},
			rows: [][]driver.Value{{bucket, storedVWAP}},
		},
	)
	r, err := NewVWAPUSDFXResolver(store, VWAPUSDFXResolverOptions{
		USDPegs:        []string{usdcClassicPeg},
		PegSACWrappers: pegSACWrappersFixture,
		Clock:          func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	got, ok, err := r.USDPriceAt(context.Background(), canonical.NativeAsset(), at)
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatal("XLM/USD anchor declined: the Soroban XLM book was never asked for " +
			"(pre-#372-F4 behaviour — every 2024/2025 on-chain XLM trade reads $0.00)")
	}
	if got != wantRate {
		t.Errorf("USDPriceAt(native) = %q, want %q", got, wantRate)
	}

	// The base side must be tried in canonical PRIORITY order, SAC last:
	// a set-shaped `= ANY(forms)` would let the thin Soroban pool outrank
	// the deep SDEX book whenever it printed more recently.
	wantBases := []string{"native", "crypto:XLM", canonical.XLMSacContractID}
	if len(conn.stmts) != len(wantBases) {
		t.Fatalf("issued %d direct-leg statements, want %d (one per XLM form)", len(conn.stmts), len(wantBases))
	}
	for i, want := range wantBases {
		if got := conn.stmts[i].arg(t, 1); got != want {
			t.Errorf("statement %d bound base_asset %v, want %q", i, got, want)
		}
	}

	// And the peg side must carry the SAC wrapper of the declared peg —
	// without it the SAC-base query matches nothing, since the Soroban
	// book quotes in the USDC SAC, not in classic USDC.
	pegArg := conn.stmts[len(conn.stmts)-1].arg(t, 2)
	pegs, isSlice := pegArg.([]string)
	if !isSlice {
		t.Fatalf("peg arg is %T, want []string", pegArg)
	}
	if len(pegs) != 2 || pegs[0] != usdcClassicPeg || pegs[1] != usdcSAC {
		t.Errorf("bound peg forms = %q, want [%q %q]", pegs, usdcClassicPeg, usdcSAC)
	}
}

// TestVWAPUSDFXResolver_AliasLoopStopsAtTheEstablishedForm pins the other
// half of the #372-F4 contract: the loop is ADDITIVE. Where the
// pre-existing `native` form already answers — every date from 2026-03-12
// — it answers FIRST, no further form is consulted, and the result is
// what the pre-fix code returned. This is the manipulation guard
// [canonical.AssetAliases] documents (SAC form LAST) expressed as
// behaviour: the fresher, 99.00 Soroban print must not win.
func TestVWAPUSDFXResolver_AliasLoopStopsAtTheEstablishedForm(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)
	bucket := at.Add(-1 * time.Minute)

	store, conn := newScriptedStore(t,
		scriptedResult{cols: []string{"bucket", "vwap"}, rows: [][]driver.Value{{bucket, "0.31500000"}}},
		// A second scripted row a later form would consume if the loop
		// wrongly kept going / wrongly preferred the fresher bucket.
		scriptedResult{cols: []string{"bucket", "vwap"}, rows: [][]driver.Value{{at, "99.00000000"}}},
	)
	r, err := NewVWAPUSDFXResolver(store, VWAPUSDFXResolverOptions{
		USDPegs:        []string{usdcClassicPeg},
		PegSACWrappers: pegSACWrappersFixture,
		Clock:          func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	got, ok, err := r.USDPriceAt(context.Background(), canonical.NativeAsset(), at)
	if err != nil || !ok {
		t.Fatalf("USDPriceAt = (%q, %t, %v), want a rate", got, ok, err)
	}
	if got != "0.315" {
		t.Errorf("USDPriceAt(native) = %q, want 0.315 (the established SDEX form)", got)
	}
	if len(conn.stmts) != 1 {
		t.Fatalf("issued %d statements, want exactly 1 — the loop must stop at the first hit", len(conn.stmts))
	}
	if got := conn.stmts[0].arg(t, 1); got != "native" {
		t.Errorf("bound base_asset %v, want \"native\"", got)
	}
}

// TestUSDPegForms_MatchesTheExactTierPegSet is the lockstep guard between
// the package's two spellings of "is this quote a declared dollar":
// [USDVolumeQuoteSpec.QuoteUSDPegInfo] (tiers 1/2) and [usdPegForms]
// (tiers 3/4). They drifted — the exact tier let a SAC INHERIT its
// classic's peg while the FX tier bound the classic strings alone — and
// that drift is half of what made the XLM anchor blind to Soroban.
func TestUSDPegForms_MatchesTheExactTierPegSet(t *testing.T) {
	t.Parallel()
	pegs := []string{usdcClassicPeg}
	forms, err := usdPegForms(pegs, pegSACWrappersFixture)
	if err != nil {
		t.Fatalf("usdPegForms: %v", err)
	}
	want := []string{usdcClassicPeg, usdcSAC}
	if len(forms) != len(want) {
		t.Fatalf("usdPegForms = %q, want %q", forms, want)
	}
	for i := range want {
		if forms[i] != want[i] {
			t.Fatalf("usdPegForms = %q, want %q", forms, want)
		}
	}

	spec, err := NewUSDVolumeQuoteSpec(pegs, pegSACWrappersFixture)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	for _, form := range forms {
		asset, err := canonical.ParseAsset(form)
		if err != nil {
			t.Fatalf("ParseAsset(%q): %v", form, err)
		}
		if _, pegged := spec.QuoteUSDPegInfo(asset); !pegged {
			t.Errorf("usdPegForms bound %q, which the exact tier does NOT treat as a peg", form)
		}
	}
	// The converse: a declared wrapper whose classic is not a peg must
	// stay out. AQUA's SAC is in the fixture precisely to catch a fix
	// that widened the set to "every sac_wrapper".
	for _, form := range forms {
		if form == aquaSAC {
			t.Error("usdPegForms admitted a non-peg classic's SAC wrapper")
		}
	}
}

// TestUSDPegForms_RejectsMalformedWrapper — strict on the WRAPPER, like
// [NewUSDVolumeQuoteSpec]: a wrapper we cannot parse is a peg form we
// would silently never bind, i.e. invisible under-counted volume.
//
// The declared peg strings themselves are deliberately NOT re-validated:
// they are bound verbatim into the SQL exactly as they were before this
// expansion existed, and [NewUSDVolumeQuoteSpec] already rejects an
// unparseable one on the production wiring path.
func TestUSDPegForms_RejectsMalformedWrapper(t *testing.T) {
	t.Parallel()
	if _, err := usdPegForms([]string{usdcClassicPeg}, map[string]string{usdcSAC: "not-an-asset"}); err == nil {
		t.Fatal("expected an error for an unparseable sac_wrapper target")
	}
	forms, err := usdPegForms([]string{"USDC-G..."}, pegSACWrappersFixture)
	if err != nil {
		t.Fatalf("an unrecognised peg spelling must pass through, not error: %v", err)
	}
	if len(forms) != 1 || forms[0] != "USDC-G..." {
		t.Errorf("usdPegForms = %q, want the peg bound verbatim with no expansion", forms)
	}
}
