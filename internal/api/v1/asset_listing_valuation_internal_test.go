package v1

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// The listing-priced valuation arm, tested from the two sides that can
// go wrong: the BINDING (whose price ends up on whose row) and the
// HOLE (which rows this arm is allowed to speak about at all).
//
// Every address below is real. That is not decoration: the whole claim
// of this arm is that a third party who never read this repository
// published the same 56 characters the repository derives, and an
// invented strkey cannot express that claim or fail to.

const (
	// The catalogue identity. USDT0 launched on Stellar 2026-09-02 and
	// trades ~$106/day there, so the dust-liquidity guard suppresses its
	// market cap — the exact hole this arm exists to fill.
	tlvUSDT0Asset = "USDT0-GATISXX6BZ6NC7IKQBY37CJD4SOZL3CYZJWXEDG6JVIY4WBS6KXJHN6Q"
	// Its SAC, derived from (code, issuer) + the pubnet passphrase, and
	// the address the upstream publishes under platforms.stellar for
	// coin id "usdt0". Two parties, one string.
	tlvUSDT0SAC = "CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26YF"

	// USDC — the known-answer control for the SAC derivation. The
	// instrument is checked on a case whose answer was already public
	// before it is trusted on one whose answer was not.
	tlvUSDCAsset = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	tlvUSDCSAC   = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

	// EURC is named by the upstream under its CLASSIC id, not its SAC —
	// the other of the two routes, and live proof that supporting one
	// form only would drop half the set.
	tlvEURCAsset = "EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"

	// A USDT0 IMPERSONATOR: the same code, a different real G-account
	// (Aquarius's issuer). It is not in the catalogue and its SAC is a
	// different address, so neither gate can be crossed by wearing the
	// ticker.
	tlvUSDT0Impersonator = "USDT0-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

	// Supply readings for USDT0 on 2026-09-15, and the reason this arm
	// may not use whichever one is to hand: mint−burn over the SAC says
	// 2,581,052.8958550 tokens, the trustline sum says 6,469.52. Valuing
	// the second publishes a figure 400x too small.
	tlvUSDT0LakeSupply      = "25810528958550"
	tlvUSDT0TrustlineSupply = "64695200000"
)

// ─── stubs ──────────────────────────────────────────────────────────

type stubAssetListingDirectory struct {
	rows map[string]timescale.ListingEntry
	err  error
}

func (s *stubAssetListingDirectory) ListingDirectoryByAddress(
	context.Context,
) (map[string]timescale.ListingEntry, timescale.ListingDirectoryCensus, error) {
	if s.err != nil {
		return nil, timescale.ListingDirectoryCensus{}, s.err
	}
	return s.rows, timescale.ListingDirectoryCensus{Entries: len(s.rows)}, nil
}

// stubLakeSupplies answers the bulk contract-supply read the lake path
// uses, keyed by SAC address.
type stubLakeSupplies struct {
	byContract map[string]string
}

func (s *stubLakeSupplies) TokenSupply(context.Context, string) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{}, errors.New("not used")
}

func (s *stubLakeSupplies) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("not used")
}

func (s *stubLakeSupplies) TokenSupplyForContracts(
	_ context.Context, ids []string,
) (map[string]clickhouse.TokenSupply, error) {
	out := make(map[string]clickhouse.TokenSupply, len(ids))
	for _, id := range ids {
		raw, ok := s.byContract[id]
		if !ok {
			continue
		}
		total, _ := new(big.Int).SetString(raw, 10)
		out[id] = clickhouse.TokenSupply{ContractID: id, Total: total}
	}
	return out, nil
}

// tlvEntry builds a directory row with a price published `age` ago.
func tlvEntry(addr, listingID, price string, age time.Duration) timescale.ListingEntry {
	return timescale.ListingEntry{
		Address:   addr,
		ListingID: listingID,
		Symbol:    listingID,
		PriceUSD:  price,
		PricedAt:  time.Now().UTC().Add(-age),
		Source:    "coingecko",
	}
}

