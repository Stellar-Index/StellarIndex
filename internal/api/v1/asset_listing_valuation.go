// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// A listing-sourced valuation for a verified-catalogue asset whose market cap this index declines
// to publish. It ships as `reference` plus `valuation` blocks and is never written into
// `market_cap_usd`. The listing must name the asset by its classic CODE-GISSUER id or its SAC
// address, never by code, so an impersonator cannot match. It fills only a price hole: a published
// market cap or an observed, gated market price wins, scam-flagged and collision rows get nothing,
// and an unreadable or empty snapshot publishes nothing. Price age: [rwaReferenceStaleAfter]
// labels, [rwaReferenceMaxAge] withholds.

// ─── wire shape ─────────────────────────────────────────────────────

// AssetListingReference is a listing platform's own USD price for this Stellar address, not this index's price.
type AssetListingReference struct {
	// PriceUSD is the platform's USD price, verbatim as the decimal string it published (ADR-0003),
	// never re-rounded or parsed through a float.
	PriceUSD string `json:"price_usd"`
	// Source names the listing platform the row came from.
	Source string `json:"source"`
	// ListingID is that platform's own opaque coin id, not an oracle feed id.
	ListingID string `json:"listing_id"`
	// Quote is always `fiat:USD` on a served row; a price in a reserve asset is a ratio, invisible
	// in the number alone.
	Quote string `json:"quote"`
	// Address is the EXACT Stellar address the platform named, published so a reader can check the
	// join rather than trust it.
	Address string `json:"address"`
	// AddressForm is which route matched: [ListingAddressFormClassic] or [ListingAddressFormSAC].
	// The SAC form is derived: the platform named a contract and this index proved it is the one
	// this (code, issuer) pair deterministically produces.
	AddressForm string `json:"address_form"`
	// AsOf is the PLATFORM's publication time, not when this index read it, so a recent sync
	// cannot launder a price that froze weeks ago.
	AsOf WireTime `json:"as_of"`
	// Stale marks a price older than [rwaReferenceStaleAfter]: served but labelled. Past
	// [rwaReferenceMaxAge] it is not served.
	Stale bool `json:"stale,omitempty"`
	// Provenance names what kind of figure this is; mandatory, never defaulted, so a consumer
	// meeting a second kind cannot mistake it for [RWAReferenceListingPrice].
	Provenance string `json:"provenance"`
}

// AssetListingValuation values circulating supply at the listing platform's price; it is NOT market cap and is never summed into a total without saying so.
type AssetListingValuation struct {
	// Status is the single authority on why there is or is not a figure.
	Status string `json:"status"`
	// ValueUSD is circulating supply (scaled by decimals) times the listing price, a 2-dp decimal
	// string from exact rational arithmetic (ADR-0003). Present iff Status is published.
	ValueUSD *string `json:"value_usd,omitempty"`
	// CirculatingSupply is the raw integer supply ValueUSD was computed from. It can differ from
	// the row's own `circulating_supply`: against a trustline floor this arm takes the LARGER of
	// the row's reading and the lake's flow total.
	CirculatingSupply string `json:"circulating_supply,omitempty"`
	// SupplyBasis names where that number came from: [ListingSupplyBasisLakeFlows] or
	// [ListingSupplyBasisServed]. Most of these assets' float sits in contracts and claimable
	// balances a trustline query cannot see, so the wrong basis gives a valuation hundreds of
	// times too small.
	SupplyBasis string `json:"supply_basis,omitempty"`
}

// AssetListingReference.AddressForm values.
const (
	// ListingAddressFormClassic: the listing named `CODE-GISSUER` itself, with no derivation.
	ListingAddressFormClassic = "classic"
	// ListingAddressFormSAC: the listing named the contract address that
	// canonical.Asset.SacContractID() derives from this asset's (code, issuer).
	ListingAddressFormSAC = "sac"
)

