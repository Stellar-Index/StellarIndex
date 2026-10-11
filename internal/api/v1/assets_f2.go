package v1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// SupplyLooker is the read-side interface behind the F2 fields on /v1/assets/{id}. LatestSupply
// returns the most recent persisted [supply.Supply] or [ErrSupplyNotFound]. Real errors propagate
// so the handler can log them at WARN; the F2 fields then stay null (F2 is a best-effort overlay).
type SupplyLooker interface {
	LatestSupply(ctx context.Context, assetKey string) (supply.Supply, error)
	// DailyCirculatingSupply returns daily last-known circulating
	// supply for the asset_key from the supply_1d CAGG within [from,
	// to], plus the carry-in point before `from` for forward-filling.
	// Backs crypto market-cap-over-time (/v1/chart?price_type=market_cap).
	DailyCirculatingSupply(ctx context.Context, assetKey string, from, to time.Time) ([]timescale.SupplyDayPoint, error)
}

// ErrSupplyNotFound is what [SupplyLooker.LatestSupply] returns when the asset has no snapshot.
// The handler treats it as "unavailable for this asset": F2 fields stay null, nothing is logged.
var ErrSupplyNotFound = errors.New("api: supply snapshot not found")

// Change24hReader returns the asset's USD price from about 24 hours ago, the baseline for
// change_24h_pct. It picks the latest closed prices_1m bucket whose bucket-end is at or before
// now-24h. [ErrChange24hUnavailable] means no closed bucket exists in the window (e.g. first
// traded under 24h ago); change_24h_pct then stays null and nothing is logged. Real errors
// propagate so the handler can log them at WARN.
type Change24hReader interface {
	USDPrice24hAgo(ctx context.Context, asset canonical.Asset) (string, error)
}

// ErrChange24hUnavailable is what [Change24hReader.USDPrice24hAgo] returns when no comparison
// price exists in the 24h-ago window; unlike a real read error, the handler falls through silently.
var ErrChange24hUnavailable = errors.New("api: change_24h_pct comparison price unavailable")

// VolumeReader backs volume_24h_usd on /v1/assets/{id}: the trailing-24h USD trade volume summed
// across every pair where the asset is base or quote, as a decimal string ("0" is valid). Trades
// with no trade-time usd_volume contribute 0 and lowerBound reports that one was excluded.
// Errors propagate so the handler can log them; the field then stays null.
type VolumeReader interface {
	Volume24hUSDForAsset(ctx context.Context, assetKey string) (usd string, lowerBound bool, err error)
}

// SorobanVolumeReader is OPTIONALLY implemented by the wired [VolumeReader] to give pure-Soroban
// SEP-41 assets an XLM-anchored 24h USD volume. The plain reader only sees the insert-time
// usd_volume column (set when the quote is a USD-pegged classic), so a token trading against XLM
// reports a bogus "0". This variant values XLM-legged trades through the on-chain XLM/USD VWAP;
// SEP-41/SEP-41 legs still contribute 0. Same "0"-not-null convention as VolumeReader.
type SorobanVolumeReader interface {
	SorobanVolume24hUSDForAsset(ctx context.Context, assetKey string) (usd string, lowerBound bool, err error)
}