// tlvServer wires the catalogue, the listing directory and the lake.
func tlvServer(t *testing.T, listings AssetListingDirectoryReader, lake map[string]string) *Server {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("load catalogue: %v", err)
	}
	return New(Options{
		VerifiedCurrencies: cat,
		Listings:           listings,
		TokenSupply:        &stubLakeSupplies{byContract: lake},
	})
}

// tlvDustSuppressedRow is a row in the state USDT0 is actually served
// in: a price, the dust guard's flag set, and no market cap.
func tlvDustSuppressedRow(assetID, price, servedSupply string) AssetDetail {
	row := AssetDetail{
		AssetID:               assetID,
		Decimals:              7,
		PriceUSD:              &price,
		MarketCapLowLiquidity: true,
	}
	if servedSupply != "" {
		// The shape stampCirculatingSupply leaves when classicSupplyReading
		// fell through to the trustline arm: value and basis travel together.
		stampCirculatingSupply(&row, servedSupply, supply.BasisClassicTrustlineSum)
	}
	return row
}

// TestListingValuation_ObservationIsNeverPromotedOver is #531: the row's
// supply is an ADR-0011 observation, which classicSupplyReading ranks
// ABOVE the lake because the lake over-counts replayed mints (BLND read
// +11.53%). The floor guard that lets the lake replace a trustline sum
// does not apply to an observation, so the valuation must multiply the
// observation the row itself publishes. A reading with no basis is not
// known to be a floor either.
func TestListingValuation_ObservationIsNeverPromotedOver(t *testing.T) {
	const observation = "23000000000000" // below tlvUSDT0LakeSupply
	observed := string(supply.BasisIssuerExclusion)
	for _, basis := range []*string{&observed, nil} {
		s := tlvServer(t,
			&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
				tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1", time.Minute),
			}},
			map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply},
		)
		row := tlvDustSuppressedRow(tlvUSDT0Asset, "1.00", "")
		obs := observation
		row.CirculatingSupply = &obs
		row.SupplyBasis = basis
		row = tlvApply(t, s, []AssetDetail{row})[0]

		if row.ListingValuation == nil || row.ListingValuation.ValueUSD == nil {
			t.Fatalf("basis %v: no figure published: %+v", basis, row.ListingValuation)
		}
		if got := row.ListingValuation.CirculatingSupply; got != observation {
			t.Errorf("basis %v: listing_valuation.circulating_supply = %q, want the observation %q, not the lake %q",
				basis, got, observation, tlvUSDT0LakeSupply)
		}
		if got := *row.ListingValuation.ValueUSD; got != "2300000.00" {
			t.Errorf("basis %v: value_usd = %q, want 2300000.00", basis, got)
		}
		if got := row.ListingValuation.SupplyBasis; got != ListingSupplyBasisServed {
			t.Errorf("basis %v: supply_basis = %q, want %q", basis, got, ListingSupplyBasisServed)
		}
	}
}

func tlvApply(t *testing.T, s *Server, rows []AssetDetail) []AssetDetail {
	t.Helper()
	s.applyListingValuations(t.Context(), rows)
	return rows
}

// ─── the binding ────────────────────────────────────────────────────

// TestListingValuation_SACDerivationMatchesThePublishedAddress checks the
// instrument on a case whose answer was already known before trusting it
// on one whose answer was not.
//
// Both halves matter. USDC's SAC is public and long-standing, so a
// derivation that reproduces it is working; USDT0's is the address this
// arm is about to bind a price to.
func TestListingValuation_SACDerivationMatchesThePublishedAddress(t *testing.T) {
	for _, tc := range []struct{ assetID, wantSAC string }{
		{tlvUSDCAsset, tlvUSDCSAC},
		{tlvUSDT0Asset, tlvUSDT0SAC},
	} {
		classic, sac := listingAddressesFor(tc.assetID)
		if classic != tc.assetID {
			t.Errorf("%s: classic form = %q, want the asset id itself", tc.assetID, classic)
		}
		if sac != tc.wantSAC {
			t.Errorf("%s: SAC = %q, want %q", tc.assetID, sac, tc.wantSAC)
		}
	}
	// Native XLM has no classic form and must not invent one.
	classic, sac := listingAddressesFor("native")
	if classic != "" {
		t.Errorf("native classic form = %q, want empty", classic)
	}
	if sac != canonical.XLMSacContractID {
		t.Errorf("native SAC = %q, want %q", sac, canonical.XLMSacContractID)
	}
}

