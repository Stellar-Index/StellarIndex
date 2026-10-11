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
// to publish (e.g. USDT0, whose thin market trips the dust-liquidity guard). Like the RWA surface
// it ships as `reference` plus `valuation` blocks and is never written into `market_cap_usd`.
//
// The listing must name the asset by its classic CODE-GISSUER id or its SAC address, never by
// code, so an impersonator's contract cannot match; verified-catalogue membership is a second
// gate. [Server.listingValuationFor] then fills only a price hole:
//
//   - a published market cap wins ([ListingValuationMarketCapPublished]);
//   - an observed, substance-gated market price means the hole is in our own data, not the
//     price ([ListingValuationMarketPriceObserved]); a declared-peg or transitive `price_basis`
//     is not an observation;
//   - scam-flagged and unverified-collision rows get no valuation at all.
//
// An unreadable or empty snapshot publishes nothing ([ListingValuationUnavailable]). Price age:
// [rwaReferenceStaleAfter] labels, [rwaReferenceMaxAge] withholds.

// ─── wire shape ─────────────────────────────────────────────────────

// AssetListingReference is an independent listing platform's own USD price for the exact Stellar
// address this asset lives at. It is NOT this index's price and not derived from any Stellar
// market; the asset's own `price_usd` sits beside it unchanged.
type AssetListingReference struct {
	// PriceUSD is the platform's published USD price, verbatim as the
	// decimal string it published (ADR-0003). Never re-rounded and never
	// parsed through a float at any point on the path from the wire to
	// here.
	PriceUSD string `json:"price_usd"`
	// Source names the listing platform the row came from.
	Source string `json:"source"`
	// ListingID is that platform's own opaque coin id, so a reader can
	// pull the same figure from the same source. It is NOT an ADR-0028
	// oracle feed id and does not pretend to be one — the meaning of
	// this block follows `provenance`.
	ListingID string `json:"listing_id"`
	// Quote is the denominator, always `fiat:USD` on a served row. On
	// the wire rather than assumed, because a price denominated in a
	// reserve asset is a ratio and the difference is invisible in the
	// number alone.
	Quote string `json:"quote"`
	// Address is the EXACT Stellar address the platform named, which is
	// the whole of the binding. Published so a reader can check the
	// join rather than trust it.
	Address string `json:"address"`
	// AddressForm is which route matched: [ListingAddressFormClassic] or [ListingAddressFormSAC].
	// The SAC form is DERIVED: the platform named a contract and this index proved it is the one
	// this (code, issuer) pair deterministically produces.
	AddressForm string `json:"address_form"`
	// AsOf is the PLATFORM's own publication time, not when this index read it, so a recent sync
	// cannot launder a price that froze weeks ago.
	AsOf WireTime `json:"as_of"`
	// Stale marks a price older than [rwaReferenceStaleAfter] — served,
	// but labelled. Past [rwaReferenceMaxAge] it is not served at all.
	Stale bool `json:"stale,omitempty"`
	// Provenance names WHAT KIND of figure this is and is mandatory, never defaulted. Today only
	// [RWAReferenceListingPrice]; a consumer meeting a second kind must not mistake it for this one.
	Provenance string `json:"provenance"`
}

// AssetListingValuation is the asset's circulating supply valued at the listing platform's price.
// It is NOT a market capitalisation.
//
//   - `market_cap_usd` passed the thin-market substance gate, the dust-liquidity guard and the
//     scam-issuer suppression, and rests on a trade on a venue this index observed.
//   - this figure is a platform's aggregate price times a supply reading; this index observed and
//     gated none of those trades. It is a second opinion, not an equivalent.
//
// The two are never folded or summed into one total without the total saying so.
type AssetListingValuation struct {
	// Status is the single authority on why there is or is not a
	// figure. Always present when the block is.
	Status string `json:"status"`
	// ValueUSD is circulating supply (scaled by the asset's own
	// decimals) times the listing price, as a 2-dp decimal string —
	// exact rational arithmetic throughout, never a float (ADR-0003).
	// Present if and only if Status is [ListingValuationPublished].
	ValueUSD *string `json:"value_usd,omitempty"`
	// CirculatingSupply is the raw integer supply, in the asset's smallest unit, that ValueUSD
	// was computed from; scale it by the row's `decimals`.
	//
	// It is published here because it can legitimately differ from the row's own
	// `circulating_supply`: against a trustline floor this arm takes the LARGER of the row's
	// reading and the lake's flow total, and the detail row often has no reading at all. A
	// dollar total with a guessed multiplicand is worse than no figure.
	CirculatingSupply string `json:"circulating_supply,omitempty"`
	// SupplyBasis names where that number came from: [ListingSupplyBasisLakeFlows] (the lake's
	// mint-burn total over the asset's SAC) or [ListingSupplyBasisServed] (the reading already on
	// the row).
	//
	// The two differ by far more than rounding on exactly the assets this arm serves: USDT0's
	// trustline sum is 6,469 tokens against 2,581,052 by mint-burn, because most of its float sits
	// in contracts and claimable balances that a trustline query cannot see. The wrong basis gives
	// a plausible-looking valuation 400x too small.
	SupplyBasis string `json:"supply_basis,omitempty"`
}