// applyF2Fields populates the F2 supply / market-cap / FDV fields on detail, best-effort. Per
// ADR-0011 nothing is fabricated: no max_supply means no fdv_usd, no USD price means no
// market_cap_usd or fdv_usd, and no supply snapshot leaves all six F2 fields nil.
func (s *Server) applyF2Fields(ctx context.Context, detail *AssetDetail, asset canonical.Asset) {
	key, keyErr := supply.AssetKey(asset)

	// The overlay fans out to up to four independent DB reads that write disjoint AssetDetail
	// fields, so a cold /v1/assets/{id} does not pay the sum. Each populate* helper is
	// best-effort: a failure logs, leaves its field null and folds into detail.f2ReadFailed.
	//
	// IMPORTANT: pass asset.String() to populateVolume24h (the wire form trades.base_asset stores),
	// NOT supply.AssetKey(asset): for native AssetKey returns "XLM" but trades stores "native", so
	// the supply key would match no trade rows.
	var (
		snap             supply.Supply
		haveSnap         bool
		priceSourceCount int
		volFailed        bool
		changeFailed     bool
		supplyFailed     bool
		wg               sync.WaitGroup
	)
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Child goroutines are not covered by middleware.Recoverer, so an unrecovered panic
			// would take down the whole API process. Degrade to best-effort: log and leave the
			// field unset.
			defer func() {
				if p := recover(); p != nil {
					// worker.Report feeds the stellarindex_worker_panics_total page rule, so a
					// populator that panics on every request is visible to an operator.
					worker.Report(s.logger, "api-asset-detail-populator", p)
				}
			}()
			fn()
		}()
	}
	run(func() { volFailed = s.populateVolume24h(ctx, detail, asset) })
	run(func() { changeFailed = s.populateChange24h(ctx, detail, asset) })
	// price_usd is inlined independent of supply so wallet UIs need no second /v1/price round
	// trip; populateMarketCap (phase 2) reuses it plus the venue count for the dust-liquidity guard.
	run(func() { priceSourceCount = s.populatePriceUSD(ctx, detail, asset) })
	// Only with a supply reader and a supply key: off-chain assets have no snapshot.
	if s.Supply != nil && keyErr == nil {
		run(func() { snap, haveSnap, supplyFailed = s.fetchSupplySnapshot(ctx, key) })
	}
	wg.Wait()

	// SEP-41 fallback: a Soroban token has no observer snapshot unless its contract is on the
	// operator watch-list, impractical at 10k+ tokens, so use the lake-derived supply
	// (stellar.supply_flows: mint minus burn minus clawback) that /v1/assets/{id}/supply uses.
	// The ClickHouse read is Soroban-only; a stale snapshot yields to the lake reading.
	now := time.Now()
	if (!haveSnap || supplyObservationStale(snap, now)) && s.TokenSupply != nil && asset.Type == canonical.AssetSoroban && asset.ContractID != "" {
		// ts.Incomplete means burns exceed mints: supply_flows are incompletely seeded, NOT that
		// supply is negative. Treat it as UNAVAILABLE so total_supply/circulating_supply stay
		// null, as SEP41Computer.Compute does. Never clamp to 0: that would read as a real
		// "fully burned" supply (ADR-0003).
		//
		// Bounded by tokenMetadataReadTimeout: this runs AFTER the wg.Wait barrier on the handler
		// goroutine, so an unbounded slow sum would extend /v1/assets/{id} for a mere null field.
		sctx, scancel := context.WithTimeout(ctx, tokenMetadataReadTimeout)
		defer scancel()
		if ts, terr := s.TokenSupply.TokenSupply(sctx, asset.ContractID); terr == nil && ts.Total != nil && !ts.Incomplete {
			snap = supply.Supply{
				AssetKey:          asset.ContractID,
				TotalSupply:       ts.Total,
				CirculatingSupply: ts.Total,
				Basis:             supply.BasisSEP41LakeFlows,
			}
			haveSnap = true
			supplyFailed = false
		}
	}
	detail.f2ReadFailed = volFailed || changeFailed || supplyFailed

	// max_supply precedence step 2: an operator override already shows as a non-nil snap.MaxSupply;
	// otherwise overlay the issuer's stellar.toml max_number / fixed_number (DISPLAY units, scaled
	// to raw by detail.Decimals). max_supply_basis="sep1_declared_max" tells consumers the cap and
	// the FDV derived from it are issuer-declared, not on-chain enforced.
	if haveSnap && snap.MaxSupply == nil {
		overlaid, applied, err := supply.Overlay(ctx, snap, asset, sep1DeclaredMaxResolver{detail: detail})
		if err != nil {
			s.logger.Debug("sep1 max_supply overlay failed", "asset", asset.String(), "err", err)
		} else if applied {
			snap = overlaid
		}
	}

	// Phase 2 is pure compute, run once the parallel reads have joined; the wg.Wait barrier makes
	// detail.PriceUSD and the snapshot safely visible to populateMarketCap.
	if haveSnap {
		detail.SupplyStale = supplyObservationStale(snap, now)
		populateSupplyFields(detail, snap)
		s.populateMarketCap(ctx, detail, asset, snap, key, priceSourceCount)
	}
}