// AssetListingValuation statuses.
const (
	// ListingValuationPublished: a listing price and a supply reading were both available.
	ListingValuationPublished = "published"
	// ListingValuationMarketCapPublished: the row carries an observed `market_cap_usd`, so this arm
	// stands down.
	ListingValuationMarketCapPublished = "market_cap_published"
	// ListingValuationMarketPriceObserved — the row has an observed price but no cap, almost always
	// a missing supply reading that a third party's price would paper over.
	ListingValuationMarketPriceObserved = "market_price_observed"
	// ListingValuationUnavailable: the directory could not be read, is not wired, or holds no fresh
	// rows. "We did not look" is not "nobody lists it", so no conclusion is drawn.
	ListingValuationUnavailable = "listing_unavailable"
	// ListingValuationNotListed: the directory WAS read and names neither the asset's classic id
	// nor its SAC. The ordinary state of almost every asset, not an accusation.
	ListingValuationNotListed = "not_listed"
	// ListingValuationNoListingPrice: the directory names the address but has no price for it, or
	// one past the storage layer's price bound.
	ListingValuationNoListingPrice = "no_listing_price"
	// ListingValuationPriceNotPositive: the price is zero, negative or unparseable.
	ListingValuationPriceNotPositive = "listing_price_not_positive"
	// ListingValuationPriceExpired — the publication time is past [rwaReferenceMaxAge]. Re-checked
	// despite the tighter SQL bound, so the documented bound holds however the row arrived.
	ListingValuationPriceExpired = "listing_price_expired"
	// ListingValuationNoSupply: no supply to multiply. Refused rather than published as zero.
	ListingValuationNoSupply = "no_supply"
)

// AssetListingValuation.SupplyBasis values.
const (
	// ListingSupplyBasisLakeFlows — Σmint − Σburn − Σclawback over the asset's SAC, from the
	// lake's flow log. The only reading not keyed on where tokens are HELD, so it alone sees
	// claimable balances, liquidity-pool reserves and SAC-held balances.
	ListingSupplyBasisLakeFlows = "lake_flows"
	// ListingSupplyBasisServed is the `circulating_supply` already on the row. Used when the row
	// holds an observation (outranks the lake, see [classicSupplyReading]), when its trustline
	// floor is larger, or when the lake could not answer. Every trustline balance was minted, so a
	// lake total below the trustline sum ([higherClassicSupply]) is incomplete seeding.
	ListingSupplyBasisServed = "served"
)

// ─── storage seam ───────────────────────────────────────────────────

// AssetListingDirectoryReader is the seam for the independent listing directory (contract rows differ from [RWAListingDirectoryReader]); optional.
type AssetListingDirectoryReader interface {
	ListingDirectoryByAddress(ctx context.Context) (
		map[string]timescale.ListingEntry, timescale.ListingDirectoryCensus, error)
}

// ─── snapshot ───────────────────────────────────────────────────────

// assetListingSnapshotTTL is a request-rate bound on reusing one directory read, not a freshness
// one: the SQL carries its own 48h and 24h bounds, and this is far shorter so it never delays a
// correction.
const assetListingSnapshotTTL = 60 * time.Second

// assetListingReadBudget bounds one refresh read. It runs detached from the request under the
// cache mutex that every listing request queues on, so it needs its own deadline.
const assetListingReadBudget = 5 * time.Second

// assetListing is one read of the directory, reduced to what this surface may consult.
type assetListing struct {
	// byAddress holds every recognised row, of either address form, keyed by published address.
	byAddress map[string]timescale.ListingEntry
	// census is the storage layer's account of its own numbers, including rows refused as stale.
	census timescale.ListingDirectoryCensus
	// available is false when the read failed, no reader is wired, or no fresh rows exist. Each
	// means "no binding was established", never "this address is not listed".
	available bool
}

// assetListingCache is the process-wide memo for [assetListing].
type assetListingCache struct {
	mu   sync.Mutex
	snap assetListing
	at   time.Time
}

