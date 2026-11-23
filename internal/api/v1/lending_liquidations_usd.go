package v1

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// liquidationPriceMaxAge bounds how long before a fill the price bucket may have closed: the /v1/price/at honesty cap.
const liquidationPriceMaxAge = priceAtMaxLookback

// Reasons a liquidation leg, or a whole fill, has no USD value.
const (
	liqReasonAmountsNotEmitted = "amounts_not_emitted"
	liqReasonBackstopToken     = "backstop_token_not_valued_at_event_time"
	liqReasonUnparseable       = "amount_unparseable"
	liqReasonRateUnrecorded    = "rate_at_fill_unrecorded"
	liqReasonPriceWithheld     = "price_withheld"
	liqReasonPriceThin         = "price_thin_market"
	liqReasonPriceUnread       = "price_unread"
	liqReasonNoPrice           = "no_price_within_bound"
)

// liqPrice is one closed USD price bucket: its VWAP per whole token (at scale decimals) and the time it closed.
type liqPrice struct {
	close time.Time
	price string
}

// liqMarks is what one asset is valued with across a page: its closed USD price buckets and its reserve state per ledger.
type liqMarks struct {
	prices      []liqPrice // ascending by close
	priceReason string     // set when no series could be used at all
	scale       int
	// withheldFrom: an exclusion dropped a bucket from here on, so a later fill could be priced across the hole.
	withheldFrom time.Time
	// cappedAt: the series is incomplete after it.
	cappedAt   time.Time
	rates      map[clickhouse.LendingTxRef]blend.ReserveData // reserve state each fill's transaction stored
	rateReason string
}

// priceAt is the latest bucket that closed at or before t and no more than liquidationPriceMaxAge before it.
func (m *liqMarks) priceAt(t time.Time) (liqPrice, string) {
	switch {
	case m == nil:
		return liqPrice{}, liqReasonPriceUnread
	case m.priceReason != "":
		return liqPrice{}, m.priceReason
	case !m.withheldFrom.IsZero() && !t.Before(m.withheldFrom):
		return liqPrice{}, liqReasonPriceWithheld
	case !m.cappedAt.IsZero() && t.After(m.cappedAt):
		return liqPrice{}, liqReasonPriceUnread
	}
	i := sort.Search(len(m.prices), func(i int) bool { return m.prices[i].close.After(t) }) - 1
	if i < 0 || t.Sub(m.prices[i].close) > liquidationPriceMaxAge {
		return liqPrice{}, liqReasonNoPrice
	}
	return m.prices[i], ""
}

// underlying converts a b/dToken amount with the reserve state the fill's own transaction stored. The fill accrues the reserve
// before moving positions and stores it after, and moving positions changes no rate, so that image carries the fill's rate. An
// earlier write lacks the fill's accrual; a later transaction in the same ledger may default debt (b_rate falls).
func (m *liqMarks) underlying(kind string, tx clickhouse.LendingTxRef, amount *big.Int) (*big.Int, string) {
	if kind == auctionLegUnderlying {
		return amount, ""
	}
	if m == nil || m.rateReason != "" {
		return nil, auctionUSDRateUnread
	}
	d, ok := m.rates[tx]
	switch {
	case !ok:
		return nil, liqReasonRateUnrecorded
	case kind == auctionLegBToken && d.BRate != nil:
		return d.BTokensToUnderlying(amount), "" // floor, as the contract rounds
	case kind == auctionLegDToken && d.DRate != nil:
		return d.DTokensToUnderlying(amount), "" // ceiling, as the contract rounds
	}
	return nil, liqReasonRateUnrecorded
}

// liqValuer values a page's fills against per-asset marks.
type liqValuer struct {
	protocol string
	marks    map[string]*liqMarks
}

// leg values one bid or lot entry at the fill.
func (lv liqValuer) leg(kind, asset, amount string, f timescale.LendingLiquidationFill) (LendingLiquidationLeg, *big.Rat) {
	l := LendingLiquidationLeg{Asset: asset, Kind: kind, Amount: amount}
	fail := func(reason string) (LendingLiquidationLeg, *big.Rat) {
		l.Reason = strRef(reason)
		return l, nil
	}
	switch kind {
	case auctionLegUnknown:
		return fail(auctionUSDKindUnknown)
	case auctionLegBackstop:
		return fail(liqReasonBackstopToken)
	}
	n, ok := parseBig(amount)
	if !ok || n.Sign() < 0 {
		return fail(liqReasonUnparseable)
	}
	m := lv.marks[asset]
	u, reason := m.underlying(kind, liqTxRef(f), n)
	if u == nil {
		return fail(reason)
	}
	l.Underlying = strRef(u.String())
	p, reason := m.priceAt(f.Ts)
	if reason != "" {
		return fail(reason)
	}
	usd, err := usdValueRat(u, p.price, m.scale)
	if err != nil {
		return fail(liqReasonNoPrice)
	}
	at := WireTime(p.close)
	l.PriceUSD, l.PriceAt, l.USD = strRef(p.price), &at, strRef(ratString(usd, 2))
	return l, usd
}