// populateVolume24h fills detail.VolumeUSD24h via the [VolumeReader], best-effort. Pure-Soroban
// SEP-41 assets use the XLM-anchored [SorobanVolumeReader] when available, since the plain reader
// reports a bogus "0" for them. A Soroban lookup ERROR falls back to the plain reader; that
// figure omits XLM-legged trades, so it is flagged as a lower bound. Reports whether the plain
// read failed.
func (s *Server) populateVolume24h(ctx context.Context, detail *AssetDetail, asset canonical.Asset) (failed bool) {
	if s.Volume == nil {
		return false
	}
	assetKey := asset.String()
	if asset.Type == canonical.AssetSoroban {
		if sv, ok := s.Volume.(SorobanVolumeReader); ok {
			v, lowerBound, err := sv.SorobanVolume24hUSDForAsset(ctx, assetKey)
			if err == nil {
				detail.VolumeUSD24h = &v
				detail.VolumeLowerBound = lowerBound
				return false
			}
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.logger.Warn("soroban volume_24h_usd lookup failed; falling back to plain reader",
					"err", err, "asset_key", assetKey)
			}
			// Fall through to the plain reader below.
		}
	}
	v, lowerBound, err := s.Volume.Volume24hUSDForAsset(ctx, assetKey)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.logger.Warn("volume_24h_usd lookup failed", "err", err, "asset_key", assetKey)
		}
		// A pre-filled figure (insert-time prices_1m) is as partial as the plain read.
		detail.VolumeLowerBound = asset.Type == canonical.AssetSoroban && detail.VolumeUSD24h != nil
		return true
	}
	detail.VolumeUSD24h = &v
	// The plain reader never values a Soroban asset's XLM-legged trades.
	detail.VolumeLowerBound = lowerBound || asset.Type == canonical.AssetSoroban
	return false
}

// fetchSupplySnapshot wraps the SupplyLooker call with the best-effort error policy:
// ErrSupplyNotFound is silent, real errors log at WARN and report failed, client cancel is not logged.
func (s *Server) fetchSupplySnapshot(ctx context.Context, key string) (snap supply.Supply, ok, failed bool) {
	snap, err := s.Supply.LatestSupply(ctx, key)
	if errors.Is(err, ErrSupplyNotFound) {
		return supply.Supply{}, false, false
	}
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.logger.Warn("supply lookup failed", "err", err, "asset_key", key)
		}
		return supply.Supply{}, false, true
	}
	return snap, true, false
}

// populateSupplyFields sets the raw supply integers + basis on
// detail. Each field skipped when its source is nil/zero — per
// ADR-0011 we don't fabricate.
func populateSupplyFields(detail *AssetDetail, snap supply.Supply) {
	if snap.TotalSupply != nil {
		v := snap.TotalSupply.String()
		detail.TotalSupply = &v
	}
	if snap.CirculatingSupply != nil {
		v := snap.CirculatingSupply.String()
		detail.CirculatingSupply = &v
	}
	if snap.MaxSupply != nil {
		v := snap.MaxSupply.String()
		detail.MaxSupply = &v
		if snap.MaxSupplyBasis != "" {
			b := string(snap.MaxSupplyBasis)
			detail.MaxSupplyBasis = &b
		}
	}
	if snap.Basis != "" {
		v := string(snap.Basis)
		detail.SupplyBasis = &v
	}
	if !snap.ObservedAt.IsZero() {
		at := snap.ObservedAt.UTC()
		detail.SupplyAsOf = wireTimePtr(&at)
	}
	if snap.LedgerSequence != 0 {
		l := snap.LedgerSequence
		detail.SupplyAsOfLedger = &l
	}
}

// supplyObservationStale reports whether an observer snapshot is older
// than preciseSupplyMaxAge — the bound the listing's precise arm applies
// to the same asset_supply_history rows. A reading with no vintage (the
// lake-flows fallback) is live, not stale.
func supplyObservationStale(snap supply.Supply, now time.Time) bool {
	return !snap.ObservedAt.IsZero() && now.Sub(snap.ObservedAt) > preciseSupplyMaxAge
}

// populatePriceUSD inlines detail.PriceUSD via the lookupUSDPrice path; a no-op if another path
// already set PriceUSD. It returns the number of DISTINCT venues that backed the price, the
// liquidity signal for the dust-liquidity valuation guard; 0 when no price was found or one was
// already set.
func (s *Server) populatePriceUSD(ctx context.Context, detail *AssetDetail, asset canonical.Asset) int {
	if s.Prices == nil || detail.PriceUSD != nil {
		return 0
	}
	if asset.Equal(defaultPriceQuote) {
		// USD in USD is 1 by definition — the same identity the catalogue
		// view serves; no market is read, so no venue count backs it.
		identity := "1.00000000000000"
		detail.PriceUSD = &identity
		return 0
	}
	l := s.lookupUSDPriceWithSources(ctx, asset)
	if l.withheld != "" {
		detail.PriceWithheldReason = l.withheld
		return 0
	}
	if l.price == "" {
		return 0
	}
	priceCopy := l.price
	detail.PriceUSD = &priceCopy
	return l.sources
}