// AssetListingReference.AddressForm values.
const (
	// ListingAddressFormClassic — the listing named `CODE-GISSUER`
	// itself. Direct: no derivation stands between the published string
	// and the asset's identity.
	ListingAddressFormClassic = "classic"
	// ListingAddressFormSAC — the listing named a contract address, and
	// it is the one canonical.Asset.SacContractID() derives from this
	// asset's (code, issuer) and the network passphrase. Derived, and
	// exact: no other issuer's asset derives to it.
	ListingAddressFormSAC = "sac"
)

// AssetListingValuation statuses.
const (
	// ListingValuationPublished — a listing price and a supply reading
	// were both available and the product is served.
	ListingValuationPublished = "published"
	// ListingValuationMarketCapPublished — the row carries an observed
	// `market_cap_usd`. This arm stands down; it exists to fill a hole,
	// not to offer a second opinion beside a figure the gates approved.
	ListingValuationMarketCapPublished = "market_cap_published"
	// ListingValuationMarketPriceObserved — the row has an observed price that cleared every gate
	// but no cap. The gap is almost always a missing supply reading, and a third party's price
	// would paper over it.
	ListingValuationMarketPriceObserved = "market_price_observed"
	// ListingValuationUnavailable — the listing directory could not be
	// read, is not wired, or holds no fresh rows. Nothing is published
	// and no conclusion is drawn about any address: "we did not look"
	// is not "nobody lists it".
	ListingValuationUnavailable = "listing_unavailable"
	// ListingValuationNotListed — the directory WAS read, and it names
	// neither this asset's classic id nor its SAC. The ordinary state
	// of almost every asset on the network, and not an accusation.
	ListingValuationNotListed = "not_listed"
	// ListingValuationNoListingPrice — the directory names the address
	// but published no price for it, or published one whose own
	// publication time is past the storage layer's price bound.
	ListingValuationNoListingPrice = "no_listing_price"
	// ListingValuationPriceNotPositive — the published price is zero,
	// negative or unparseable, so no valuation can be taken from it.
	ListingValuationPriceNotPositive = "listing_price_not_positive"
	// ListingValuationPriceExpired — the publication time is past [rwaReferenceMaxAge]. Re-checked
	// despite the tighter SQL bound, so the documented bound holds however the row arrived.
	ListingValuationPriceExpired = "listing_price_expired"
	// ListingValuationNoSupply — neither the lake's flow total nor the
	// row's own reading produced a supply to multiply. Refused rather
	// than published as zero.
	ListingValuationNoSupply = "no_supply"
)

// AssetListingValuation.SupplyBasis values.
const (
	// ListingSupplyBasisLakeFlows — Σmint − Σburn − Σclawback over the asset's SAC, from the
	// lake's flow log. The only reading not keyed on where tokens are HELD, so the only one that
	// sees claimable balances, liquidity-pool reserves and SAC-held balances.
	ListingSupplyBasisLakeFlows = "lake_flows"
	// ListingSupplyBasisServed — the `circulating_supply` already on the row, with its own
	// `supply_basis`. Used when the row holds an ADR-0011 observation (outranks the lake, see
	// [classicSupplyReading]), when its trustline floor is larger, or when the lake could not
	// answer. Against a floor [higherClassicSupply] applies: every trustline balance was minted,
	// so a lake total below the trustline sum is incomplete seeding, not a smaller truth.
	ListingSupplyBasisServed = "served"
)

// ─── storage seam ───────────────────────────────────────────────────

// AssetListingDirectoryReader is the seam for reading the independent listing directory;
// *timescale.Store satisfies it.
//
// It is deliberately a DIFFERENT method from [RWAListingDirectoryReader]'s, which serves contract
// rows only; a classic catalogue asset can be named under either address form.
//
// Optional: without it no listing valuations are published and [ListingValuationUnavailable] says
// nobody looked, not that nobody lists these addresses.
type AssetListingDirectoryReader interface {
	ListingDirectoryByAddress(ctx context.Context) (
		map[string]timescale.ListingEntry, timescale.ListingDirectoryCensus, error)
}

// ─── snapshot ───────────────────────────────────────────────────────

// assetListingSnapshotTTL bounds how long one directory read is reused across requests. It is a
// REQUEST-RATE bound, not a freshness one: the table's facts carry their own 48h and 24h bounds
// in SQL. It is far shorter than both so it never delays a correction.
const assetListingSnapshotTTL = 60 * time.Second

// assetListingReadBudget bounds one refresh read. The read is detached
// from the triggering request and runs under the cache mutex every
// listing-consulting request queues on, so it needs a deadline of its
// own.
const assetListingReadBudget = 5 * time.Second