// TestListingValuation_ImpersonatorDerivesADifferentAddress is the
// reason the binding is on the address and never on the code.
func TestListingValuation_ImpersonatorDerivesADifferentAddress(t *testing.T) {
	_, fake := listingAddressesFor(tlvUSDT0Impersonator)
	if fake == "" {
		t.Fatal("impersonator derived no SAC at all")
	}
	if fake == tlvUSDT0SAC {
		t.Fatalf("impersonator derived the real USDT0 SAC %q — SAC derivation is not issuer-bound", fake)
	}
}

// TestListingValuation_DustSuppressedCatalogueAssetGainsAFigure is the
// unlock, and it pins every property at once: the market cap stays
// withheld, the dust flag stays set, and the new figure appears under
// its own name with the listing's provenance on it.
func TestListingValuation_DustSuppressedCatalogueAssetGainsAFigure(t *testing.T) {
	s := tlvServer(t,
		&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
			tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "0.999383", 5*time.Minute),
		}},
		map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply},
	)
	row := tlvApply(t, s, []AssetDetail{
		tlvDustSuppressedRow(tlvUSDT0Asset, "0.9993830000", tlvUSDT0TrustlineSupply),
	})[0]

	if row.MarketCapUSD != nil {
		t.Fatalf("market_cap_usd = %q — this arm must never publish one", *row.MarketCapUSD)
	}
	if !row.MarketCapLowLiquidity {
		t.Error("market_cap_low_liquidity was cleared; the dust guard's verdict must stand")
	}
	if row.ListingValuation == nil || row.ListingValuation.Status != ListingValuationPublished {
		t.Fatalf("listing_valuation = %+v, want status %q", row.ListingValuation, ListingValuationPublished)
	}
	// 25810528958550 / 10^7 x 0.999383 = 2,579,460.39 (2 dp, exact
	// rational arithmetic — never a float).
	want := "2579460.39"
	if got := *row.ListingValuation.ValueUSD; got != want {
		t.Errorf("value_usd = %q, want %q", got, want)
	}
	if row.ListingValuation.SupplyBasis != ListingSupplyBasisLakeFlows {
		t.Errorf("supply_basis = %q, want %q", row.ListingValuation.SupplyBasis, ListingSupplyBasisLakeFlows)
	}
	ref := row.ListingReference
	if ref == nil {
		t.Fatal("no listing_reference beside the figure — a dollar total with no traceable source")
	}
	if ref.Address != tlvUSDT0SAC || ref.AddressForm != ListingAddressFormSAC {
		t.Errorf("reference address = %q/%q, want %q/%q",
			ref.Address, ref.AddressForm, tlvUSDT0SAC, ListingAddressFormSAC)
	}
	if ref.Provenance != RWAReferenceListingPrice {
		t.Errorf("provenance = %q, want %q", ref.Provenance, RWAReferenceListingPrice)
	}
	if ref.Quote != "fiat:USD" || ref.ListingID != "usdt0" || ref.Source != "coingecko" {
		t.Errorf("reference = %+v, want quote fiat:USD / listing_id usdt0 / source coingecko", ref)
	}
	if ref.Stale {
		t.Error("a five-minute-old price is not stale")
	}
}