// assetDetailCacheKey keys the opt-in apart: its body can carry a thin
// price the default body must never replay.
func assetDetailCacheKey(asset canonical.Asset, includeThin bool) string {
	if includeThin {
		return asset.String() + "|thin"
	}
	return asset.String()
}

// clearThinWhenUnpriced drops the thin markers from a detail a later
// suppressor unpriced, so no response is flagged thin without a price.
func clearThinWhenUnpriced(d *AssetDetail) {
	if d.PriceUSD == nil {
		d.ThinMarket = false
		d.Substance = nil
	}
}

// thinDetailPricePass is the detail's opted-in second pass: re-read the price with the request's
// thin legs released, after pass 1 withheld it on measured evidence. It runs only the price
// producers, never applyF2Fields, so no market cap, FDV or change derives from a thin price.
// "unattributed" is excluded because on detail it labels the issuer suppression, which no thin
// release may override.
func (s *Server) thinDetailPricePass(ctx context.Context, detail *AssetDetail, asset canonical.Asset, off *ThinAdmission) {
	if detail.PriceUSD != nil || off.Evidence() == nil {
		return
	}
	switch detail.PriceWithheldReason {
	case "", PriceWithheldSubstance, PriceWithheldUpstreamLeg:
	default:
		return
	}
	onCtx, on := WithThinAdmission(ctx, asset, defaultPriceQuote, true)
	trial := *detail
	trial.PriceWithheldReason = ""
	s.populatePriceUSD(onCtx, &trial, asset)
	s.fillTransitivePrice(onCtx, &trial, asset, asset.String())
	if trial.PriceUSD == nil {
		return
	}
	detail.PriceUSD = trial.PriceUSD
	detail.PriceBasis = trial.PriceBasis
	detail.PriceWithheldReason = ""
	if on.Admitted() {
		detail.ThinMarket = true
		detail.Substance = substanceEvidenceWire(on.Evidence())
	}
}

// marketCapRefused is the pre-figure half of the valuation guards shared by the detail cap and
// the market-cap chart, so they cannot disagree on whether an asset has a valuation. An
// unverified ticker collision refuses outright (a look-alike of a verified currency must not
// publish price x supply). Single-venue sub-floor trading refuses as low liquidity. The turnover
// ceiling tests the computed figure, so each caller applies [capExceedsObservedTurnover] itself.
func (s *Server) marketCapRefused(asset canonical.Asset, priceSourceCount int, volume24hUSD *string) (refused, lowLiquidity bool) {
	if s.VerifiedCurrencies != nil && asset.Type == canonical.AssetClassic {
		if _, collision := s.VerifiedCurrencies.StellarCollision(asset.Code, asset.Issuer); collision {
			return true, false
		}
	}
	if !asset.Equal(canonical.NativeAsset()) &&
		dustLiquiditySuppressed(priceSourceCount, volume24hUSD, s.MinMarketCapVolumeUSD) {
		return true, true
	}
	return false, false
}

