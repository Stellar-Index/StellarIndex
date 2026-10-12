package v1

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// assetExtensionTimeout is the shared deadline for the six parallel reader calls behind the
// /v1/assets/{id} catalogue overlay (see [Server.fetchAssetExtensionResults]).
const assetExtensionTimeout = 4 * time.Second

// applyAssetExtensionFields lifts trailing-window activity and history fields onto AssetDetail, best-effort (failure leaves the
// field nil; skipped for fiat:*/external:*). assetExtensionResults holds the parallel-fetch results, split out for the gocognit
// ceiling.
type assetExtensionResults struct {
	topMarkets    []timescale.AssetTopMarket
	topMarketsErr error
	hist24        []timescale.AssetPricePoint
	hist24Err     error
	hist7d        []timescale.AssetPricePoint
	hist7dErr     error
	marketsCount  int64
	marketsErr    error
	tradeCount    int64
	tradeCountErr error
	ath           *timescale.AssetATH
	athErr        error
}

func (s *Server) applyAssetExtensionFields(ctx context.Context, detail *AssetDetail, asset canonical.Asset) {
	if s.AssetsReader == nil || asset.Type == canonical.AssetFiat {
		return
	}
	assetID := asset.String()

	cctx, cancel := context.WithTimeout(ctx, assetExtensionTimeout)
	defer cancel()

	row, rowErr := s.lookupAssetRow(cctx, asset, assetID)
	results := s.fetchAssetExtensionResults(cctx, assetID)

	s.applyAssetRowToDetail(cctx, detail, asset, row, rowErr, assetID)
	s.applyAssetExtensionResults(detail, asset, results)
	s.logAssetExtensionFailures(assetID, results)
}

// fetchAssetExtensionResults runs the six reader calls in parallel. Each goroutine is DETACHED from the handler (outside
// middleware.Recoverer), so worker.Recover leaves the field zero rather than letting a reader panic kill the process. Nothing
// is cached.
func (s *Server) fetchAssetExtensionResults(ctx context.Context, assetID string) assetExtensionResults {
	var (
		r  assetExtensionResults
		wg sync.WaitGroup
	)
	wg.Add(6)
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-asset-extension-top-markets")
		r.topMarkets, r.topMarketsErr = s.AssetsReader.GetAssetTopMarkets(ctx, assetID, 5)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-asset-extension-history-24h")
		r.hist24, r.hist24Err = s.AssetsReader.GetAssetPriceHistory24h(ctx, assetID)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-asset-extension-history-7d")
		r.hist7d, r.hist7dErr = s.AssetsReader.GetAssetPriceHistory7d(ctx, assetID)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-asset-extension-markets-count")
		r.marketsCount, r.marketsErr = s.AssetsReader.GetAssetMarketsCount(ctx, assetID)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-asset-extension-trade-count")
		r.tradeCount, r.tradeCountErr = s.AssetsReader.GetAssetTradeCount24h(ctx, assetID)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-asset-extension-ath")
		r.ath, r.athErr = s.AssetsReader.GetAssetATH(ctx, assetID)
	}()
	wg.Wait()
	return r
}

