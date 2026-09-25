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

// A listing-sourced valuation for a verified-catalogue asset whose
// MARKET capitalisation this index declines to publish.
//
// # The hole this fills, and the hole it must not fill
//
// USDT0 is the case that produced this file. It launched on Stellar on
// 2026-09-02 and trades about a hundred dollars a day there. The
// dust-liquidity guard therefore suppresses its market cap, and that is
// the guard working: a price scraped off a $106/day market, multiplied
// by two and a half million tokens, is a headline nobody should publish
// and this index refuses to. Nothing below relaxes that refusal — the
// market cap stays absent, the guard's flag stays set, and no figure
// here is ever written into `market_cap_usd`.
//
// What was missing was never the price. An independent listing platform
// publishes a USD price for this exact token, derived from venues this
// index does not observe and has no opinion about. Serving supply times
// THAT price, under its own name and its own provenance, states
// something true and separately checkable. Folding it into market cap
// would state something false.
//
// So this surface is the /v1/assets twin of the split the RWA surface
// already ships (internal/api/v1/rwa_reference.go): a `reference` block
// carrying the price and where it came from, beside a `valuation` block
// carrying supply times that price. Never summed into market cap, never
// substituted for it, and labelled on every row with the provenance that
// says which kind of claim it is.
//
// # Two routes to the binding, both exact, neither on the code
//
// A row qualifies when the listing directory names EITHER
//
//	CODE-GISSUER   the asset's own classic id, or
//	C…             the Stellar Asset Contract address that
//	               canonical.Asset.SacContractID() derives from that
//	               exact (code, issuer) pair and the network passphrase.
//
// The second route is safe for a structural reason and not a
// probabilistic one: SAC derivation is a pure function of the asset and
// the network, so the address the listing published is reachable from
// one (code, issuer) pair and no other. An impersonator minting the same
// code from a different G-account derives a DIFFERENT contract address,
// and the listing's row does not name it.
//
// Both routes are needed because the upstream publishes each asset under
// one form or the other, with no pattern this index controls. Measured
// against the live upstream on 2026-09-15: EURC, AQUA, SHX, VELO, BLND
// and yUSDC are named by their classic ids; USDC, PYUSD, USDT0 and XLM
// are named by their SAC addresses and not by their classic ids at all.
// A reader supporting only one form would silently drop half the set.
//
// Matching by CODE is the one thing that is never done. This network
// carries impersonating issuers of PYUSD, USDT, USDC and XLM, one of
// them holding a 920-billion fake balance; a code match would hand each
// of them the real instrument's price.
//
// # Membership is the second, independent gate
//
// An exact address match is not on its own enough to publish a dollar
// figure, so the asset must ALSO be a verified-catalogue entry — a
// (code, issuer) pair written into internal/currency/data/seed.yaml,
// which is a code change and a redeploy. The listing directory
// corroborates; the catalogue attests. Neither alone publishes anything.
//
// # It only ever fills a hole
//
// Every one of these conditions has to hold, and the order below is the
// order [Server.listingValuationFor] tests them in:
//
//   - the row publishes NO market cap. Where the existing gates DO
//     publish one, this arm records [ListingValuationMarketCapPublished]
//     and stops. A served market cap is never replaced, never adjusted
//     and never compared against;
//   - the hole is a PRICE hole. A row carrying an observed market price
//     that cleared the substance gate, with no dust suppression, is
//     missing its cap for some other reason (most often no supply
//     reading yet), and pricing it from a third party would paper over
//     a gap in this index's own data with somebody else's number. That
//     row records [ListingValuationMarketPriceObserved] and stops. A
//     declared-peg or transitive price is NOT an observation and does
//     not stop it — `price_basis` is precisely the field that says so;
//   - the row is not scam-flagged and not an unverified collision. Those
//     rows publish no valuation of any kind, here or anywhere.
//
// # Fail closed, and say so
//
// A snapshot that could not be read, or that came back with no fresh
// rows, publishes NOTHING and records [ListingValuationUnavailable]. It
// does not carry a previous snapshot forward. This is the same posture,
// and the same reasoning, as [Server.rwaListingSnapshot]: an old price
// served as current is a lie about a number, and this surface would
// rather say it is not answering.
//
// The price's own age is bounded by the same two constants the RWA
// reference arm uses — [rwaReferenceStaleAfter] to LABEL and
// [rwaReferenceMaxAge] to WITHHOLD — rather than by a second pair
// invented here. One bound, one meaning, one place to change it.