// populateMarketCap fills market_cap_usd + fdv_usd from the supply snapshot and the
// already-populated detail.PriceUSD, avoiding a second prices_1m lookup. Compute failures log at
// WARN and leave the field nil.
func (s *Server) populateMarketCap(ctx context.Context, detail *AssetDetail, asset canonical.Asset, snap supply.Supply, key string, priceSourceCount int) {
	if detail.PriceUSD == nil {
		// No price (no reader wired, or lookup found none): nothing to compute against.
		return
	}
	if detail.SupplyStale {
		// Today's price times a supply unobserved for longer than preciseSupplyMaxAge is not a
		// market cap; supply_as_of says how old it is and flags.stale marks the body.
		return
	}
	if detail.MarketCapDecimalsMismatch || detail.DecimalsUnresolved {
		// DecimalsUnresolved: the lake read failed and no projection row vouches for the scale, so
		// detail.Decimals is a guess.
		//
		// Decimals lockstep refusal (applyTokenDecimals): the price was normalised through the
		// nonstandard_decimals_assets projection but detail.Decimals is the lake's reading and they
		// disagree, so usdMarketValue would be wrong by 10^|difference| with both inputs plausible.
		// price_usd and supply still serve. Placed before the liquidity guards because this is not a
		// liquidity verdict.
		return
	}
	usdPrice := *detail.PriceUSD
	_ = ctx
	// Valuation-integrity guard: when the price came from a single venue AND trailing-24h USD
	// volume is below the operator floor, suppress BOTH market_cap_usd and fdv_usd and flag it, so
	// one dust trade cannot present an obscure asset as worth billions. price_usd still serves.
	// VolumeUSD24h is set by populateVolume24h before this join.
	//
	// Native carve-out for list/detail parity: the listing SQL forces native XLM's source_count to
	// NULL, so XLM is NEVER dust-suppressed there; mirror that so /v1/assets/native agrees.
	//
	// For non-native assets detail and listing count venues via DIFFERENT fallbacks (USDC proxy vs
	// XLM triangulation), so an asset can suppress on one and not the other. Detail is
	// deliberately the STRICTER and must not be weakened.
	if refused, lowLiquidity := s.marketCapRefused(asset, priceSourceCount, detail.VolumeUSD24h); refused {
		detail.MarketCapLowLiquidity = lowLiquidity
		return
	}
	if snap.CirculatingSupply != nil {
		if mc, err := usdMarketValue(snap.CirculatingSupply, usdPrice, detail.Decimals); err != nil {
			s.logger.Warn("market_cap_usd compute failed",
				"err", err, "asset_key", key, "price", usdPrice)
		} else if capExceedsObservedTurnover(mc, detail.VolumeUSD24h, s.MaxMarketCapVolumeRatio) {
			// The ceiling tests the computed FIGURE, so unlike the floor it can only run here;
			// detail and listing must agree on the verdict.
			detail.MarketCapLowLiquidity = true
		} else {
			detail.MarketCapUSD = &mc
		}
	}
	if snap.MaxSupply != nil {
		if fdv, err := usdMarketValue(snap.MaxSupply, usdPrice, detail.Decimals); err != nil {
			s.logger.Warn("fdv_usd compute failed",
				"err", err, "asset_key", key, "price", usdPrice)
		} else if capExceedsObservedTurnover(fdv, detail.VolumeUSD24h, s.MaxMarketCapVolumeRatio) {
			// FDV is computed over MAX supply, so it can breach the ceiling even when the cap did
			// not. Flag it as the cap branch does: a withheld fdv_usd with no flag would read as
			// "no max_supply on record".
			detail.MarketCapLowLiquidity = true
		} else {
			// FDV is the larger figure, so it clears the ceiling less often than the cap; withholding
			// the cap yet publishing a bigger FDV beside it would defeat the guard.
			detail.FDVUSD = &fdv
		}
	}
}

// lookupUSDPrice consults the PriceReader for the asset's USD price, returning ("", false) on any
// failure; callers treat that as "market_cap unavailable".
//
// No on-chain trade quotes in fiat:USD (they quote in classic USDC or another declared peg), so a
// literal LatestPrice(asset, fiat:USD) returns ErrPriceNotFound for nearly every asset. On a miss
// this walks usd_pegged_classic_assets and rewrites asset/fiat:USD to asset/<peg>, mirroring the
// handler's tryStablecoinFiatProxy, which the F2 paths bypass.
func (s *Server) lookupUSDPrice(ctx context.Context, asset canonical.Asset) (string, bool) {
	l := s.lookupUSDPriceWithSources(ctx, asset)
	return l.price, l.price != ""
}

// usdPriceLookup is [Server.lookupUSDPriceWithSources]'s verdict: served
// (price set), withheld (withheld set) or absent (neither). Withheld is
// its own state so a withheld asset cannot read as one that never traded.
type usdPriceLookup struct {
	price    string
	sources  int
	withheld PriceWithheldReason
}