// applyAssetRowToDetail mirrors scalar AssetRow fields onto AssetDetail. sql.ErrNoRows is the
// expected "no catalogue row" case; other errors log at Debug.
func (s *Server) applyAssetRowToDetail(ctx context.Context, detail *AssetDetail, asset canonical.Asset, row timescale.AssetRow, err error, assetID string) {
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.Debug("asset extension: row lookup failed",
				"asset_id", assetID, "err", err)
			return
		}
		// NO catalogue row, which is the case transitive pricing exists for: `classic_assets`
		// requires a G-issuer, so a Soroban-native contract asset can NEVER have a row. Returning
		// now would leave its price_usd null.
		s.fillTransitivePrice(ctx, detail, asset, assetID)
		return
	}
	// The row's price_usd is computed outside /v1/price's gated read path and would bypass the
	// thin-market substance gate, giving a dust-authored market a headline. Apply the SAME per-pair
	// verdict as the listing (listingPriceAllowed, cached ~60s). The change pills derive from that
	// price so a gated-out row loses them too; the declared-peg fill downstream (nil PriceUSD only)
	// can still supply the operator-declared basis.
	priceAllowed := s.Substance == nil || s.listingPriceAllowed(ctx, asset)
	// Fill PriceUSD from the row ONLY when the canonical price path left it nil: the row's price is
	// COALESCE(direct_usd, asset_vs_xlm x xlm_usd) and its native-XLM anchor prefers SDEX over CEX,
	// so it can differ from the canonical VWAP. Yielding keeps /v1/assets/native in agreement with
	// /v1/price while still pricing the XLM-triangulated long tail.
	//
	// The row's price is a RAW prices_1m ratio, so it is decimals-normalised first
	// (normalizeCatalogueReadUSD); a price that must be withheld leaves PriceUSD nil for the
	// transitive fill below, which reads at full precision.
	if priceAllowed && row.PriceUSD != nil && detail.PriceUSD == nil {
		if p, ok := s.normalizeCatalogueReadUSD(*row.PriceUSD, asset); ok {
			detail.PriceUSD = &p
			detail.PriceWithheldReason = ""
		} else {
			// The change pills derive from the price just withheld, and
			// must not outlive it — same rule as the substance gate above.
			priceAllowed = false
		}
	}
	// LAST resort, after the canonical path and the row both declined: a one-hop transitive price
	// that only fills a hole. transitivePriceFor gates BOTH legs (scam + substance) and returns
	// false on every error path, so this call site deliberately has no policy of its own.
	s.fillTransitivePrice(ctx, detail, asset, assetID)
	if priceAllowed && row.Change1hPct != nil {
		detail.Change1hPct = row.Change1hPct
	}
	if priceAllowed && row.Change7dPct != nil {
		detail.Change7dPct = row.Change7dPct
	}
	if row.Slug != "" {
		detail.Slug = row.Slug
	}
	// Ledger fields keep a zero-as-unset guard: ledger 0 does not exist (genesis is 1), so serving
	// `first_seen_ledger: 0` would assert an impossible ledger.
	//
	// observation_count is the opposite: 0 is a LEGITIMATE reading (registered, untraded in the
	// window) and dropping it would blur "no data" with "zero", so report it unless the row holds
	// none (native XLM).
	if row.FirstSeenLedger != 0 {
		v := row.FirstSeenLedger
		detail.FirstSeenLedger = &v
	}
	if row.LastSeenLedger != 0 {
		v := row.LastSeenLedger
		detail.LastSeenLedger = &v
	}
	detail.ObservationCount = observationCountWire(row)
}

// observationCountWire is the served observation_count: the registry's count, including a
// legitimate 0, or nil when the row has none (native XLM, see timescale.AssetRow).
func observationCountWire(row timescale.AssetRow) *int64 {
	if row.ObservationCountUnmeasured {
		return nil
	}
	obs := row.ObservationCount
	return &obs
}

// applyAssetExtensionResults populates the array fields independently. The three PRICE fields (both histories and the ATH)
// arrive as RAW ratios and are normalised for a confirmed non-7-decimals asset (normalizeCatalogueUSD), else they would sit
// unscaled beside price_usd.
func (s *Server) applyAssetExtensionResults(detail *AssetDetail, asset canonical.Asset, r assetExtensionResults) {
	if r.topMarketsErr == nil && len(r.topMarkets) > 0 {
		detail.TopMarkets = topMarketsToWire(r.topMarkets)
	}
	if r.hist24Err == nil && len(r.hist24) > 0 {
		detail.PriceHistory24h = s.normalizedAssetPointsToWire(r.hist24, asset)
	}
	if r.hist7dErr == nil && len(r.hist7d) > 0 {
		detail.PriceHistory7d = s.normalizedAssetPointsToWire(r.hist7d, asset)
	}
	if r.marketsErr == nil {
		v := r.marketsCount
		detail.MarketsCount = &v
	}
	if r.tradeCountErr == nil {
		v := r.tradeCount
		detail.TradeCount24h = &v
	}
	if r.athErr == nil && r.ath != nil {
		// prices_1d's vwap has full NUMERIC precision (no SQL ROUND), so the scale-up is exact and
		// needs no precision floor.
		if usd, ok := s.normalizeCatalogueUSD(r.ath.USD, asset, false); ok {
			detail.ATH = &AssetATH{USD: usd, At: r.ath.At}
		}
	}
}

// catalogueUSDRoundDigits is the ROUND(..., 10) the catalogue SQL applies to USD price text (not the ATH); it happens BEFORE
// decimals normalisation can.
const catalogueUSDRoundDigits = 10

// catalogueUSDMinQuanta is the fewest rounding quanta a SQL-rounded raw ratio must keep for a scaled-UP value to publish; 1000
// bounds the carried error at 0.05%.
const catalogueUSDMinQuanta = 1000