// ─── wire shape ─────────────────────────────────────────────────────

// AssetListingReference is an independent listing platform's own USD
// price for the exact Stellar address this asset lives at.
//
// It is NOT this index's price for the asset, and it is not derived from
// any Stellar market. The asset's own `price_usd` — when there is one —
// sits beside it unchanged, so a reader can see the two independently
// rather than only their ratio.
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
	// AddressForm is which of the two routes matched:
	// [ListingAddressFormClassic] or [ListingAddressFormSAC]. It
	// matters to a reader because the second is a DERIVED address — the
	// platform named a contract, and this index proved that contract is
	// the one this (code, issuer) pair deterministically produces.
	AddressForm string `json:"address_form"`
	// AsOf is the PLATFORM's own publication time for the price, not
	// when this index read it. The distinction is the reason a sync
	// that succeeded five minutes ago cannot launder a price that froze
	// two weeks ago.
	AsOf WireTime `json:"as_of"`
	// Stale marks a price older than [rwaReferenceStaleAfter] — served,
	// but labelled. Past [rwaReferenceMaxAge] it is not served at all.
	Stale bool `json:"stale,omitempty"`
	// Provenance names WHAT KIND of figure this is, and is mandatory on
	// every served reference rather than defaulted. Today the only
	// value is [RWAReferenceListingPrice]; the field is present so a
	// consumer that later meets a second kind cannot mistake it for
	// this one.
	Provenance string `json:"provenance"`
}

// AssetListingValuation is the asset's circulating supply valued at the
// listing platform's price — and it is NOT a market capitalisation.
//
// The distinction is the entire reason this field exists separately:
//
//   - `market_cap_usd` is adversarially verified. A price reaches it
//     only after the thin-market substance gate, the dust-liquidity
//     guard and the scam-issuer suppression have each declined to
//     withhold it, and behind that price is a trade somebody settled on
//     a venue this index observed.
//   - this figure is an independent platform's aggregate of what the
//     token trades at elsewhere, multiplied by a supply reading. This
//     index observed none of those trades and applied none of its gates
//     to them. It is a second opinion, published because withholding a
//     figure that can be correctly sourced and correctly labelled is
//     its own kind of dishonesty — not because it is equivalent.
//
// The two are never folded together, never summed into one total without
// the total saying so, and a consumer that renders this as a market cap
// has misread the field name, the provenance and this comment.
type AssetListingValuation struct {
	// Status is the single authority on why there is or is not a
	// figure. Always present when the block is.
	Status string `json:"status"`
	// ValueUSD is circulating supply (scaled by the asset's own
	// decimals) times the listing price, as a 2-dp decimal string —
	// exact rational arithmetic throughout, never a float (ADR-0003).
	// Present if and only if Status is [ListingValuationPublished].
	ValueUSD *string `json:"value_usd,omitempty"`
	// CirculatingSupply is the multiplicand: the raw integer supply, in
	// the asset's smallest unit, that ValueUSD was computed from.
	//
	// Published in the block rather than inferred from the row's own
	// `circulating_supply`, because the two can legitimately differ —
	// against a trustline floor this arm takes the LARGER of the row's
	// reading and the lake's flow total, and on the detail surface the
	// row often has no reading at all. A figure whose multiplicand a reader has to guess at is a
	// dollar total with no traceable source, which on this surface is
	// worse than no figure.
	//
	// Scale it by the row's `decimals`, as with every other raw supply
	// integer this API serves.
	CirculatingSupply string `json:"circulating_supply,omitempty"`
	// SupplyBasis names where that number came from:
	// [ListingSupplyBasisLakeFlows] when it is the lake's mint−burn
	// total over the asset's SAC, [ListingSupplyBasisServed] when it is
	// the supply reading already on the row.
	//
	// It is published because the two differ by more than rounding on
	// exactly the assets this arm serves. USDT0's trustline sum is
	// 6,469 tokens against 2,581,052 by mint−burn, because almost all
	// of its float is held by contracts and by claimable balances that
	// a trustline query is blind to by construction. A valuation built
	// on the wrong one is 400x too small and looks entirely plausible.
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
	// ListingValuationMarketPriceObserved — the row carries an observed
	// market price that cleared every gate, and no cap. The missing cap
	// is therefore not a price problem (it is almost always a missing
	// supply reading), and a third party's price would paper over a gap
	// in this index's own data.
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
	// ListingValuationPriceExpired — the price carries a publication
	// time past [rwaReferenceMaxAge]. Re-checked here even though the
	// storage reader enforces a tighter bound in SQL, for the reason
	// the RWA path re-checks its own: the bound this surface documents
	// has to hold however the row reached it.
	ListingValuationPriceNotPositive = "listing_price_not_positive"
	ListingValuationPriceExpired     = "listing_price_expired"
	// ListingValuationNoSupply — neither the lake's flow total nor the
	// row's own reading produced a supply to multiply. Refused rather
	// than published as zero.
	ListingValuationNoSupply = "no_supply"
)