// assetListingSnapshot returns the cached directory read, refreshing it past
// [assetListingSnapshotTTL].
//
// Fails CLOSED like [Server.rwaCuratedSnapshot]: a failed read is cached as unavailable, because
// carrying the last good one forward would keep publishing dollar figures from an unreadable
// directory. The read uses a context detached from the request (a disconnect cannot poison the
// shared cache) with its own deadline (a hung read cannot hold the mutex).
func (s *Server) assetListingSnapshot(ctx context.Context) assetListing {
	return s.assetListingSnapshotWithin(ctx, assetListingReadBudget)
}

func (s *Server) assetListingSnapshotWithin(ctx context.Context, budget time.Duration) assetListing {
	s.assetListings.mu.Lock()
	defer s.assetListings.mu.Unlock()
	if !s.assetListings.at.IsZero() && time.Since(s.assetListings.at) < assetListingSnapshotTTL {
		return s.assetListings.snap
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	// Stamped only after the read returns, so a panicking read leaves the entry expired.
	snap := s.readAssetListing(rctx)
	s.assetListings.snap = snap
	s.assetListings.at = time.Now()
	return snap
}

// readAssetListing performs the one directory read behind the cache.
func (s *Server) readAssetListing(ctx context.Context) assetListing {
	if s.Listings == nil {
		return assetListing{}
	}
	rows, census, err := s.Listings.ListingDirectoryByAddress(ctx)
	if err != nil {
		s.logger.Warn("asset listing directory read failed", "err", err)
		return assetListing{}
	}
	// The storage reader drops stale rows, so an entirely stale table looks empty; treating it as
	// available would mark every asset not_listed when only our copy expired.
	if len(rows) == 0 {
		s.logger.Warn("asset listing directory: no fresh rows",
			"stale", census.Stale, "entries", census.Entries)
		return assetListing{census: census}
	}
	return assetListing{byAddress: rows, census: census, available: true}
}

// ─── the binding ────────────────────────────────────────────────────

// listingAddressesFor returns the two addresses a listing may name this asset by, classic first.
// Native XLM yields only the SAC form, since it has no `CODE-GISSUER` id.
//
// Errors are dropped: the only way to reach one is an asset type with no SAC, which answers like
// an unlisted asset. A Soroban token is its own contract and is served by the RWA contract arm.
func listingAddressesFor(assetID string) (classic, sac string) {
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		return "", ""
	}
	switch asset.Type {
	case canonical.AssetClassic:
		classic = asset.Code + "-" + asset.Issuer
	case canonical.AssetNative:
		// No classic form.
	default:
		return "", ""
	}
	if derived, err := asset.SacContractID(); err == nil {
		sac = derived
	}
	return classic, sac
}

// entryFor resolves the one directory row that names this asset, and which route named it.
// Classic goes first because it is the platform's direct statement; the order only picks the
// published `address_form`.
func (l assetListing) entryFor(assetID string) (timescale.ListingEntry, string, bool) {
	if !l.available {
		return timescale.ListingEntry{}, "", false
	}
	return listingEntryIn(l.byAddress, assetID)
}

// listingEntryIn is the address-resolution rule over any by-address map: classic id first, SAC
// second. It is the one place the rule lives, so /v1/assets and the /v1/rwa/assets classic arm
// cannot disagree on whether a SAC-listed member is bound. A nil map answers "not listed".
func listingEntryIn(byAddress map[string]timescale.ListingEntry, assetID string) (timescale.ListingEntry, string, bool) {
	classic, sac := listingAddressesFor(assetID)
	if classic != "" {
		if e, ok := byAddress[classic]; ok {
			return e, ListingAddressFormClassic, true
		}
	}
	if sac != "" {
		if e, ok := byAddress[sac]; ok {
			return e, ListingAddressFormSAC, true
		}
	}
	return timescale.ListingEntry{}, "", false
}

// ─── the pass ───────────────────────────────────────────────────────