// TestListingValuation_TrustlineSupplyNeverWins is the bug this arm
// would have shipped without the lake read: USDT0 holds almost all of
// its float outside trustlines, so the reading a trustline query returns
// is 400x too small and entirely plausible-looking.
func TestListingValuation_TrustlineSupplyNeverWins(t *testing.T) {
	s := tlvServer(t,
		&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
			tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1", time.Minute),
		}},
		map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply},
	)
	row := tlvApply(t, s, []AssetDetail{
		tlvDustSuppressedRow(tlvUSDT0Asset, "1.00", tlvUSDT0TrustlineSupply),
	})[0]

	if row.ListingValuation == nil || row.ListingValuation.ValueUSD == nil {
		t.Fatalf("no figure published: %+v", row.ListingValuation)
	}
	if got := *row.ListingValuation.ValueUSD; got != "2581052.90" {
		t.Fatalf("value_usd = %q, want 2581052.90 (the mint−burn total, not the 6,469.52 trustline sum)", got)
	}
	// The multiplicand is published inside the block, so the figure is
	// self-describing — and the row's own supply field is left exactly
	// as the other producers left it.
	if got := row.ListingValuation.CirculatingSupply; got != tlvUSDT0LakeSupply {
		t.Errorf("listing_valuation.circulating_supply = %q, want the lake reading %q", got, tlvUSDT0LakeSupply)
	}
	if row.CirculatingSupply == nil || *row.CirculatingSupply != tlvUSDT0TrustlineSupply {
		t.Errorf("circulating_supply = %v, want it untouched at %q", row.CirculatingSupply, tlvUSDT0TrustlineSupply)
	}
}

// TestListingValuation_ClassicRouteMatches covers the other half of the
// binding: an asset the upstream names by its `CODE-GISSUER` id.
func TestListingValuation_ClassicRouteMatches(t *testing.T) {
	s := tlvServer(t,
		&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
			tlvEURCAsset: tlvEntry(tlvEURCAsset, "euro-coin", "1.17", time.Hour),
		}},
		map[string]string{},
	)
	row := tlvApply(t, s, []AssetDetail{
		tlvDustSuppressedRow(tlvEURCAsset, "1.14", "31183489179226"),
	})[0]

	if row.ListingReference == nil || row.ListingReference.AddressForm != ListingAddressFormClassic {
		t.Fatalf("listing_reference = %+v, want address_form %q", row.ListingReference, ListingAddressFormClassic)
	}
	if row.ListingValuation.SupplyBasis != ListingSupplyBasisServed {
		t.Errorf("supply_basis = %q, want %q when the lake cannot answer",
			row.ListingValuation.SupplyBasis, ListingSupplyBasisServed)
	}
	// 31183489179226 / 10^7 x 1.17 = 3,648,468.23
	if got := *row.ListingValuation.ValueUSD; got != "3648468.23" {
		t.Errorf("value_usd = %q, want 3648468.23", got)
	}
}

// ─── the hole ───────────────────────────────────────────────────────

// TestListingValuation_ServedMarketCapIsNeverReplaced is the negative
// control the whole design rests on. Where the existing gates DO publish
// a market cap, nothing changes.
func TestListingValuation_ServedMarketCapIsNeverReplaced(t *testing.T) {
	marketCap := "366239564.03"
	price := "1.0000523527"
	supply := "3662203914017012"
	s := tlvServer(t,
		&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
			// A wildly different price, so a replacement would be
			// unmistakable rather than a rounding difference.
			tlvUSDCSAC: tlvEntry(tlvUSDCSAC, "usd-coin", "5.00", time.Minute),
		}},
		map[string]string{tlvUSDCSAC: "9999999999999999"},
	)
	row := tlvApply(t, s, []AssetDetail{{
		AssetID:           tlvUSDCAsset,
		Decimals:          7,
		PriceUSD:          &price,
		MarketCapUSD:      &marketCap,
		CirculatingSupply: &supply,
	}})[0]

	if *row.MarketCapUSD != marketCap {
		t.Fatalf("market_cap_usd = %q, want it untouched at %q", *row.MarketCapUSD, marketCap)
	}
	if *row.CirculatingSupply != supply {
		t.Errorf("circulating_supply = %q, want it untouched at %q", *row.CirculatingSupply, supply)
	}
	if row.ListingReference != nil {
		t.Errorf("listing_reference = %+v, want none beside a served market cap", row.ListingReference)
	}
	if row.ListingValuation == nil || row.ListingValuation.Status != ListingValuationMarketCapPublished {
		t.Fatalf("listing_valuation = %+v, want status %q", row.ListingValuation, ListingValuationMarketCapPublished)
	}
	if row.ListingValuation.ValueUSD != nil {
		t.Errorf("value_usd = %q, want none", *row.ListingValuation.ValueUSD)
	}
}