// AssetListingValuation.SupplyBasis values.
const (
	// ListingSupplyBasisLakeFlows — Σmint − Σburn − Σclawback over the
	// asset's Stellar Asset Contract, from the lake's decode-at-ingest
	// flow log. The only one of the readings that is not keyed on where
	// the tokens are HELD, and therefore the only one that can see
	// claimable balances, liquidity-pool reserves and SAC-held
	// balances. See classic_lake_supply.go.
	ListingSupplyBasisLakeFlows = "lake_flows"
	// ListingSupplyBasisServed — the `circulating_supply` already on
	// the row, carrying the row's own `supply_basis`. Used when the row
	// holds an ADR-0011 observation (which outranks the lake, see
	// [classicSupplyReading]), when its trustline floor is the larger of
	// the two readings, or when the lake could not answer. Against a
	// floor the guard in [higherClassicSupply] applies: every trustline
	// balance was minted, so a lake total below the trustline sum is
	// incomplete seeding rather than a smaller truth.
	ListingSupplyBasisServed = "served"
)

// ─── storage seam ───────────────────────────────────────────────────

// AssetListingDirectoryReader is the seam this surface reads the
// independent listing directory through. *timescale.Store satisfies it.
//
// Deliberately a DIFFERENT method from [RWAListingDirectoryReader]'s:
// that one serves contract rows only, which is correct for a surface
// whose members are contracts, and wrong here — a classic catalogue
// asset can be named under either form and six of the ten live matches
// are classic.
//
// Optional, like every other reader on this surface. A deployment
// without it publishes no listing valuations and records
// [ListingValuationUnavailable], which says nobody looked rather than
// asserting that nobody lists these addresses.
type AssetListingDirectoryReader interface {
	ListingDirectoryByAddress(ctx context.Context) (
		map[string]timescale.ListingEntry, timescale.ListingDirectoryCensus, error)
}

// ─── snapshot ───────────────────────────────────────────────────────

// assetListingSnapshotTTL bounds how long one read of the listing
// directory is reused across requests.
//
// 60 seconds, and it is a REQUEST-RATE bound rather than a freshness
// one: the two facts in the table already carry their own bounds, of 48
// hours and 24 hours, enforced in SQL. What this constant controls is
// how often a 50-row query runs on a listing page's hot path. It is
// deliberately far shorter than either staleness bound, so it can never
// be the reason a correction takes longer to propagate.
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
	// available is false when the read failed, no reader is wired, or
	// the snapshot holds no fresh rows. Every one of those is "no
	// binding was established", and none of them may be read as "this
	// address is not listed".
	available bool
}

