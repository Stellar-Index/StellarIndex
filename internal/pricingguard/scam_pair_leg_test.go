package pricingguard

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Pair-leg regression suite for F002/K001.
//
// The gate used to answer a question about ONE asset, which made the
// withholding decision depend on which leg the client named first: the
// same market, asked the other way round, was published at 200 as the
// exact reciprocal of the number the gate had just refused. These tests
// pin the decision as a property of the MARKET.

// pairLegDirectory flags exactly the listed G-addresses and records
// every address it was asked about.
type pairLegDirectory struct {
	flagged map[string]bool
	asked   []string
}

func (d *pairLegDirectory) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	d.asked = append(d.asked, address)
	if d.flagged[address] {
		return timescale.DirectoryEntry{Address: address, Tags: []string{"unsafe"}}, true, nil
	}
	return timescale.DirectoryEntry{}, false, nil
}

const (
	pairLegFlaggedIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	pairLegCleanIssuer   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

func pairLegAssets(t *testing.T) (flagged, clean, native canonical.Asset) {
	t.Helper()
	flagged, err := canonical.NewClassicAsset("RIO", pairLegFlaggedIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	clean, err = canonical.NewClassicAsset("USDC", pairLegCleanIssuer)
	if err != nil {
		t.Fatalf("build clean classic: %v", err)
	}
	return flagged, clean, canonical.NativeAsset()
}

func TestScamGateWithheldPair_EitherLeg(t *testing.T) {
	flagged, clean, native := pairLegAssets(t)
	ctx := context.Background()

	t.Run("flagged quote leg withholds", func(t *testing.T) {
		dir := &pairLegDirectory{flagged: map[string]bool{pairLegFlaggedIssuer: true}}
		g := NewScamGate(dir, ScamGateOptions{})
		if !g.WithheldPair(ctx, native, flagged, "vwap") {
			t.Fatal("native/<flagged> was served — the price of XLM in a flagged issuer's " +
				"asset is that issuer's own withheld market price, inverted")
		}
		if len(dir.asked) == 0 || dir.asked[0] != pairLegFlaggedIssuer {
			t.Errorf("directory asked about %v, want the quote leg's issuer %q — "+
				"the lookup, not the verdict, is what proves the leg was inspected",
				dir.asked, pairLegFlaggedIssuer)
		}
	})

	t.Run("flagged base leg still withholds", func(t *testing.T) {
		dir := &pairLegDirectory{flagged: map[string]bool{pairLegFlaggedIssuer: true}}
		g := NewScamGate(dir, ScamGateOptions{})
		if !g.WithheldPair(ctx, flagged, native, "vwap") {
			t.Fatal("<flagged>/native was served — the pair fold must not have lost the base leg")
		}
		// Short-circuit: a flagged base costs the one lookup it always did.
		if len(dir.asked) != 1 {
			t.Errorf("directory asked %d times (%v), want 1 — the fold must short-circuit "+
				"on the base so a flagged pair costs no extra lookup", len(dir.asked), dir.asked)
		}
	})

	t.Run("neither leg flagged serves", func(t *testing.T) {
		dir := &pairLegDirectory{flagged: map[string]bool{}}
		g := NewScamGate(dir, ScamGateOptions{})
		if g.WithheldPair(ctx, clean, native, "vwap") {
			t.Error("an unflagged pair was withheld — blast radius: this would blank " +
				"ordinary markets on every gated surface")
		}
		if g.WithheldPair(ctx, native, clean, "vwap") {
			t.Error("an unflagged pair was withheld in the inverted orientation")
		}
	})

	t.Run("nil gate withholds nothing", func(t *testing.T) {
		var g *ScamGate
		if g.WithheldPair(ctx, flagged, native, "vwap") {
			t.Error("a nil gate (operator has no directory table) must withhold nothing")
		}
	})
}

// TestScamGateWithheldPair_ResolvesSACOnTheQuoteLeg — the canonical
// family resolution must apply to the quote leg too. It lives inside
// withheldLeg precisely so neither leg can be the ungated spelling: a
// fold that resolved only the base would re-open the R8 SAC bypass one
// orientation at a time.
//
// NOT parallel: the alias registry is process-global.
func TestScamGateWithheldPair_ResolvesSACOnTheQuoteLeg(t *testing.T) {
	flagged, _, native := pairLegAssets(t)

	sacID, err := flagged.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	sac, err := canonical.NewSorobanAsset(sacID)
	if err != nil {
		t.Fatalf("soroban asset: %v", err)
	}
	reg, err := canonical.NewAliasRegistry(map[string]string{sacID: "RIO:" + pairLegFlaggedIssuer})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	dir := &pairLegDirectory{flagged: map[string]bool{pairLegFlaggedIssuer: true}}
	g := NewScamGate(dir, ScamGateOptions{})

	if !g.WithheldPair(context.Background(), native, sac, "vwap") {
		t.Fatal("the SAC spelling of a flagged issuance on the QUOTE leg was served — " +
			"the contract id must not be a second, ungated way to name the market")
	}
	if len(dir.asked) == 0 || dir.asked[0] != pairLegFlaggedIssuer {
		t.Errorf("directory asked about %v, want the wrapped issuance's issuer %q",
			dir.asked, pairLegFlaggedIssuer)
	}
}

// TestPriceWithheldChokepoint pins the shared decision both binaries
// import: substance refusal OR either flagged leg, and nil gates allow.
func TestPriceWithheldChokepoint(t *testing.T) {
	flagged, _, native := pairLegAssets(t)
	ctx := context.Background()

	if (Gate{}).PriceWithheld(ctx, native, flagged, "price_alert") {
		t.Error("nil gates must allow — a disabled [pricing_guard] must not withhold every price")
	}

	dir := &pairLegDirectory{flagged: map[string]bool{pairLegFlaggedIssuer: true}}
	g := NewScamGate(dir, ScamGateOptions{})
	if !(Gate{Scam: g}).PriceWithheld(ctx, native, flagged, "price_alert") {
		t.Error("the chokepoint served a pair whose QUOTE leg is directory-flagged — " +
			"every consumer of this function inherits that hole")
	}
	if (Gate{Scam: g}).PriceWithheld(ctx, native, canonical.NativeAsset(), "price_alert") {
		t.Error("the chokepoint withheld a pair with no flagged leg")
	}
}
