// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/StellarIndex/stellar-index/internal/aggregate"
	"github.com/StellarIndex/stellar-index/internal/currency"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// Verified-issuer strings mirror internal/currency/verified_test.go.
const (
	realUSDCIssuer   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	fakeIssuerStrkey = "GBADISSUERSOMETHINGTHATWILLNEVERMATCHANYREALACCOUNTAB"
)

// TestStampListingCollisions pins the FIX-1 per-row trust signal: the
// listing serves COALESCE(slug, code) AS slug, so a NULL-slug
// impersonator emits the verified asset's CODE as its slug. The
// per-row unverified_ticker_collision flag must fire on the
// impersonator (so the explorer withholds the "verified" badge) and
// must NOT fire on the real verified row or on a code no verified
// currency claims.
func TestStampListingCollisions(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	s := &Server{verifiedCurrencies: cat}

	realIss := realUSDCIssuer
	fakeIss := fakeIssuerStrkey
	rows := []AssetDetail{
		{Code: "USDC", Issuer: &realIss, Slug: "USDC"},              // the real verified Circle USDC
		{Code: "USDC", Issuer: &fakeIss, Slug: "USDC"},              // impersonator sharing the ticker
		{Code: "NOTAVERIFIEDCODE", Issuer: &fakeIss, Slug: "NOTA…"}, // code no verified currency claims
		{Code: "XLM", Slug: "XLM"},                                  // catalogue/global row, no issuer
	}
	s.stampListingCollisions(rows)

	if rows[0].UnverifiedTickerCollision {
		t.Error("real verified USDC row was flagged as a collision — it would lose its badge")
	}
	if !rows[1].UnverifiedTickerCollision {
		t.Error("impersonator USDC row was NOT flagged — it would wear a false verified badge")
	}
	if rows[2].UnverifiedTickerCollision {
		t.Error("non-verified code was flagged — false positive")
	}
	if rows[3].UnverifiedTickerCollision {
		t.Error("issuer-less (catalogue/global) row was flagged")
	}
}

// TestStampListingCollisionsNilCatalogue guards the no-catalogue
// deployment: stamping must be a safe no-op.
func TestStampListingCollisionsNilCatalogue(t *testing.T) {
	s := &Server{}
	fakeIss := fakeIssuerStrkey
	rows := []AssetDetail{{Code: "USDC", Issuer: &fakeIss, Slug: "USDC"}}
	s.stampListingCollisions(rows) // must not panic
	if rows[0].UnverifiedTickerCollision {
		t.Error("collision flagged with no catalogue wired")
	}
}

// TestApplyAssetRowYieldsToCanonicalPrice pins the FIX-2 precedence: the
// asset-catalogue overlay (listing query, which for native mixes SDEX + CEX pairs)
// must NOT clobber a PriceUSD already set by the canonical /v1/price
// reader (F2 populatePriceUSD → lookupUSDPrice, run earlier). It fills
// only when that canonical value is absent.
func TestApplyAssetRowYieldsToCanonicalPrice(t *testing.T) {
	s := &Server{}
	rowPrice := "0.2012011674"
	row := timescale.AssetRow{AssetID: "native", PriceUSD: &rowPrice}

	// Canonical price already set — asset-catalogue overlay must yield.
	canonicalPrice := "0.20114638079663692765"
	detail := &AssetDetail{PriceUSD: &canonicalPrice}
	s.applyAssetRowToDetail(detail, row, nil, "native")
	if detail.PriceUSD == nil || *detail.PriceUSD != canonicalPrice {
		t.Fatalf("asset-catalogue overlay overwrote the canonical price: got %v, want %q",
			detail.PriceUSD, canonicalPrice)
	}

	// No canonical price — asset-catalogue overlay fills the long tail (SHX, AQUA…).
	detail2 := &AssetDetail{}
	s.applyAssetRowToDetail(detail2, row, nil, "native")
	if detail2.PriceUSD == nil || *detail2.PriceUSD != rowPrice {
		t.Fatalf("asset-catalogue overlay did not fill an absent price: got %v, want %q",
			detail2.PriceUSD, rowPrice)
	}
}