// assetListingCache is the process-wide memo for [assetListing].
type assetListingCache struct {
	mu   sync.Mutex
	snap assetListing
	at   time.Time
}

// assetListingSnapshot returns the cached directory read, refreshing it
// past [assetListingSnapshotTTL].
//
// Fails CLOSED in the strong sense, the same way [Server.rwaCuratedSnapshot]
// does: a failed read yields an UNAVAILABLE snapshot and is CACHED as
// such. Carrying the last good snapshot forward is the tempting move and
// the wrong one — it would keep publishing dollar figures from a
// directory nobody can currently read, with nothing on the wire to say
// so, for as long as the outage lasted.
//
// The read runs on a context DETACHED from the caller's request, so the
// request that triggers a refresh cannot poison the shared cache by
// disconnecting mid-read, and under its own deadline, so a hung read
// cannot hold the mutex indefinitely.
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
	s.assetListings.at = time.Now()
	s.assetListings.snap = s.readAssetListing(rctx)
	return s.assetListings.snap
}

// readAssetListing performs the one directory read behind the cache.
func (s *Server) readAssetListing(ctx context.Context) assetListing {
	if s.listings == nil {
		return assetListing{}
	}
	rows, census, err := s.listings.ListingDirectoryByAddress(ctx)
	if err != nil {
		s.logger.Warn("asset listing directory read failed", "err", err)
		return assetListing{}
	}
	// Zero fresh rows cannot support the finding "nobody independent
	// names this address", whatever emptied it. The storage reader drops
	// every row past the recognition bound, so a table that has gone
	// entirely stale arrives here as zero rows beside a non-zero Stale
	// count — indistinguishable, from this side, from a directory that
	// genuinely lists nothing on Stellar. Treated as available it would
	// refuse every asset under not_listed, a claim about the world, when
	// the truth is that our copy expired.
	if len(rows) == 0 {
		s.logger.Warn("asset listing directory: no fresh rows",
			"stale", census.Stale, "entries", census.Entries)
		return assetListing{census: census}
	}
	return assetListing{byAddress: rows, census: census, available: true}
}

// ─── the binding ────────────────────────────────────────────────────

// listingAddressesFor returns the two addresses a listing may name this
// asset by, classic form first.
//
// Native XLM yields only the SAC form: it has no `CODE-GISSUER` id, and
// the listing names it by its SAC (`CAS3J7GY…`) alone.
//
// Errors are dropped rather than reported because there is exactly one
// way to reach them — an asset whose type has no SAC at all — and the
// answer for such an asset is the same as the answer for one nobody
// lists: no address to match on, so no valuation. A Soroban token IS its
// own contract and is out of scope here; the RWA contract arm serves it.
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

// listingEntryFor resolves the one directory row that names this asset,
// and which route named it.
//
// Classic first, then the SAC. The order is not arbitrary: the classic
// id is the platform's direct statement about the asset, while the SAC
// is a derivation this index performed. When both are present they name
// the same asset, so the choice only decides which address is published
// in `address_form` — and the direct statement is the more useful thing
// to hand a reader checking the join.
func (l assetListing) entryFor(assetID string) (timescale.ListingEntry, string, bool) {
	if !l.available {
		return timescale.ListingEntry{}, "", false
	}
	return listingEntryIn(l.byAddress, assetID)
}

// listingEntryIn is the address-resolution rule itself, over any
// by-address map of directory rows: classic id first, SAC second, the
// form that matched reported alongside.
//
// It is the ONE place the rule lives. /v1/assets reaches it through
// [assetListing.entryFor]; the /v1/rwa/assets classic arm reaches it
// directly over the same snapshot's map. Two resolvers over one
// directory is how a SAC-listed classic member came to be priced on one
// surface and refused as unbound on the other (issue #514).
//
// A nil map answers every lookup with "not listed", which is the
// fail-closed reading an unavailable snapshot needs.
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