// lookupUSDPriceWithSources is [Server.lookupUSDPrice] plus the number of DISTINCT venues behind
// the price (the liquidity signal for [dustLiquiditySuppressed]) and the withholding verdict.
// lookupUSDPrice wraps it for callers that can only use a served price.
func (s *Server) lookupUSDPriceWithSources(ctx context.Context, asset canonical.Asset) usdPriceLookup {
	if s.Prices == nil {
		// Prices is independently optional, and populateChange24h reaches here without
		// populatePriceUSD's guard; a Prices==nil, Change24h!=nil wiring must not nil-panic.
		return usdPriceLookup{}
	}
	if asset.Equal(defaultPriceQuote) {
		// fiat:USD priced against fiat:USD is meaningless;
		// short-circuit before the reader rejects it.
		return usdPriceLookup{}
	}
	// Alias-aware read, the SAME resolution /v1/price uses. XLM has two canonical forms (`native`
	// and `crypto:XLM`) and the aggregator writes the fresh CEX VWAP under whichever matches its
	// pair set; a bare LatestPrice(native, fiat:USD) would miss it and fall to the SDEX proxy,
	// diverging /v1/assets/native from /v1/price by ~0.2%. Non-aliased assets are unaffected.
	snap, sources, _, err := s.readPriceWithAliases(ctx, s.Prices, asset, defaultPriceQuote)
	if errors.Is(err, ErrPriceWithheld) {
		// Withheld beats every fallback, as on /v1/price: the proxy below
		// would re-serve the refused market through a side door.
		return usdPriceLookup{withheld: priceWithheldReason(err)}
	}
	if err == nil && snap.Price != "" {
		// readPriceWithAliases returns the RAW asset/fiat:USD ratio. This is the chokepoint for all
		// THREE F2 USD fields (price_usd, market_cap_usd, fdv_usd), so normalizing decimals here
		// fixes each at once; it is a no-op for a 7dp asset. The proxy branch below normalizes
		// itself.
		s.normalizeRawPriceSnapshot(&snap, asset, defaultPriceQuote)
		return usdPriceLookup{price: snap.Price, sources: len(sources)}
	}
	// Stablecoin-fiat proxy fallback, already decimals-normalized inside tryStablecoinFiatProxy:
	// do NOT re-apply.
	proxy, proxySources, ok, withheld := s.tryStablecoinFiatProxy(ctx, asset, defaultPriceQuote)
	if ok && proxy.Price != "" {
		return usdPriceLookup{price: proxy.Price, sources: len(proxySources)}
	}
	if withheld {
		// The verdict /v1/price and the tip report for a withheld peg leg.
		return usdPriceLookup{withheld: PriceWithheldUpstreamLeg}
	}
	return usdPriceLookup{}
}

// populateChange24h fills detail.Change24hPct via the [Change24hReader] and the market-cap
// [PriceReader]. Best-effort: no current price or no 24h-ago bucket leaves the field null, and
// only real read errors log at WARN. USD-against-USD is meaningless; lookupUSDPrice already
// returns ("", false) for it. Reports whether the 24h-ago read failed.
func (s *Server) populateChange24h(ctx context.Context, detail *AssetDetail, asset canonical.Asset) (failed bool) {
	if s.Change24h == nil {
		return false
	}
	currStr, ok := s.lookupUSDPrice(ctx, asset)
	if !ok {
		return false
	}
	thenStr, err := s.Change24h.USDPrice24hAgo(ctx, asset)
	if errors.Is(err, ErrChange24hUnavailable) {
		// Asset first traded < 24h ago, or retention pruned the row.
		// Silent — feature unavailable for this asset.
		return false
	}
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.logger.Warn("change_24h_pct lookup failed", "err", err, "asset", asset.String())
		}
		return true
	}
	pct, err := pctChange(currStr, thenStr)
	if err != nil {
		s.logger.Warn("change_24h_pct compute failed",
			"err", err, "asset", asset.String(), "now", currStr, "then", thenStr)
		return false
	}
	detail.Change24hPct = &pct
	return false
}

// pctChange returns (now - then) / then * 100 as a signed decimal string with two fractional
// digits ("+1.27", "-0.05", "0.00"). `then` must be > 0. Pure big.Rat math, so extreme prices stay
// precise; the explicit "+" lets consumers tell "0.00" (no change) from a missing field.
func pctChange(nowStr, thenStr string) (string, error) {
	now, ok := new(big.Rat).SetString(nowStr)
	if !ok {
		return "", fmt.Errorf("pctChange: parse now %q", nowStr)
	}
	then, ok := new(big.Rat).SetString(thenStr)
	if !ok {
		return "", fmt.Errorf("pctChange: parse then %q", thenStr)
	}
	if then.Sign() <= 0 {
		return "", fmt.Errorf("pctChange: then %q must be > 0", thenStr)
	}
	delta := new(big.Rat).Sub(now, then)
	pct := new(big.Rat).Quo(delta, then)
	pct.Mul(pct, big.NewRat(100, 1))
	out := pct.FloatString(2)
	// FloatString takes the '-' from the Rat's sign, not the rounded digits, so a sub-cent negative
	// delta must collapse "-0.00" to "0.00", as the positive branch never emits "+0.00".
	switch {
	case out == "-0.00":
		out = "0.00"
	case pct.Sign() > 0 && out != "0.00":
		out = "+" + out
	}
	return out, nil
}

