package v1

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
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
	rates      map[uint32]blend.ReserveData // reserve state written in each ledger
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

// underlying converts a b/dToken amount with the reserve state written in the fill's own ledger: the contract accrues before a
// fill and stores the reserve after, so that state carries the rate the fill used. Any other ledger's rate is not the fill's.
func (m *liqMarks) underlying(kind string, ledger uint32, amount *big.Int) (*big.Int, string) {
	if kind == auctionLegUnderlying {
		return amount, ""
	}
	if m == nil || m.rateReason != "" {
		return nil, auctionUSDRateUnread
	}
	d, ok := m.rates[ledger]
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
	u, reason := m.underlying(kind, f.Ledger, n)
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

// liquidationMarks reads each asset's marks over the page's window: one price series and one reserve history read per asset.
func (s *Server) liquidationMarks(ctx context.Context, pool, protocol string, fills []timescale.LendingLiquidationFill, flags *Flags) map[string]*liqMarks {
	assets := liquidationAssets(protocol, fills)
	out := make(map[string]*liqMarks, len(assets))
	if len(assets) == 0 {
		return out
	}
	oldest, newest := fills[len(fills)-1].Ts, fills[0].Ts
	for asset, needRate := range assets {
		m := s.liquidationPrices(ctx, asset, oldest, flags)
		if needRate {
			m.rates, m.rateReason = s.liquidationRates(ctx, pool, asset, oldest, newest, flags)
		}
		out[asset] = m
	}
	return out
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

// liquidationRates reads the reserve's hourly state history over the fills' window and keeps each state by the ledger that wrote it.
func (s *Server) liquidationRates(ctx context.Context, pool, asset string, oldest, newest time.Time, flags *Flags) (map[uint32]blend.ReserveData, string) {
	hr, ok := s.Explorer.(lendingHistoryReader)
	if !ok {
		return nil, auctionUSDRateUnread
	}
	to := newest.Add(time.Hour)
	from := oldest
	if to.Sub(from) > lendingHistoryMaxHourlyWindow {
		from = to.Add(-lendingHistoryMaxHourlyWindow) // older fills find no state: rate_at_fill_unrecorded
	}
	h, err := hr.LendingReserveHistory(ctx, pool, asset, clickhouse.LendingHistoryQuery{From: from, To: to, MaxPoints: lendingHistoryMaxPoints})
	if err != nil {
		s.logger.Warn("lending liquidations: reserve history read failed", "pool", pool, "asset", asset, "err", err)
		flags.Degraded = true
		return nil, auctionUSDRateUnread
	}
	out := make(map[uint32]blend.ReserveData, len(h.Points))
	for _, p := range h.Points {
		if !p.Removed {
			out[p.StateLedger] = p.State.Data
		}
	}
	return out, ""
}