// applyListingValuations attaches a listing-sourced valuation, or the reason there is none, to
// every verified-catalogue row publishing no market cap. Best-effort: any missing input yields
// fewer figures, never a failed response.
func (s *Server) applyListingValuations(ctx context.Context, rows []AssetDetail) {
	if s.VerifiedCurrencies == nil {
		return
	}
	// Narrow to eligible rows before the two reads (directory snapshot, lake-supply lookup).
	candidates := make([]int, 0, 8)
	for i := range rows {
		if s.listingValuationCandidate(&rows[i]) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return
	}

	snap := s.assetListingSnapshot(ctx)
	now := time.Now().UTC()

	// The lake supply read is the expensive half, so it waits until after the address match.
	priced := make([]int, 0, len(candidates))
	for _, i := range candidates {
		entry, form, ok := snap.entryFor(rows[i].AssetID)
		switch {
		case !snap.available:
			rows[i].ListingValuation = &AssetListingValuation{Status: ListingValuationUnavailable}
		case !ok:
			rows[i].ListingValuation = &AssetListingValuation{Status: ListingValuationNotListed}
		default:
			if st := listingPriceRefusal(entry, now); st != "" {
				rows[i].ListingValuation = &AssetListingValuation{Status: st}
				continue
			}
			rows[i].ListingReference = listingReferenceOf(entry, form, now)
			priced = append(priced, i)
		}
	}
	if len(priced) == 0 {
		return
	}

	pending := make([]AssetDetail, 0, len(priced))
	for _, i := range priced {
		pending = append(pending, rows[i])
	}
	lake := s.classicLakeSupply(ctx, pending)
	for _, i := range priced {
		s.publishListingValuation(&rows[i], lake[rows[i].AssetID])
	}
}

// listingValuationCandidate reports whether this row is one this arm may speak about, and records
// the terminal status when it is not. These refusals hold whatever the listing says, so they are
// decided before the directory is read.
func (s *Server) listingValuationCandidate(row *AssetDetail) bool {
	// Not a catalogued identity: no status, since the listing has no standing to price an
	// unattested asset.
	if _, ok := s.VerifiedCurrencies.LookupByStellarAssetID(row.AssetID); !ok {
		return false
	}
	// A scam-flagged or ticker-impersonating row gets no valuation, so this arm cannot re-publish
	// a dollar figure under suppressScamIssuerPricing. Silent: the row's warning fields say why.
	if row.IssuerScamReason != "" {
		return false
	}
	if issuerPricingWithheld(row) {
		return false
	}
	if row.UnverifiedTickerCollision {
		return false
	}
	// An observed market cap is served as is: this arm only fills a hole.
	if row.MarketCapUSD != nil {
		row.ListingValuation = &AssetListingValuation{Status: ListingValuationMarketCapPublished}
		return false
	}
	// A gated price with no cap is a missing supply reading in our data; a third party's number
	// would hide that gap. A non-empty PriceBasis (declared-peg or transitive) is a conversion
	// basis, not an observation, so such a row is still a price hole.
	if row.PriceUSD != nil && row.PriceBasis == "" && !row.MarketCapLowLiquidity && !row.ThinMarket {
		row.ListingValuation = &AssetListingValuation{Status: ListingValuationMarketPriceObserved}
		return false
	}
	return true
}

// listingPriceRefusal returns the status refusing this entry's price, or "" when it may be
// served. The storage reader already blanks a price past its 24-hour bound; the age is re-checked
// against [rwaReferenceMaxAge] so the documented bound holds however the row arrived.
func listingPriceRefusal(entry timescale.ListingEntry, now time.Time) string {
	if entry.PriceUSD == "" {
		return ListingValuationNoListingPrice
	}
	if entry.PricedAt.IsZero() || now.Sub(entry.PricedAt) > rwaReferenceMaxAge {
		return ListingValuationPriceExpired
	}
	// Refused rather than multiplied: the product would be a number nobody claimed.
	if p := ratFromOptionalString(&entry.PriceUSD); p == nil || p.Sign() <= 0 {
		return ListingValuationPriceNotPositive
	}
	return ""
}