// sep1DeclaredMaxResolver adapts the SEP-1 [[CURRENCIES]] fields already stamped on the
// AssetDetail into [supply.MetadataResolver], without a second issuers-table read.
//
// Precedence: max_number beats fixed_number (a fixed total implies the cap); is_unlimited=true
// blocks both, since a declared number beside it is contradictory. SEP-1 declares DISPLAY units
// but the snapshot carries RAW units, so the resolver scales by 10^detail.Decimals; unscaled it
// would understate max_supply and FDV by 10^7 for classic assets.
type sep1DeclaredMaxResolver struct {
	detail *AssetDetail
}

// SEP1MaxSupply implements [supply.MetadataResolver]. It never errors: every junk shape
// (unparseable, negative, fractional raw units) degrades to ok=false rather than fabricating.
func (r sep1DeclaredMaxResolver) SEP1MaxSupply(_ context.Context, _ canonical.Asset) (string, bool, error) {
	d := r.detail
	if d == nil {
		return "", false, nil
	}
	if d.IsUnlimited != nil && *d.IsUnlimited {
		return "", false, nil
	}
	declared := ""
	if d.MaxNumber != nil {
		declared = strings.TrimSpace(*d.MaxNumber)
	}
	if declared == "" && d.FixedNumber != nil {
		declared = strings.TrimSpace(*d.FixedNumber)
	}
	if declared == "" {
		return "", false, nil
	}
	raw, ok := sep1DisplayToRawUnits(declared, d.Decimals)
	if !ok {
		return "", false, nil
	}
	return raw, true, nil
}

// sep1DisplayToRawUnits converts a SEP-1 display-unit decimal string to a raw-unit integer string
// at the asset's decimals. ok=false unless the result is a non-negative INTEGER: unparseable
// input, negatives, and more fractional digits than the asset has decimals (fractional stroops
// are junk, not roundable). Rational ("1/3") and e-notation are rejected because stellar.toml
// numerics are plain decimals and big.Rat.SetString would silently accept both.
func sep1DisplayToRawUnits(display string, decimals int) (string, bool) {
	if decimals < 0 || strings.ContainsAny(display, "/eE") {
		return "", false
	}
	rat, ok := new(big.Rat).SetString(display)
	if !ok || rat.Sign() < 0 {
		return "", false
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	rat.Mul(rat, new(big.Rat).SetInt(scale))
	if !rat.IsInt() {
		return "", false
	}
	return rat.Num().String(), true
}

// usdMarketValue computes amountStroops x usdPriceStr / 10^decimals as a decimal string with two
// fractional digits. Pure big.Rat math, so huge supplies and tiny prices stay precise. It errors
// on an unparseable price or negative decimals; amountStroops==0 yields "0.00", a legitimate
// "no circulating supply".
//
// A NEGATIVE result is an error, matching [computeMarketCapUSD] on the listing path: burns
// exceeding mints is a data error, and every caller leaves the field unset rather than serve it.
func usdMarketValue(amountStroops *big.Int, usdPriceStr string, decimals int) (string, error) {
	if amountStroops == nil {
		return "", errors.New("usdMarketValue: amountStroops is nil")
	}
	if decimals < 0 {
		return "", fmt.Errorf("usdMarketValue: negative decimals (%d)", decimals)
	}
	price, ok := new(big.Rat).SetString(usdPriceStr)
	if !ok {
		return "", fmt.Errorf("usdMarketValue: parse price %q", usdPriceStr)
	}

	// amount × price
	valueRat := new(big.Rat).Mul(new(big.Rat).SetInt(amountStroops), price)

	// divide by 10^decimals
	if decimals > 0 {
		divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
		valueRat.Quo(valueRat, new(big.Rat).SetInt(divisor))
	}

	if valueRat.Sign() < 0 {
		return "", fmt.Errorf("usdMarketValue: negative market value (amount=%s price=%s)",
			amountStroops.String(), usdPriceStr)
	}

	return valueRat.FloatString(2), nil
}