// assetListing is one read of the directory, reduced to what this
// surface may consult.
type assetListing struct {
	// byAddress holds every recognised row, of either address form,
	// keyed by the exact address as published.
	byAddress map[string]timescale.ListingEntry
	// census is the storage layer's account of its own numbers,
	// including the rows it refused to serve as stale — the visible
	// form of the fail-closed guarantee.
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
// Fails CLOSED like [Server.rwaCuratedSnapshot]: a failed read yields an UNAVAILABLE snapshot and
// is CACHED as such. Carrying the last good snapshot forward would keep publishing dollar figures
// from an unreadable directory with nothing on the wire to say so.
//
// The read uses a context DETACHED from the request, so a disconnect cannot poison the shared
// cache, and its own deadline, so a hung read cannot hold the mutex indefinitely.
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
	// Stamped only once the read has returned, as rwaCuratedSnapshot does:
	// a read that panics leaves the entry expired for the next caller.
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
	// Zero fresh rows cannot support "nobody names this address": the storage reader drops stale
	// rows, so an entirely stale table looks like an empty directory. Treated as available it
	// would mark every asset not_listed, a claim about the world when only our copy expired.
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
// Errors are dropped: the only way to reach one is an asset type with no SAC, and that answers
// like an unlisted asset (no address, no valuation). A Soroban token IS its own contract and is
// served by the RWA contract arm.
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

// listingEntryFor resolves the one directory row that names this asset, and which route named it.
// Classic goes first because it is the platform's direct statement, while the SAC is our
// derivation; both name the same asset, so the order only picks the published `address_form`.
func (l assetListing) entryFor(assetID string) (timescale.ListingEntry, string, bool) {
	if !l.available {
		return timescale.ListingEntry{}, "", false
	}
	return listingEntryIn(l.byAddress, assetID)
}

// listingEntryIn is the address-resolution rule over any by-address map: classic id first, SAC
// second, with the matching form reported.
//
// It is the ONE place the rule lives; /v1/assets and the /v1/rwa/assets classic arm both use it.
// Two resolvers would let a SAC-listed classic member be priced on one surface and refused as
// unbound on the other. A nil map answers "not listed", the fail-closed reading for an
// unavailable snapshot.
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
// every verified-catalogue row publishing no market cap. Other rows stay untouched: a status on
// each of the thousands of uncatalogued assets would be noise.
//
// Best-effort at every step: a missing catalogue or reader, a failed read or a cold supply cache
// yield fewer figures, never an error or failed response.
func (s *Server) applyListingValuations(ctx context.Context, rows []AssetDetail) {
	if s.VerifiedCurrencies == nil {
		return
	}
	// Narrow to eligible rows BEFORE the two reads (directory snapshot, lake-supply lookup), so a
	// 500-row page whose catalogue assets are already valued pays for neither.
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

	// The supply reading waits until after the address match: the lake read is the expensive
	// half and an unlisted asset never needs it.
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
	// unattested asset and a status on every long-tail row would drown the rest.
	if _, ok := s.VerifiedCurrencies.LookupByStellarAssetID(row.AssetID); !ok {
		return false
	}
	// A scam-flagged issuer or a row impersonating a verified ticker gets no valuation: this arm
	// must not re-publish a dollar figure under suppressScamIssuerPricing. Silent, because the
	// accusation belongs on the row's own warning fields.
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
	// A price that cleared the substance gate with no dust suppression, yet no cap, is a missing
	// supply reading in our data; a third party's number would hide that gap. A non-empty
	// PriceBasis (declared-peg or transitive) is a conversion basis, not an observation, so such
	// a row is still a price hole.
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
// `lake` is Σmint − Σburn − Σclawback over the asset's SAC. [listingSupplyReading] prefers it
// when the row's reading is a trustline FLOOR (or absent) and the lake is larger; an ADR-0011
// observation is kept. The gap can be two orders of magnitude (USDT0: 6,469 vs 2,581,052). Taking
// the larger is safe because the trustline sum is a provable LOWER BOUND, so a smaller lake total
// means incomplete flow seeding.
//
// The reading is published INSIDE the valuation block and the row's `circulating_supply` is left
// alone, since a dollar figure needs a visible multiplicand.
func (s *Server) publishListingValuation(row *AssetDetail, lake string) {
	circ, reading := listingSupplyReading(row, lake)
	if circ == "" {
		row.ListingValuation = &AssetListingValuation{Status: ListingValuationNoSupply}
		row.ListingReference = nil
		return
	}
	// Taken from the picker, not by comparing the winner to `lake`: that mislabelled a served
	// figure merely EQUAL to the lake total, giving identical readings two provenances.
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
// total. The lake may replace the row's reading only when that is a provable FLOOR
// ([supply.Basis.LowerBound]) or absent, since the [higherClassicSupply] guard is sound only
// against a sum of minted balances. An ADR-0011 observation outranks the lake
// ([classicSupplyReading]) because the lake over-counts replayed mints with missing burns, and
// the valuation must not overrule the row's published supply. A reading with no basis is not
// known to be a floor, so it is kept too.
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
// Exact rational arithmetic end to end (ADR-0003, like [rwaReferenceValueUSD]): never a float, and
// the 2-dp rounding happens once, at the end, so a consumer summing the served strings gets the
// same total. The scale is the ASSET's own decimals. A negative scale must be guarded because
// [big.Int.Exp] answers a negative exponent with 1, which would publish the raw count as dollars.
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
