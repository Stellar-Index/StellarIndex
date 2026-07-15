package v1

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// assetExtensionTimeout caps the total wall time for the
// asset-catalogue overlay on /v1/assets/{id}. Five reader calls
// run in parallel; each is bounded by this shared deadline.
const assetExtensionTimeout = 4 * time.Second

// applyAssetExtensionFields lifts the trailing-window activity +
// history fields from the assetsReader catalogue onto AssetDetail. Skipped
// for fiat:* / external:* assets (no asset-catalogue row) and when no
// AssetsReader is wired.
//
// All sub-fetches run in parallel and are best-effort: individual
// failures log at Debug level and leave the affected field nil. A
// missing asset-catalogue row (the asset has never traded) is the common case
// and isn't an error — it just leaves all the extension fields
// nil/empty.
//
// Same readers /v1/coins/{slug} uses — wiring is identical, the
// only difference is the lookup key (asset_id from the URL path
// here vs slug there).
// assetExtensionResults holds the parallel-fetch results. Separated
// from applyAssetExtensionFields to keep that function's cognitive
// complexity under the gocognit ceiling.
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
	if s.assetsReader == nil || asset.Type == canonical.AssetFiat {
		return
	}
	assetID := asset.String()

	cctx, cancel := context.WithTimeout(ctx, assetExtensionTimeout)
	defer cancel()

	row, rowErr := s.lookupAssetRow(cctx, asset, assetID)
	results := s.fetchAssetExtensionResults(cctx, assetID)

	s.applyAssetRowToDetail(detail, row, rowErr, assetID)
	applyAssetExtensionResults(detail, results)
	s.logAssetExtensionFailures(assetID, results)
}

// fetchAssetExtensionResults runs the 6 reader calls in parallel.
func (s *Server) fetchAssetExtensionResults(ctx context.Context, assetID string) assetExtensionResults {
	var (
		r  assetExtensionResults
		wg sync.WaitGroup
	)
	wg.Add(6)
	go func() {
		defer wg.Done()
		r.topMarkets, r.topMarketsErr = s.assetsReader.GetAssetTopMarkets(ctx, assetID, 5)
	}()
	go func() {
		defer wg.Done()
		r.hist24, r.hist24Err = s.assetsReader.GetAssetPriceHistory24h(ctx, assetID)
	}()
	go func() {
		defer wg.Done()
		r.hist7d, r.hist7dErr = s.assetsReader.GetAssetPriceHistory7d(ctx, assetID)
	}()
	go func() {
		defer wg.Done()
		r.marketsCount, r.marketsErr = s.assetsReader.GetAssetMarketsCount(ctx, assetID)
	}()
	go func() {
		defer wg.Done()
		r.tradeCount, r.tradeCountErr = s.assetsReader.GetAssetTradeCount24h(ctx, assetID)
	}()
	go func() {
		defer wg.Done()
		r.ath, r.athErr = s.assetsReader.GetAssetATH(ctx, assetID)
	}()
	wg.Wait()
	return r
}

// applyAssetRowToDetail mirrors scalar fields from AssetRow onto
// AssetDetail. sql.ErrNoRows is the expected "no asset-catalogue row" case —
// silent skip. Other errors are logged at Debug.
func (s *Server) applyAssetRowToDetail(detail *AssetDetail, row timescale.AssetRow, err error, assetID string) {
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.Debug("asset extension: row lookup failed",
				"asset_id", assetID, "err", err)
		}
		return
	}
	// Fill PriceUSD from the asset-catalogue row ONLY when the canonical
	// price path (F2 populatePriceUSD → lookupUSDPrice, run earlier)
	// left it nil. The asset-catalogue row's USD price is the listing-query
	// COALESCE(direct_usd, asset_vs_xlm × xlm_usd) — for native XLM
	// its xlm_usd CTE mixes the SDEX (native/USDC) and CEX
	// (native/fiat:USD) pairs and picks the latest bucket, which
	// diverged from the canonical /v1/price CEX VWAP by ~0.2%.
	// Yielding to the already-set canonical value keeps
	// /v1/assets/native in agreement with /v1/price and
	// /v1/assets/crypto:XLM, while still pricing the XLM-triangulated
	// long tail (SHX, AQUA, …) that the canonical reader can't reach.
	if row.PriceUSD != nil && detail.PriceUSD == nil {
		detail.PriceUSD = row.PriceUSD
	}
	if row.Change1hPct != nil {
		detail.Change1hPct = row.Change1hPct
	}
	if row.Change7dPct != nil {
		detail.Change7dPct = row.Change7dPct
	}
	if reason := scamReason(row.IssuerGStrkey); reason != "" {
		detail.IssuerScamReason = reason
	}
	// Identity + activity metadata. Mirrors AssetSummary scalars so
	// the explorer's asset-detail page can drop its parallel
	// /v1/coins/{slug} fetch (R-018 finish — consumer migration).
	if row.Slug != "" {
		detail.Slug = row.Slug
	}
	if row.FirstSeenLedger != 0 {
		v := row.FirstSeenLedger
		detail.FirstSeenLedger = &v
	}
	if row.LastSeenLedger != 0 {
		v := row.LastSeenLedger
		detail.LastSeenLedger = &v
	}
	if row.ObservationCount != 0 {
		v := row.ObservationCount
		detail.ObservationCount = &v
	}
}

// applyAssetExtensionResults populates the array-shaped fields from
// the parallel-fetch results. Each field is independent — one
// failure doesn't fail the others.
func applyAssetExtensionResults(detail *AssetDetail, r assetExtensionResults) {
	if r.topMarketsErr == nil && len(r.topMarkets) > 0 {
		detail.TopMarkets = topMarketsToWire(r.topMarkets)
	}
	if r.hist24Err == nil && len(r.hist24) > 0 {
		detail.PriceHistory24h = assetPointsToWire(r.hist24)
	}
	if r.hist7dErr == nil && len(r.hist7d) > 0 {
		detail.PriceHistory7d = assetPointsToWire(r.hist7d)
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
		detail.ATH = &AssetATH{USD: r.ath.USD, At: r.ath.At}
	}
}

// logAssetExtensionFailures emits one Debug line per failed sub-fetch
// so an operator can correlate cold-cache spikes without 6 separate
// log helpers inside the parallel-fetch goroutines.
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

// lookupAssetRow picks the right AssetsReader method based on the
// asset shape: native short-circuits to GetNativeAssetRow; everything
// else uses GetAssetByAssetID.
func (s *Server) lookupAssetRow(ctx context.Context, asset canonical.Asset, assetID string) (timescale.AssetRow, error) {
	if asset.Type == canonical.AssetNative {
		return s.assetsReader.GetNativeAssetRow(ctx)
	}
	return s.assetsReader.GetAssetByAssetID(ctx, assetID)
}

// assetPointsToWire projects the storage-layer price points onto the
// API wire shape — same field rename (Bucket → T, USDPrice → P) as
// /v1/coins uses.
func assetPointsToWire(pts []timescale.AssetPricePoint) []AssetPricePoint {
	out := make([]AssetPricePoint, len(pts))
	for i, p := range pts {
		out[i] = AssetPricePoint{T: p.T, P: p.P}
	}
	return out
}