// TestListingValuation_ObservedPriceWithoutSupplyStandsDown is the
// subtler half of the same rule. A catalogue row carrying a gated,
// observed market price and no cap is missing a SUPPLY reading, not a
// price — and filling it from a third party would hide a gap in this
// index's own data behind somebody else's number.
func TestListingValuation_ObservedPriceWithoutSupplyStandsDown(t *testing.T) {
	price := "0.0036550258"
	s := tlvServer(t,
		&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
			"SHX-GDSTRSHXHGJ7ZIVRBXEYE5Q74XUVCUSEKEBR7UCHEUUEK72N7I7KJ6JH": tlvEntry(
				"SHX-GDSTRSHXHGJ7ZIVRBXEYE5Q74XUVCUSEKEBR7UCHEUUEK72N7I7KJ6JH",
				"stronghold-token", "0.0037", time.Minute),
		}},
		map[string]string{},
	)
	row := tlvApply(t, s, []AssetDetail{{
		AssetID:  "SHX-GDSTRSHXHGJ7ZIVRBXEYE5Q74XUVCUSEKEBR7UCHEUUEK72N7I7KJ6JH",
		Decimals: 7,
		PriceUSD: &price,
	}})[0]

	if row.ListingValuation == nil || row.ListingValuation.Status != ListingValuationMarketPriceObserved {
		t.Fatalf("listing_valuation = %+v, want status %q", row.ListingValuation, ListingValuationMarketPriceObserved)
	}
	if row.ListingReference != nil {
		t.Errorf("listing_reference = %+v, want none", row.ListingReference)
	}
}

// TestListingValuation_DeclaredPegPriceIsStillAHole — a peg-filled price
// is a conversion basis, not a market observation, so it does not stand
// this arm down the way a gated market price does.
func TestListingValuation_DeclaredPegPriceIsStillAHole(t *testing.T) {
	price := "1.0000000000"
	s := tlvServer(t,
		&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
			tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "0.999383", time.Minute),
		}},
		map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply},
	)
	row := tlvApply(t, s, []AssetDetail{{
		AssetID:    tlvUSDT0Asset,
		Decimals:   7,
		PriceUSD:   &price,
		PriceBasis: priceBasisDeclaredPeg,
	}})[0]

	if row.ListingValuation == nil || row.ListingValuation.Status != ListingValuationPublished {
		t.Fatalf("listing_valuation = %+v, want status %q", row.ListingValuation, ListingValuationPublished)
	}
}