// listingReferenceOf projects one directory row onto the wire.
func listingReferenceOf(entry timescale.ListingEntry, form string, now time.Time) *AssetListingReference {
	return &AssetListingReference{
		PriceUSD:    entry.PriceUSD,
		Source:      entry.Source,
		ListingID:   entry.ListingID,
		Quote:       "fiat:USD",
		Address:     entry.Address,
		AddressForm: form,
		AsOf:        WireTime(entry.PricedAt),
		Stale:       now.Sub(entry.PricedAt) > rwaReferenceStaleAfter,
		Provenance:  RWAReferenceListingPrice,
	}
}

// publishListingValuation multiplies the row's supply by the reference price already attached to
// it, or records why it cannot.
//
// `lake` is the net mint-burn-clawback total over the asset's SAC; taking the larger of it and a
// trustline floor is safe because the trustline sum is a provable lower bound. The reading is
// published inside the valuation block and the row's `circulating_supply` is left alone.
func (s *Server) publishListingValuation(row *AssetDetail, lake string) {
	circ, reading := listingSupplyReading(row, lake)
	if circ == "" {
		row.ListingValuation = &AssetListingValuation{Status: ListingValuationNoSupply}
		row.ListingReference = nil
		return
	}
	// Taken from the picker, not by comparing to `lake`, which would mislabel a served figure
	// merely equal to the lake total.
	basis := ListingSupplyBasisServed
	if reading == supply.BasisClassicLakeFlows {
		basis = ListingSupplyBasisLakeFlows
	}
	price := ratFromOptionalString(&row.ListingReference.PriceUSD)
	value := listingValueUSD(circ, row.Decimals, price)
	if value == "" {
		row.ListingValuation = &AssetListingValuation{Status: ListingValuationNoSupply}
		row.ListingReference = nil
		return
	}
	row.ListingValuation = &AssetListingValuation{
		Status:            ListingValuationPublished,
		ValueUSD:          &value,
		CirculatingSupply: circ,
		SupplyBasis:       basis,
	}
}

// listingSupplyReading picks the multiplicand from the row's supply reading and the lake's flow
// total. The lake may replace the row's reading only when that is a provable floor
// ([supply.Basis.LowerBound]) or absent, since [higherClassicSupply] is sound only against a sum
// of minted balances. An observation outranks the lake ([classicSupplyReading]) because the lake
// over-counts replayed mints with missing burns; a reading with no basis is kept too.
func listingSupplyReading(row *AssetDetail, lake string) (string, supply.Basis) {
	if row.CirculatingSupply == nil || *row.CirculatingSupply == "" {
		return higherClassicSupply(lake, "")
	}
	served := *row.CirculatingSupply
	var basis supply.Basis
	if row.SupplyBasis != nil {
		basis = supply.Basis(*row.SupplyBasis)
	}
	if basis.LowerBound() || basis == supply.BasisClassicLakeFlows {
		return higherClassicSupply(lake, served)
	}
	return served, basis
}

// listingValueUSD = (circulating / 10^decimals) x price, as a 2-dp decimal string; empty on any
// input that is not a valuation.
//
// Exact rational arithmetic (ADR-0003), one 2-dp rounding at the end. A negative scale is guarded
// because [big.Int.Exp] answers a negative exponent with 1, which would publish the raw count as
// dollars.
func listingValueUSD(circRaw string, decimals int, price *big.Rat) string {
	if price == nil || decimals < 0 {
		return ""
	}
	circ := ratFromOptionalString(&circRaw)
	// A negative supply is bad data and is refused, not published as a negative valuation.
	if circ == nil || circ.Sign() < 0 {
		return ""
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
	out := new(big.Rat).Quo(circ, scale)
	out.Mul(out, price)
	return out.FloatString(2)
}