// normalizeCatalogueUSD applies 10^(decimals - standard) normalisation to a USD price made from RAW ratios (classic and native
// XLM pass through). ok=false means withhold: an unparseable or non-positive flagged value cannot be corrected, and scaling UP
// a `rounded` price multiplies SQL rounding error, so below catalogueUSDMinQuanta the point becomes a gap.
func (s *Server) normalizeCatalogueUSD(value string, asset canonical.Asset, rounded bool) (string, bool) {
	baseDec := aggregate.ResolveDecimals(s.NonstandardDecimals, asset)
	quoteDec := aggregate.ResolveDecimals(s.NonstandardDecimals, defaultPriceQuote)
	if baseDec == quoteDec {
		return value, true
	}
	raw := ratFromDecimal(value)
	if raw == nil || raw.Sign() <= 0 {
		return "", false
	}
	if rounded && baseDec > quoteDec {
		quanta := new(big.Rat).Mul(raw, aggregate.DecimalsAdjustment(catalogueUSDRoundDigits, 0))
		if quanta.Cmp(big.NewRat(catalogueUSDMinQuanta, 1)) < 0 {
			return "", false
		}
	}
	return ratToDecimal(aggregate.AdjustPrice(raw, baseDec, quoteDec), ohlcPriceDigits), true
}

// normalizedAssetPointsToWire is [assetPointsToWire] with each priced point normalised via [Server.normalizeCatalogueReadUSD];
// a withheld point becomes a null-priced bucket, keeping the client's grid.
func (s *Server) normalizedAssetPointsToWire(pts []timescale.AssetPricePoint, asset canonical.Asset) []AssetPricePoint {
	out := assetPointsToWire(pts)
	for i := range out {
		if out[i].P == nil {
			continue
		}
		if p, ok := s.normalizeCatalogueReadUSD(*out[i].P, asset); ok {
			out[i].P = &p
		} else {
			out[i].P = nil
		}
	}
	return out
}

// logAssetExtensionFailures emits one Debug line per failed sub-fetch to correlate cold-cache
// spikes, keeping logging out of the parallel goroutines.
func (s *Server) logAssetExtensionFailures(assetID string, r assetExtensionResults) {
	for _, e := range [...]struct {
		err error
		tag string
	}{
		{r.topMarketsErr, "top_markets"},
		{r.hist24Err, "price_history_24h"},
		{r.hist7dErr, "price_history_7d"},
		{r.marketsErr, "markets_count"},
		{r.tradeCountErr, "trade_count_24h"},
		{r.athErr, "ath"},
	} {
		if e.err != nil {
			s.logger.Debug("asset extension: "+e.tag+" failed",
				"asset_id", assetID, "err", e.err)
		}
	}
}

// topMarketsToWire projects storage rows onto the API shape.
func topMarketsToWire(in []timescale.AssetTopMarket) []AssetTopMarket {
	out := make([]AssetTopMarket, len(in))
	for i, m := range in {
		out[i] = AssetTopMarket{
			Counterparty:  m.Counterparty,
			Side:          m.Side,
			Volume24hUSD:  m.Volume24hUSD,
			TradeCount24h: m.TradeCount24h,
		}
	}
	return out
}

// lookupAssetRow reads native via GetNativeAssetRow and everything else via GetAssetByAssetID.
func (s *Server) lookupAssetRow(ctx context.Context, asset canonical.Asset, assetID string) (timescale.AssetRow, error) {
	if asset.Type == canonical.AssetNative {
		return s.AssetsReader.GetNativeAssetRow(ctx)
	}
	return s.AssetsReader.GetAssetByAssetID(ctx, assetID)
}

// assetPointsToWire projects storage price points onto the wire shape (Bucket → T, USDPrice → P),
// as /v1/coins does.
func assetPointsToWire(pts []timescale.AssetPricePoint) []AssetPricePoint {
	out := make([]AssetPricePoint, len(pts))
	for i, p := range pts {
		out[i] = AssetPricePoint{T: p.T, P: p.P}
	}
	return out
}

// fillTransitivePrice fills a one-hop USD price when nothing else could price the asset. Reached from BOTH arms of
// applyAssetRowToDetail (row declined, or no row); no-ops unless PriceUSD is nil, and transitivePriceFor gates both legs on
// scam and substance.
func (s *Server) fillTransitivePrice(ctx context.Context, detail *AssetDetail, asset canonical.Asset, assetID string) {
	if detail == nil || detail.PriceUSD != nil {
		return
	}
	if p, ok := s.transitivePriceFor(ctx, asset, assetID); ok {
		detail.PriceUSD = &p
		detail.PriceBasis = priceBasisTransitive
		detail.PriceWithheldReason = ""
	}
}