// TestListingValuation_UncataloguedRowsAreUntouched — an impersonator
// wearing a catalogued ticker, with the directory naming the REAL
// identity's addresses, gets nothing. Neither block appears at all, so
// the long tail of the listing is unchanged byte for byte.
func TestListingValuation_UncataloguedRowsAreUntouched(t *testing.T) {
	price := "0.5"
	s := tlvServer(t,
		&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
			tlvUSDT0SAC:   tlvEntry(tlvUSDT0SAC, "usdt0", "0.999383", time.Minute),
			tlvUSDT0Asset: tlvEntry(tlvUSDT0Asset, "usdt0", "0.999383", time.Minute),
		}},
		map[string]string{},
	)
	rows := tlvApply(t, s, []AssetDetail{
		tlvDustSuppressedRow(tlvUSDT0Impersonator, price, "920000000000000000"),
		// A collision-flagged row is refused even if it somehow were
		// catalogued.
		func() AssetDetail {
			r := tlvDustSuppressedRow(tlvUSDT0Asset, price, "1")
			r.UnverifiedTickerCollision = true
			return r
		}(),
		// A scam-flagged issuer publishes no valuation of any kind.
		func() AssetDetail {
			r := tlvDustSuppressedRow(tlvUSDT0Asset, price, "1")
			r.IssuerScamReason = "curated scam list"
			return r
		}(),
	})
	for i, row := range rows {
		if row.ListingReference != nil || row.ListingValuation != nil {
			t.Errorf("row %d: got reference=%+v valuation=%+v, want neither", i, row.ListingReference, row.ListingValuation)
		}
	}
}

// ─── failing closed ─────────────────────────────────────────────────

// TestListingValuation_FailsClosed covers both ways the snapshot can
// stop answering. Neither publishes a figure, and both say "we did not
// look" rather than "nobody lists it".
func TestListingValuation_FailsClosed(t *testing.T) {
	for name, listings := range map[string]AssetListingDirectoryReader{
		"read error":     &stubAssetListingDirectory{err: errors.New("connection refused")},
		"no fresh rows":  &stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{}},
		"reader unwired": nil,
	} {
		t.Run(name, func(t *testing.T) {
			s := tlvServer(t, listings, map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply})
			if listings == nil {
				s.listings = nil
			}
			row := tlvApply(t, s, []AssetDetail{
				tlvDustSuppressedRow(tlvUSDT0Asset, "0.99", tlvUSDT0TrustlineSupply),
			})[0]
			if row.ListingValuation == nil || row.ListingValuation.Status != ListingValuationUnavailable {
				t.Fatalf("listing_valuation = %+v, want status %q", row.ListingValuation, ListingValuationUnavailable)
			}
			if row.ListingReference != nil || row.ListingValuation.ValueUSD != nil {
				t.Error("published something from a directory nobody could read")
			}
		})
	}
}

// TestListingValuation_NotListedIsDistinctFromUnavailable — the
// directory answered and does not name this asset. That is a finding,
// and it must not wear the outage's status.
func TestListingValuation_NotListedIsDistinctFromUnavailable(t *testing.T) {
	s := tlvServer(t,
		&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
			tlvUSDCSAC: tlvEntry(tlvUSDCSAC, "usd-coin", "1.00", time.Minute),
		}},
		map[string]string{},
	)
	// PHO is a catalogue asset the upstream does not list at all.
	row := tlvApply(t, s, []AssetDetail{
		tlvDustSuppressedRow("PHO-GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO", "0.0109", "778827871496573"),
	})[0]
	if row.ListingValuation == nil || row.ListingValuation.Status != ListingValuationNotListed {
		t.Fatalf("listing_valuation = %+v, want status %q", row.ListingValuation, ListingValuationNotListed)
	}
}

// TestListingValuation_PriceAgeBounds — the two bounds are the RWA
// reference arm's, reused rather than re-invented: 72h LABELS, 7d
// WITHHOLDS.
func TestListingValuation_PriceAgeBounds(t *testing.T) {
	t.Run("stale but served", func(t *testing.T) {
		s := tlvServer(t,
			&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
				tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1", rwaReferenceStaleAfter+time.Hour),
			}},
			map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply},
		)
		row := tlvApply(t, s, []AssetDetail{tlvDustSuppressedRow(tlvUSDT0Asset, "0.99", "")})[0]
		if row.ListingReference == nil || !row.ListingReference.Stale {
			t.Fatalf("listing_reference = %+v, want stale:true", row.ListingReference)
		}
		if row.ListingValuation.Status != ListingValuationPublished {
			t.Errorf("status = %q, want %q — stale LABELS, it does not withhold",
				row.ListingValuation.Status, ListingValuationPublished)
		}
	})
	t.Run("expired and withheld", func(t *testing.T) {
		s := tlvServer(t,
			&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
				tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1", rwaReferenceMaxAge+time.Hour),
			}},
			map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply},
		)
		row := tlvApply(t, s, []AssetDetail{tlvDustSuppressedRow(tlvUSDT0Asset, "0.99", "")})[0]
		if row.ListingValuation == nil || row.ListingValuation.Status != ListingValuationPriceExpired {
			t.Fatalf("listing_valuation = %+v, want status %q", row.ListingValuation, ListingValuationPriceExpired)
		}
		if row.ListingReference != nil {
			t.Error("served a reference for a price past the absolute bound")
		}
	})
}