// applyListingValuations attaches a listing-sourced valuation, or the
// reason there is none, to every row of `rows` that is a
// verified-catalogue asset publishing no market cap.
//
// Rows outside that population are left completely untouched and carry
// neither block — the listing is silent about the several thousand
// classic assets on this network that nobody has catalogued, and a
// status on every one of them would be noise rather than information.
//
// Best-effort at every step, like every other overlay on this path: no
// catalogue, no reader, a failed read or a cold supply cache all yield
// fewer published figures, never an error and never a failed response.
func (s *Server) applyListingValuations(ctx context.Context, rows []AssetDetail) {
	if s.verifiedCurrencies == nil {
		return
	}
	// Narrow to the rows this arm may speak about BEFORE paying for
	// anything. Two reads hang off this — the directory snapshot and,
	// for the candidates, a lake-supply lookup — and a 500-row listing
	// page whose catalogue assets are all already valued should pay for
	// neither.
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

	// The supply reading, for the candidates that got past the address
	// match. Deferred until after the match for the same reason the
	// candidate narrowing happens first: the lake read is the expensive
	// half, and an asset nobody lists will never need it.
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

// listingValuationCandidate reports whether this row is one this arm may
// speak about at all, and records the terminal status when it is not.
//
// The three refusals here are the ones that hold whatever the listing
// says, so they are decided before the directory is even read.
func (s *Server) listingValuationCandidate(row *AssetDetail) bool {
	// Not a catalogued identity. No status: the listing has no standing
	// to price an asset this repository has not attested, and saying so
	// on every row of the long tail would drown the rows that matter.
	if _, ok := s.verifiedCurrencies.LookupByStellarAssetID(row.AssetID); !ok {
		return false
	}
	// A scam-flagged issuer, or a row impersonating a verified ticker,
	// publishes no valuation of any kind. These sit above the whole
	// price/market-cap machinery — suppressScamIssuerPricing nils the
	// price outright — and this arm must not be the one path that
	// re-publishes a dollar figure underneath them. Silent for the same
	// reason: a status would be a second, quieter place to state an
	// accusation that belongs on the row's own warning fields.
	if row.IssuerScamReason != "" {
		return false
	}
	if issuerPricingWithheld(row) {
		return false
	}
	if row.UnverifiedTickerCollision {
		return false
	}
	// An observed market cap is served. Never replaced, never compared
	// against, never supplemented — this arm fills a hole and there is
	// no hole.
	if row.MarketCapUSD != nil {
		row.ListingValuation = &AssetListingValuation{Status: ListingValuationMarketCapPublished}
		return false
	}
	// A price that cleared the substance gate, with no dust suppression,
	// and still no cap. The hole is not a price hole — it is almost
	// always a supply reading this index does not have yet — and filling
	// it from a third party would hide a gap in our own data behind
	// somebody else's number.
	//
	// PriceBasis is what separates the two cases: a declared-peg or
	// transitive price is a conversion basis, not a market observation,
	// so a row carrying one is still a price hole.
	if row.PriceUSD != nil && row.PriceBasis == "" && !row.MarketCapLowLiquidity {
		row.ListingValuation = &AssetListingValuation{Status: ListingValuationMarketPriceObserved}
		return false
	}
	return true
}

// listingPriceRefusal returns the status refusing this entry's price, or
// "" when it may be served.
//
// The storage reader already blanks a price past its own 24-hour bound,
// so an entry arriving here with a price is inside it. The age is
// re-checked anyway against [rwaReferenceMaxAge] for the reason the RWA
// reference path re-checks its own: the bound this surface documents has
// to hold however the row reached it, not only on the path that was
// expected to deliver it.
func listingPriceRefusal(entry timescale.ListingEntry, now time.Time) string {
	if entry.PriceUSD == "" {
		return ListingValuationNoListingPrice
	}
	if entry.PricedAt.IsZero() || now.Sub(entry.PricedAt) > rwaReferenceMaxAge {
		return ListingValuationPriceExpired
	}
	// Not a valuation. Refused rather than multiplied, the same way the
	// oracle path refuses a non-positive net asset value: the product
	// would be a number nobody claimed.
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

// publishListingValuation multiplies the row's supply by the reference
// price already attached to it, or records why it cannot.
//
// # Which supply, and why it is not the one on the row
//
// `lake` is Σmint − Σburn − Σclawback over the asset's Stellar Asset
// Contract, and [listingSupplyReading] prefers it over the row's own
// reading whenever that reading is a trustline FLOOR (or absent) and the
// lake is the larger of the two; an ADR-0011 observation on the row is
// kept. That preference is the
// difference between a correct figure and one that is wrong by two
// orders of magnitude, and USDT0 is the proof: its trustline sum is
// 6,469 tokens against 2,581,052 by mint−burn, because a trustline query
// is blind by construction to tokens held by contracts, by claimable
// balances and by liquidity pools. Valuing the trustline sum would have
// published a figure 400 times too small and looked entirely plausible
// doing it.
//
// The floor guard in [higherClassicSupply] is what makes taking the
// larger safe rather than merely optimistic: every trustline balance was
// minted, so the trustline sum is a provable LOWER BOUND on issued
// supply, and a lake total below it is evidence of incomplete flow
// seeding rather than of a smaller truth.
//
// The reading is published INSIDE the valuation block and the row's own
// `circulating_supply` is left exactly as every other producer left it.
// Two reasons, and they point the same way. A dollar figure whose
// multiplicand is invisible is a total with no traceable source, so the
// number has to appear somewhere; and overwriting the row's field would
// change what an existing, widely-consumed field means on the strength
// of a third party's row, which is a much larger claim than this arm is
// making.
func (s *Server) publishListingValuation(row *AssetDetail, lake string) {
	circ, reading := listingSupplyReading(row, lake)
	if circ == "" {
		row.ListingValuation = &AssetListingValuation{Status: ListingValuationNoSupply}
		row.ListingReference = nil
		return
	}
	// Taken from the picker rather than re-derived by comparing the winner
	// back against `lake`: that comparison called a served figure that
	// merely EQUALS the lake total "served", so two identical readings
	// published two different provenances.
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

// listingSupplyReading picks the multiplicand from the row's own supply
// reading and the lake's flow total.
//
// The lake may replace the row's reading only when that reading is a
// provable FLOOR ([supply.Basis.LowerBound]) or absent: the floor guard
// in [higherClassicSupply] is sound only against a sum every balance of
// which was minted. When the row carries an ADR-0011 observation, that
// is the arm [classicSupplyReading] ranks above the lake, because the
// lake over-counts replayed mints whose burns are missing (BLND +11.53%,
// PHO +156.79% on 2026-09-15), and the valuation must not overrule the
// supply the row itself publishes. A reading with no basis is not known
// to be a floor, so it is kept too.
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

// listingValueUSD = (circulating / 10^decimals) x price, as a 2-dp
// decimal string. Empty on any input that is not a valuation.
//
// EXACT rational arithmetic end to end (ADR-0003), the same shape as
// [rwaReferenceValueUSD] and for the same reasons. Both inputs arrive
// exact and stay exact: the supply is an integer count of the smallest
// unit and the price is the decimal literal the platform published, so
// nothing on this path is ever a float. The 2-dp rounding happens once,
// at the end, which is what lets a consumer add the served strings up by
// hand and get the same total.
//
// The scale is the ASSET's own decimals, never a constant. A negative
// scale is not a scale, and [big.Int.Exp] answers a negative exponent
// with 1 — which would publish the raw smallest-unit count as dollars —
// so it is guarded rather than assumed.
func listingValueUSD(circRaw string, decimals int, price *big.Rat) string {
	if price == nil || decimals < 0 {
		return ""
	}
	circ := ratFromOptionalString(&circRaw)
	// A negative supply is bad data — a legitimate float is never
	// negative — and is refused rather than published as a negative
	// valuation.
	if circ == nil || circ.Sign() < 0 {
		return ""
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
	out := new(big.Rat).Quo(circ, scale)
	out.Mul(out, price)
	return out.FloatString(2)
}