// side values one map in asset order: the exact sum of its valued legs, nil when none is valued, lowerBound when one is left out.
func (lv liqValuer) side(kind string, amounts map[string]string, f timescale.LendingLiquidationFill) ([]LendingLiquidationLeg, *big.Rat, bool) {
	assets := make([]string, 0, len(amounts))
	for a := range amounts {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	legs := make([]LendingLiquidationLeg, 0, len(assets))
	var sum *big.Rat
	missing := false
	for _, a := range assets {
		l, usd := lv.leg(kind, a, amounts[a], f)
		legs = append(legs, l)
		if usd == nil {
			missing = true
			continue
		}
		if sum == nil {
			sum = new(big.Rat)
		}
		sum.Add(sum, usd)
	}
	if len(assets) == 0 {
		sum = new(big.Rat)
	}
	return legs, sum, missing && sum != nil
}

// value builds one fill's view; the filler's profit is lot - bid only when every leg of both sides is valued.
func (lv liqValuer) value(f timescale.LendingLiquidationFill) LendingLiquidationView {
	v := liquidationView(f)
	if !f.HasAmounts {
		v.USDReason = strRef(liqReasonAmountsNotEmitted)
		return v
	}
	bidKind, lotKind := auctionLegKinds(lv.protocol, f.AuctionType)
	var lot, bid *big.Rat
	v.Lot, lot, v.LotUSDLowerBound = lv.side(lotKind, f.Lot, f)
	v.Bid, bid, v.BidUSDLowerBound = lv.side(bidKind, f.Bid, f)
	if lot != nil {
		v.LotUSD = strRef(ratString(lot, 2))
	}
	if bid != nil {
		v.BidUSD = strRef(ratString(bid, 2))
	}
	if lot != nil && bid != nil && !v.LotUSDLowerBound && !v.BidUSDLowerBound {
		v.FillerProfitUSD = strRef(ratString(new(big.Rat).Sub(lot, bid), 2))
	}
	return v
}

// liquidationAssets is each asset the page's fills need marks for, and whether it needs reserve rates.
func liquidationAssets(protocol string, fills []timescale.LendingLiquidationFill) map[string]bool {
	out := map[string]bool{}
	add := func(kind string, amounts map[string]string) {
		if kind != auctionLegBToken && kind != auctionLegDToken && kind != auctionLegUnderlying {
			return
		}
		for a := range amounts {
			out[a] = out[a] || kind != auctionLegUnderlying
		}
	}
	for _, f := range fills {
		if f.HasAmounts {
			bidKind, lotKind := auctionLegKinds(protocol, f.AuctionType)
			add(bidKind, f.Bid)
			add(lotKind, f.Lot)
		}
	}
	return out
}

// liquidationMarks reads each asset's marks over the page's window: one price series per asset and one reserve read for the page.
func (s *Server) liquidationMarks(ctx context.Context, pool, protocol, model string, fills []timescale.LendingLiquidationFill, flags *Flags) map[string]*liqMarks {
	assets := liquidationAssets(protocol, fills)
	out := make(map[string]*liqMarks, len(assets))
	if len(assets) == 0 {
		return out
	}
	var rateAssets []string
	for asset, needRate := range assets {
		out[asset] = s.liquidationPrices(ctx, asset, fills[len(fills)-1].Ts, flags)
		if needRate {
			rateAssets = append(rateAssets, asset)
		}
	}
	if len(rateAssets) == 0 {
		return out
	}
	rates, reason := s.liquidationRates(ctx, pool, model, rateAssets, fills, flags)
	for _, asset := range rateAssets {
		m := out[asset]
		m.rateReason = reason
		m.rates = map[clickhouse.LendingTxRef]blend.ReserveData{}
		for k, d := range rates {
			if k.Asset == asset {
				m.rates[k.LendingTxRef] = d
			}
		}
	}
	return out
}

// liqTxRef is the fill's transaction as the lake keys it.
func liqTxRef(f timescale.LendingLiquidationFill) clickhouse.LendingTxRef {
	return clickhouse.LendingTxRef{Ledger: f.Ledger, TxHash: strings.ToLower(f.TxHash)}
}

// liquidationStateModel is the lake reserve model of a pool's lineage; "" when none is known.
func liquidationStateModel(protocol, lineageReason string) string {
	switch {
	case protocol == blnt.SourceName:
		return clickhouse.LendingBLNT
	case protocol == blend.SourceName && lineageReason == backstopReasonV1:
		return clickhouse.LendingBlendV1
	case protocol == blend.SourceName:
		return clickhouse.LendingBlendV2
	}
	return ""
}

// liquidationPrices reads the asset's closed hourly USD series from before the oldest fill through the chart walk (alias spellings,
// stablecoin proxies, source exclusions) after the reserve page's SAC and issuer gates.
func (s *Server) liquidationPrices(ctx context.Context, assetC string, oldest time.Time, flags *Flags) *liqMarks {
	asset, ok, unread := s.reserveTokenAsset(ctx, assetC)
	switch {
	case unread:
		flags.Degraded = true
		return &liqMarks{priceReason: liqReasonPriceUnread}
	case !ok:
		return &liqMarks{priceReason: liqReasonNoPrice}
	}
	if flagged, unread := s.reserveIssuerFlagged(ctx, asset); unread {
		flags.Degraded = true
		return &liqMarks{priceReason: liqReasonPriceUnread}
	} else if flagged {
		return &liqMarks{priceReason: liqReasonPriceWithheld}
	}
	return s.liquidationSeries(ctx, assetC, asset, oldest, flags)
}

// liquidationSeries is the walk itself, refused whole when its latest bucket stands on a thin book (the market-cap chart's guard):
// a dust market the live page will not price values no fill either.
func (s *Server) liquidationSeries(ctx context.Context, assetC string, asset canonical.Asset, oldest time.Time, flags *Flags) *liqMarks {
	usd, err := canonical.NewFiatAsset("USD")
	pair, perr := canonical.NewPair(asset, usd)
	if s.History == nil || err != nil || perr != nil {
		return &liqMarks{priceReason: liqReasonNoPrice}
	}
	from := oldest.Add(-liquidationPriceMaxAge)
	gran := chartFitGranularity(chartVWAPGranularityLadder, string(timescale.Granularity1h), s.now().Sub(from))
	width, fixed := chartFixedBucketWidth(gran)
	if !fixed || width > liquidationPriceMaxAge {
		return &liqMarks{priceReason: liqReasonNoPrice}
	}
	points, walk, err := s.chartSeriesPoints(ctx, pair, chartWindow{from: from, gran: gran}, s.chartVWAPReader(gran))
	switch {
	case errors.Is(err, ErrPriceWithheld):
		return &liqMarks{priceReason: liqReasonPriceWithheld}
	case err != nil:
		s.logger.Warn("lending liquidations: price read failed", "asset", assetC, "err", err)
		flags.Degraded = true
		return &liqMarks{priceReason: liqReasonPriceUnread}
	}
	flags.Degraded = flags.Degraded || walk.degraded
	switch thin, unmeasured := s.chartLatestThin(ctx, pair, walk); {
	case thin:
		return &liqMarks{priceReason: liqReasonPriceThin}
	case unmeasured:
		flags.Degraded = true
		return &liqMarks{priceReason: liqReasonPriceUnread}
	}
	m := &liqMarks{scale: aggregate.ResolveDecimals(s.NonstandardDecimals, asset), withheldFrom: walk.withheldFrom, cappedAt: walk.cappedAt}
	for _, p := range points {
		m.prices = append(m.prices, liqPrice{close: p.Bucket.UTC().Add(width), price: p.VWAP})
	}
	sort.Slice(m.prices, func(i, j int) bool { return m.prices[i].close.Before(m.prices[j].close) })
	return m
}

// lendingTxReserveReader reads the reserve state given transactions stored, batched over a page.
type lendingTxReserveReader interface {
	LendingReservesAtTxs(ctx context.Context, pool, protocol string, assets []string, txs []clickhouse.LendingTxRef) (map[clickhouse.LendingTxReserve]blend.ReserveData, error)
}

var _ lendingTxReserveReader = (*clickhouse.ExplorerReader)(nil)

// liquidationRates reads, in one lake query, the reserve state each fill's transaction stored for each asset.
func (s *Server) liquidationRates(ctx context.Context, pool, model string, assets []string, fills []timescale.LendingLiquidationFill, flags *Flags) (map[clickhouse.LendingTxReserve]blend.ReserveData, string) {
	rr, ok := s.Explorer.(lendingTxReserveReader)
	if !ok || model == "" {
		return nil, auctionUSDRateUnread
	}
	txs := make([]clickhouse.LendingTxRef, 0, len(fills))
	for _, f := range fills {
		txs = append(txs, liqTxRef(f))
	}
	out, err := rr.LendingReservesAtTxs(ctx, pool, model, assets, txs)
	if err != nil {
		s.logger.Warn("lending liquidations: reserve state read failed", "pool", pool, "err", err)
		flags.Degraded = true
		return nil, auctionUSDRateUnread
	}
	return out, ""
}