// TestListingValuation_NoPriceAndNoSupplyAreSeparateRefusals.
func TestListingValuation_NoPriceAndNoSupplyAreSeparateRefusals(t *testing.T) {
	t.Run("named but unpriced", func(t *testing.T) {
		s := tlvServer(t,
			&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
				tlvUSDT0SAC: {Address: tlvUSDT0SAC, ListingID: "usdt0", Source: "coingecko"},
			}},
			map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply},
		)
		row := tlvApply(t, s, []AssetDetail{tlvDustSuppressedRow(tlvUSDT0Asset, "0.99", "")})[0]
		if row.ListingValuation.Status != ListingValuationNoListingPrice {
			t.Fatalf("status = %q, want %q", row.ListingValuation.Status, ListingValuationNoListingPrice)
		}
	})
	t.Run("priced but no supply anywhere", func(t *testing.T) {
		s := tlvServer(t,
			&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
				tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1", time.Minute),
			}},
			map[string]string{},
		)
		row := tlvApply(t, s, []AssetDetail{tlvDustSuppressedRow(tlvUSDT0Asset, "0.99", "")})[0]
		if row.ListingValuation.Status != ListingValuationNoSupply {
			t.Fatalf("status = %q, want %q", row.ListingValuation.Status, ListingValuationNoSupply)
		}
		if row.ListingReference != nil {
			t.Error("a reference with no valuation is a price on the wire with no statement of its use")
		}
	})
	t.Run("non-positive price", func(t *testing.T) {
		s := tlvServer(t,
			&stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
				tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "0", time.Minute),
			}},
			map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply},
		)
		row := tlvApply(t, s, []AssetDetail{tlvDustSuppressedRow(tlvUSDT0Asset, "0.99", "")})[0]
		if row.ListingValuation.Status != ListingValuationPriceNotPositive {
			t.Fatalf("status = %q, want %q", row.ListingValuation.Status, ListingValuationPriceNotPositive)
		}
	})
}

// ─── arithmetic ─────────────────────────────────────────────────────

// TestListingValueUSD_IsExact — the product is exact rational
// arithmetic with a single rounding at the end, so a consumer can add
// the served strings by hand and reach the same total.
func TestListingValueUSD_IsExact(t *testing.T) {
	for _, tc := range []struct {
		name     string
		circ     string
		decimals int
		price    string
		want     string
	}{
		{"usdt0 at one dollar", tlvUSDT0LakeSupply, 7, "1", "2581052.90"},
		{"a price a float would round", "1", 0, "0.1", "0.10"},
		{"eighteen decimals", "1000000000000000000", 18, "1234.56", "1234.56"},
		{"zero decimals is a scale, not a bug", "5", 0, "2", "10.00"},
		{"negative supply is refused", "-1", 7, "1", ""},
		{"a negative scale is not a scale", "1", -1, "1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			price, _ := new(big.Rat).SetString(tc.price)
			if got := listingValueUSD(tc.circ, tc.decimals, price); got != tc.want {
				t.Errorf("listingValueUSD(%q, %d, %s) = %q, want %q", tc.circ, tc.decimals, tc.price, got, tc.want)
			}
		})
	}
	if got := listingValueUSD("1", 7, nil); got != "" {
		t.Errorf("a nil price produced %q", got)
	}
}