// fallbackAssets is an AssetsReader stub that prices exactly one asset_id
// via GetAssetByAssetID and returns a no-price row for everything else.
type fallbackAssets struct {
	AssetsReader
	assetID string
	price   string
}

func (c *fallbackAssets) GetAssetByAssetID(_ context.Context, assetID string) (timescale.AssetRow, error) {
	if assetID == c.assetID {
		p := c.price
		return timescale.AssetRow{AssetID: assetID, PriceUSD: &p}, nil
	}
	return timescale.AssetRow{AssetID: assetID}, nil
}

// TestGlobalAssetViewOnChainFallback pins FIX-3: when the global
// CEX/aggregator tier produces no price for a Stellar-only verified
// token (globalPrice nil here mimics the miss), buildGlobalAssetView
// falls back to the on-chain per-Stellar-asset price the /v1/assets
// listing shows, with VWAPNative authority + a stellar_onchain source
// marker (honest disclosure, not a fabricated value).
func TestGlobalAssetViewOnChainFallback(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	// Pick any crypto/stablecoin catalogue entry that has a Stellar
	// issuance (AQUA / SHX / yXLM …). Skip if the seed changed shape.
	var vc *currency.VerifiedCurrency
	for _, slug := range []string{"aqua", "shx", "yxlm", "velo", "blnd", "pho"} {
		if v, ok := cat.LookupBySlug(slug); ok && v.StellarEntry() != nil && v.StellarEntry().AssetID != "" {
			vc = v
			break
		}
	}
	if vc == nil {
		t.Skip("no Stellar-issued crypto catalogue entry available")
	}
	stellarID := vc.StellarEntry().AssetID
	price := "0.0039386011"
	s := &Server{
		verifiedCurrencies: cat,
		assetsReader:       &fallbackAssets{assetID: stellarID, price: price},
		// globalPrice left nil → the CEX/aggregator tier misses.
	}

	view := s.buildGlobalAssetView(context.Background(), vc)
	if view.PriceUSD == nil {
		t.Fatalf("global view price_usd stayed null — on-chain fallback did not fire (asset %s)", stellarID)
	}
	if *view.PriceUSD != price {
		t.Errorf("fallback price = %q, want %q (the listing/on-chain value)", *view.PriceUSD, price)
	}
	if view.PriceAuthority != aggregate.AuthorityVWAPNative {
		t.Errorf("fallback authority = %q, want %q", view.PriceAuthority, aggregate.AuthorityVWAPNative)
	}
	if len(view.PriceSources) != 1 || view.PriceSources[0] != "stellar_onchain" {
		t.Errorf("fallback sources = %v, want [stellar_onchain]", view.PriceSources)
	}
}

// TestGlobalAssetViewNoFabricationWhenOffChain guards against
// fabricating a price: when the on-chain twin also has no price, the
// global view stays null rather than inventing one.
func TestGlobalAssetViewNoFabricationWhenOffChain(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	var vc *currency.VerifiedCurrency
	for _, slug := range []string{"aqua", "shx", "yxlm"} {
		if v, ok := cat.LookupBySlug(slug); ok && v.StellarEntry() != nil && v.StellarEntry().AssetID != "" {
			vc = v
			break
		}
	}
	if vc == nil {
		t.Skip("no Stellar-issued crypto catalogue entry available")
	}
	// assetID mismatch → the stub returns a no-price row for the twin.
	s := &Server{
		verifiedCurrencies: cat,
		assetsReader:       &fallbackAssets{assetID: "SOMETHING-ELSE", price: "1.23"},
	}
	view := s.buildGlobalAssetView(context.Background(), vc)
	if view.PriceUSD != nil {
		t.Errorf("global view fabricated a price %q with no on-chain data", *view.PriceUSD)
	}
}
