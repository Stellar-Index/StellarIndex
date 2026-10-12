package explorer

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// AccountCohortView is GET /v1/accounts/{g}/graph/cohort: what the accounts this address created or sponsored went on to hold and do, from the cohort rollup's last cycle.
type AccountCohortView struct {
	Account  string `json:"account"`
	Relation string `json:"relation"`
	// Covered is false when the rollup does not carry this root: a
	// creator under the floor, or an address with no edges in this
	// relation. Cycle still says when the rollup last ran.
	Covered bool                `json:"covered"`
	Cohort  *AccountCohortSizeV `json:"cohort,omitempty"`
	// Holdings is what the cohort holds now, per asset, most widely held
	// first; HoldingsTruncated says the read cap applied.
	Holdings          []AccountCohortHoldingV  `json:"holdings"`
	HoldingsTruncated bool                     `json:"holdings_truncated"`
	Valuation         AccountCohortValuationV  `json:"valuation"`
	Flows             AccountCohortFlowsV      `json:"flows"`
	Contracts         []AccountCohortContractV `json:"contracts"`
	Positions         []AccountCohortPositionV `json:"positions"`
	Cycle             *AccountCohortCycleV     `json:"cycle,omitempty"`
	Note              string                   `json:"note"`
}

// AccountCohortSizeV counts the cohort and how much of it is alive and recently active.
type AccountCohortSizeV struct {
	Accounts     uint64 `json:"accounts"`
	LiveAccounts uint64 `json:"live_accounts"`
	Active30d    uint64 `json:"active_30d"`
	Active90d    uint64 `json:"active_90d"`
	Active365d   uint64 `json:"active_365d"`
}

// AccountCohortHoldingV is the cohort's position in one asset.
type AccountCohortHoldingV struct {
	Asset string `json:"asset"`
	// Kind is native | classic | pool_share. A pool share is a classic
	// liquidity-pool position, served here because it IS a holding and
	// never priced here because a share is not an asset.
	Kind     string  `json:"kind"`
	Holders  uint64  `json:"holders"`
	Balance  string  `json:"balance"`
	PriceUSD *string `json:"price_usd,omitempty"`
	ValueUSD *string `json:"value_usd,omitempty"`
}

// AccountCohortValuationV totals priced holdings at the live price; only the PriceCap largest are looked up, and Degraded means an in-cap lookup was abandoned so the counts are partial.
type AccountCohortValuationV struct {
	TotalUSD         *string `json:"total_usd,omitempty"`
	PricedHoldings   int     `json:"priced_holdings"`
	UnpricedHoldings int     `json:"unpriced_holdings"`
	PriceCap         int     `json:"price_cap"`
	UnpricedOverCap  int     `json:"unpriced_over_cap"`
	Degraded         bool    `json:"degraded,omitempty"`
	Basis            string  `json:"basis"`
}

// AccountCohortFlowsV is the cohort's value moved in and out, by month.
type AccountCohortFlowsV struct {
	Granularity string `json:"granularity"`
	// Assets are broken out per month, most moved first; a month's Movements and ActiveAccounts
	// count EVERY asset. AssetsTruncated says the read cap (clickhouse.CohortFlowAssetsLimit)
	// dropped assets, which are not folded into an "other" bucket because their units differ.
	Assets          []string                  `json:"assets"`
	AssetsTruncated bool                      `json:"assets_truncated"`
	Points          []AccountCohortFlowPointV `json:"points"`
}

// AccountCohortFlowPointV is one month; its USD figures value ONE basket on each basis, absent (never zero) when the basket is empty.
type AccountCohortFlowPointV struct {
	Period         string                    `json:"period"`
	PeriodStart    string                    `json:"period_start"`
	Movements      uint64                    `json:"movements"`
	ActiveAccounts uint64                    `json:"active_accounts"`
	PricedAssets   uint64                    `json:"priced_assets"`
	InflowUSD      *string                   `json:"inflow_usd,omitempty"`
	OutflowUSD     *string                   `json:"outflow_usd,omitempty"`
	InflowUSDThen  *string                   `json:"inflow_usd_then,omitempty"`
	OutflowUSDThen *string                   `json:"outflow_usd_then,omitempty"`
	ByAsset        []AccountCohortAssetFlowV `json:"by_asset"`
}

// AccountCohortAssetFlowV is one asset's month; Inflow/Outflow are decimal strings (whole units when Scaled), USD figures are at today's price and *Then at PriceUSDThen, absent where unpriced.
type AccountCohortAssetFlowV struct {
	Asset          string  `json:"asset"`
	Inflow         string  `json:"inflow"`
	Outflow        string  `json:"outflow"`
	Scaled         bool    `json:"scaled"`
	InflowUSD      *string `json:"inflow_usd,omitempty"`
	OutflowUSD     *string `json:"outflow_usd,omitempty"`
	InflowUSDThen  *string `json:"inflow_usd_then,omitempty"`
	OutflowUSDThen *string `json:"outflow_usd_then,omitempty"`
	PriceUSDThen   *string `json:"price_usd_then,omitempty"`
}

// AccountCohortContractV is one C... contract the cohort moved value through; Protocol is set when the roster claims it, Label when the lake can name it.
type AccountCohortContractV struct {
	ContractID     string `json:"contract_id"`
	Protocol       string `json:"protocol,omitempty"`
	Label          string `json:"label,omitempty"`
	Movements      uint64 `json:"movements"`
	ActiveAccounts uint64 `json:"active_accounts"`
	FirstAt        string `json:"first_at"`
	LastAt         string `json:"last_at"`
}

// AccountCohortPositionV is the cohort's aggregate in one DeFi venue; Amount is the exact sum across holders in the fold's own unit.
type AccountCohortPositionV struct {
	Protocol     string `json:"protocol"`
	PositionKind string `json:"position_kind"`
	Venue        string `json:"venue"`
	Asset        string `json:"asset,omitempty"`
	AssetLabel   string `json:"asset_label,omitempty"`
	Holders      uint64 `json:"holders"`
	Amount       string `json:"amount"`
}

// AccountCohortCycleV dates the snapshot.
type AccountCohortCycleV struct {
	ComputedAt string `json:"computed_at"`
	TipLedger  uint32 `json:"tip_ledger"`
}

const accountCohortNote = "Every figure is the cohort's own ledger footprint as of cycle.computed_at, " +
	"never the root's. holdings and valuation are current balances at the live price; only the " +
	"valuation.price_cap largest holdings by balance are priced (valuation.unpriced_over_cap says " +
	"how many were left unpriced for that reason); a pool " +
	"share is a classic liquidity-pool position and is never priced. flows are derived from the " +
	"movements archive — received minus sent per asset per calendar month (UTC) — so a month " +
	"with no movement emits no point, and the USD figures value each month's quantity at " +
	"today's price, not that month's; the *_usd_then figures value it at price_usd_then — then = " +
	"that month's volume-weighted USD price on this index's own markets — and are absent where " +
	"no USD-quoted market priced the asset that month. A point's own USD figures sum only its " +
	"priced_assets basket — the by_asset rows priced on BOTH bases — so today and then value the " +
	"same assets; a row priced on one basis alone is in neither point sum. active_accounts is a uniqCombined estimate, movements is " +
	"exact. contracts are the C… counterparties of cohort movements: the value-moving subset " +
	"of interaction — a call that moved no balance is not counted. positions are the served " +
	"tier's per-protocol folds joined to the cohort; amount is the fold's own unit summed " +
	"across holders. covered:false means the rollup does not carry this root (a creator under " +
	"the floor, or no edges in this relation), not that the cohort holds nothing."

const accountCohortValuationBasis = "live_vwap_current"

// cohortPricedHoldingsCap bounds how many holdings one request prices. Each price is a live read
// (40-350 ms) and a cohort carries up to clickhouse.CohortHoldingsLimit (400) holdings, so pricing
// them all serially exhausts explorerReadTimeout. The cap takes the largest holdings by balance, a
// proxy for value (ranking by USD value would need pricing everything first); the response
// reports the rest as AccountCohortValuationV.UnpricedOverCap. Flow assets (at most
// clickhouse.CohortFlowAssetsLimit) are priced on top of the cap.
const cohortPricedHoldingsCap = 50

// AccountGraphCohort serves GET /v1/accounts/{g_strkey}/graph/cohort?relation=…
func (h *Handler) AccountGraphCohort(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	relation := r.URL.Query().Get("relation")
	switch relation {
	case clickhouse.CohortRelationCreated, clickhouse.CohortRelationSponsored:
	default:
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid parameter", http.StatusBadRequest,
			"relation must be one of created, sponsored")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	cohort, ok, err := h.Reader.AccountCohort(ctx, g, relation)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-cohort-timeout",
				"Account cohort timed out")
			return
		}
		h.Logger.Error("explorer AccountGraphCohort failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !ok {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/account-cohort-warming",
			"Account cohort warming", http.StatusServiceUnavailable,
			"the cohort rollup hasn't completed its first cycle on this deployment yet; retry shortly")
		return
	}
	_, stale, _ := h.LakeWatermark(ctx)
	view := h.accountCohortView(ctx, cohort)
	h.writeJSONAt(w, view, stale, view.Valuation.Degraded, time.Time{})
}

func (h *Handler) accountCohortView(ctx context.Context, c clickhouse.AccountCohort) AccountCohortView {
	out := AccountCohortView{
		Account:   c.Root,
		Relation:  c.Relation,
		Covered:   c.Covered,
		Holdings:  []AccountCohortHoldingV{},
		Contracts: []AccountCohortContractV{},
		Positions: []AccountCohortPositionV{},
		Flows:     AccountCohortFlowsV{Granularity: "1M", Assets: []string{}, Points: []AccountCohortFlowPointV{}},
		Valuation: AccountCohortValuationV{Basis: accountCohortValuationBasis, PriceCap: cohortPricedHoldingsCap},
		Note:      accountCohortNote,
	}
	if !c.Cycle.ComputedAt.IsZero() {
		out.Cycle = &AccountCohortCycleV{ComputedAt: c.Cycle.ComputedAt.UTC().Format(time.RFC3339), TipLedger: c.Cycle.TipLedger}
	}
	if !c.Covered {
		return out
	}
	out.Cohort = &AccountCohortSizeV{
		Accounts: c.CohortAccounts, LiveAccounts: c.LiveAccounts,
		Active30d: c.Activity.Active30d, Active90d: c.Activity.Active90d, Active365d: c.Activity.Active365d,
	}
	// Labels first, prices last: labels are cheap cached lookups, prices are the expensive live
	// reads that could otherwise exhaust the budget and leave contracts labelled on a dead context.
	out.Contracts = h.cohortContractsView(ctx, c.Contracts)
	out.Positions = h.cohortPositionsView(ctx, c.Positions)
	eligible := cohortPriceable(c.Holdings, c.Flows)
	price, degraded := h.cohortPricer(ctx, eligible)
	out.Holdings, out.Valuation = cohortHoldingsView(c.Holdings, price, eligible)
	out.HoldingsTruncated = len(c.Holdings) >= clickhouse.CohortHoldingsLimit
	out.Flows = cohortFlowsView(c.Flows, price)
	out.Valuation.Degraded = *degraded
	return out
}

// cohortPrice is one asset's live USD rate: Text exactly as the price
// reader served it (what price_usd carries, byte-identical to every
// other surface's price for the asset), Rat the same number for exact
// arithmetic (ADR-0003 — a served dollar never passes through a float).
type cohortPrice struct {
	Text string
	Rat  *big.Rat
}

// cohortPriceFn prices a canonical asset id, or reports that it cannot.
type cohortPriceFn func(asset string) (cohortPrice, bool)

// cohortPriceable is the set of assets one request may look up: the
// cohortPricedHoldingsCap largest priceable holdings by balance, plus
// every priceable flow asset (bounded by the reader's flow-asset cap).
// Pool shares and C… ids are never priceable.
func cohortPriceable(holdings []clickhouse.AccountCohortHolding, flows []clickhouse.AccountCohortFlow) map[string]struct{} {
	ranked := make([]clickhouse.AccountCohortHolding, 0, len(holdings))
	for _, hd := range holdings {
		if cohortPriceableKind(hd.Asset) {
			ranked = append(ranked, hd)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		bi, bj := ranked[i].Balance, ranked[j].Balance
		if bi == nil {
			bi = new(big.Int)
		}
		if bj == nil {
			bj = new(big.Int)
		}
		return bi.Cmp(bj) > 0
	})
	if len(ranked) > cohortPricedHoldingsCap {
		ranked = ranked[:cohortPricedHoldingsCap]
	}
	out := make(map[string]struct{}, len(ranked)+len(flows))
	for _, hd := range ranked {
		out[hd.Asset] = struct{}{}
	}
	for _, f := range flows {
		if cohortPriceableKind(f.Asset) {
			out[f.Asset] = struct{}{}
		}
	}
	return out
}

func cohortPriceableKind(asset string) bool {
	kind := cohortAssetKind(asset)
	return kind == "native" || kind == "classic"
}

// cohortHoldingsView renders holdings in whole units, classified, and
// valued only where the live price exists; the valuation sums exactly
// what it priced and counts the holdings the price cap kept unpriced.
func cohortHoldingsView(holdings []clickhouse.AccountCohortHolding, price cohortPriceFn, eligible map[string]struct{}) ([]AccountCohortHoldingV, AccountCohortValuationV) {
	out := make([]AccountCohortHoldingV, 0, len(holdings))
	val := AccountCohortValuationV{Basis: accountCohortValuationBasis, PriceCap: cohortPricedHoldingsCap}
	total := new(big.Rat)
	for _, hd := range holdings {
		v := AccountCohortHoldingV{Asset: hd.Asset, Kind: cohortAssetKind(hd.Asset), Holders: hd.Holders, Balance: stroops7(hd.Balance)}
		if p, ok := price(hd.Asset); ok {
			ps := p.Text
			v.PriceUSD = &ps
			usd := usdOfStroops(hd.Balance, p.Rat)
			s := formatUSD(usd)
			v.ValueUSD = &s
			total.Add(total, usd)
			val.PricedHoldings++
		} else {
			val.UnpricedHoldings++
			if _, in := eligible[hd.Asset]; !in && cohortPriceableKind(hd.Asset) {
				val.UnpricedOverCap++
			}
		}
		out = append(out, v)
	}
	if val.PricedHoldings > 0 {
		s := formatUSD(total)
		val.TotalUSD = &s
	}
	return out, val
}

// cohortFlowsView groups the reader's rows (ascending by month, asset; the all-assets row keyed
// CohortAllAssets) into one point per month. Assets is re-ordered by total movements across all
// months, ties by asset id, matching the reader's own ranking rather than per-month alphabetic order.
func cohortFlowsView(flows []clickhouse.AccountCohortFlow, price cohortPriceFn) AccountCohortFlowsV {
	out := AccountCohortFlowsV{Granularity: "1M", Assets: []string{}, Points: []AccountCohortFlowPointV{}}
	shown := map[string]struct{}{}
	movements := map[string]uint64{}
	var cur *AccountCohortFlowPointV
	var sum *cohortMonthSum
	flush := func() {
		if cur != nil {
			sum.apply(cur)
		}
	}
	for _, f := range flows {
		period := f.Month.UTC().Format("2006-01")
		if cur == nil || cur.Period != period {
			flush()
			out.Points = append(out.Points, AccountCohortFlowPointV{
				Period: period, PeriodStart: f.Month.UTC().Format(time.RFC3339), ByAsset: []AccountCohortAssetFlowV{},
			})
			cur = &out.Points[len(out.Points)-1]
			sum = newCohortMonthSum()
		}
		if f.Asset == clickhouse.CohortAllAssets {
			cur.Movements, cur.ActiveAccounts = f.Movements, f.ActiveAccounts
			continue
		}
		if _, seen := shown[f.Asset]; !seen {
			shown[f.Asset] = struct{}{}
			out.Assets = append(out.Assets, f.Asset)
		}
		movements[f.Asset] += f.Movements
		af, usd := cohortAssetFlowView(f, price)
		sum.add(usd)
		cur.ByAsset = append(cur.ByAsset, af)
	}
	flush()
	sort.SliceStable(out.Assets, func(i, j int) bool {
		a, b := out.Assets[i], out.Assets[j]
		if movements[a] != movements[b] {
			return movements[a] > movements[b]
		}
		return a < b
	})
	out.AssetsTruncated = len(out.Assets) >= clickhouse.CohortFlowAssetsLimit
	return out
}

// cohortAssetUSD is one asset-month's exact USD legs; a nil pair means
// that basis does not price the asset.
type cohortAssetUSD struct {
	todayIn, todayOut, thenIn, thenOut *big.Rat
}

// cohortMonthSum accumulates one month's USD flows exactly over its
// basket — the assets priced on both bases — and renders them rounded
// once, never as a sum of rounded strings.
type cohortMonthSum struct {
	todayIn, todayOut, thenIn, thenOut *big.Rat
	n                                  uint64
}

func newCohortMonthSum() *cohortMonthSum {
	return &cohortMonthSum{todayIn: new(big.Rat), todayOut: new(big.Rat), thenIn: new(big.Rat), thenOut: new(big.Rat)}
}

func (s *cohortMonthSum) add(u cohortAssetUSD) {
	if u.todayIn == nil || u.thenIn == nil {
		return
	}
	s.todayIn.Add(s.todayIn, u.todayIn)
	s.todayOut.Add(s.todayOut, u.todayOut)
	s.thenIn.Add(s.thenIn, u.thenIn)
	s.thenOut.Add(s.thenOut, u.thenOut)
	s.n++
}

func (s *cohortMonthSum) apply(p *AccountCohortFlowPointV) {
	p.PricedAssets = s.n
	if s.n == 0 {
		return
	}
	ti, to, hi, ho := formatUSD(s.todayIn), formatUSD(s.todayOut), formatUSD(s.thenIn), formatUSD(s.thenOut)
	p.InflowUSD, p.OutflowUSD, p.InflowUSDThen, p.OutflowUSDThen = &ti, &to, &hi, &ho
}

// cohortAssetFlowView renders one asset's month: whole units for classic keys, the contract's own
// unit otherwise, USD at today's price and at the month's own price where available. The exact USD
// legs are returned beside the view so the month point can sum its basket before rounding.
func cohortAssetFlowView(f clickhouse.AccountCohortFlow, price cohortPriceFn) (AccountCohortAssetFlowV, cohortAssetUSD) {
	var usd cohortAssetUSD
	scaled := cohortAssetKind(f.Asset) != "contract"
	af := AccountCohortAssetFlowV{Asset: f.Asset, Scaled: scaled}
	if scaled {
		af.Inflow, af.Outflow = stroops7(f.Inflow), stroops7(f.Outflow)
	} else {
		af.Inflow, af.Outflow = f.Inflow.String(), f.Outflow.String()
	}
	if p, ok := price(f.Asset); ok && scaled {
		usd.todayIn, usd.todayOut = usdOfStroops(f.Inflow, p.Rat), usdOfStroops(f.Outflow, p.Rat)
		in, o := formatUSD(usd.todayIn), formatUSD(usd.todayOut)
		af.InflowUSD, af.OutflowUSD = &in, &o
	}
	pThen, ok := cohortPriceThen(f.PriceUSDThen)
	if !ok || !scaled {
		return af, usd
	}
	usd.thenIn, usd.thenOut = usdOfStroops(f.Inflow, pThen), usdOfStroops(f.Outflow, pThen)
	in, o, ps := formatUSD(usd.thenIn), formatUSD(usd.thenOut), *f.PriceUSDThen
	af.InflowUSDThen, af.OutflowUSDThen, af.PriceUSDThen = &in, &o, &ps
	return af, usd
}

// cohortPriceThen parses the reader's month price into an exact rate.
// A price that does not parse or is not positive is a miss, never a
// zero — the same rule cohortPricer applies to the live rate.
func cohortPriceThen(raw *string) (*big.Rat, bool) {
	if raw == nil {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(*raw))
	if !ok || r.Sign() <= 0 {
		return nil, false
	}
	return r, true
}

// cohortContractsView labels each contract where the roster knows it,
// and names a token contract the lake can name where it does not.
func (h *Handler) cohortContractsView(ctx context.Context, contracts []clickhouse.AccountCohortContract) []AccountCohortContractV {
	out := make([]AccountCohortContractV, 0, len(contracts))
	for _, ct := range contracts {
		v := AccountCohortContractV{
			ContractID: ct.ContractID, Movements: ct.Movements, ActiveAccounts: ct.ActiveAccounts,
			FirstAt: ct.FirstAt.UTC().Format(time.RFC3339), LastAt: ct.LastAt.UTC().Format(time.RFC3339),
		}
		if h.ContractProtocol != nil {
			if name, ok := h.ContractProtocol(ctx, ct.ContractID); ok {
				v.Protocol = name
			}
		}
		if v.Protocol == "" && h.Reader != nil {
			if resolved := h.resolveSEP41MovementAsset(ctx, ct.ContractID); resolved != ct.ContractID {
				v.Label = "token " + assetDisplayLabel(resolved)
			}
		}
		out = append(out, v)
	}
	return out
}

// cohortPositionsView resolves a position's asset to a display label
// where it is a token contract the lake can name.
func (h *Handler) cohortPositionsView(ctx context.Context, positions []clickhouse.AccountCohortPosition) []AccountCohortPositionV {
	out := make([]AccountCohortPositionV, 0, len(positions))
	var resolve positionAssetResolver
	for _, p := range positions {
		v := AccountCohortPositionV{
			Protocol: p.Protocol, PositionKind: p.PositionKind, Venue: p.Venue, Asset: p.Asset,
			Holders: p.Holders, Amount: p.Amount.String(),
		}
		if p.Asset != "" {
			if resolve == nil {
				resolve = h.newPositionAssetResolver(ctx)
			}
			v.AssetLabel = assetDisplayLabel(resolve(p.Asset))
		}
		out = append(out, v)
	}
	return out
}

// cohortPricer prices a canonical asset id at the live USD rate, once per asset per request, and
// only for assets in eligible (see cohortPriceable); everything else is unpriced without a read.
// The price is parsed once into an exact big.Rat; an unparseable or non-positive price is a miss,
// never a zero.
//
// The returned *bool is true once the walk abandoned an in-cap lookup because ctx expired.
// LookupUSDPrice cannot tell a cancelled read from a genuine no-price (both are (_, false)), so
// without it the counts would read as complete when the budget cut the walk short. Read it only
// after every price call has been made; it is not safe from another goroutine mid-walk.
func (h *Handler) cohortPricer(ctx context.Context, eligible map[string]struct{}) (cohortPriceFn, *bool) {
	cache := map[string]cohortPrice{}
	miss := map[string]struct{}{}
	degraded := false
	fn := func(asset string) (cohortPrice, bool) {
		if !h.PricingEnabled || h.LookupUSDPrice == nil {
			return cohortPrice{}, false
		}
		if p, ok := cache[asset]; ok {
			return p, true
		}
		if _, ok := miss[asset]; ok {
			return cohortPrice{}, false
		}
		if _, ok := eligible[asset]; !ok {
			miss[asset] = struct{}{}
			return cohortPrice{}, false
		}
		p, ok := h.lookupCohortPrice(ctx, asset, &degraded)
		if !ok {
			miss[asset] = struct{}{}
			return cohortPrice{}, false
		}
		cache[asset] = p
		return p, true
	}
	return fn, &degraded
}

// lookupCohortPrice does the ctx-aware USD lookup and parse for asset, split out of cohortPricer's
// closure to keep its early returns flat. It sets *degraded when the miss was caused by ctx
// expiry rather than a genuine no-price, per cohortPricer.
func (h *Handler) lookupCohortPrice(ctx context.Context, asset string, degraded *bool) (cohortPrice, bool) {
	if ctx.Err() != nil {
		*degraded = true
		return cohortPrice{}, false
	}
	parsed, err := canonical.ParseAsset(asset)
	if err != nil {
		return cohortPrice{}, false
	}
	raw, ok := h.LookupUSDPrice(ctx, parsed)
	if !ok {
		if ctx.Err() != nil {
			*degraded = true
		}
		return cohortPrice{}, false
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(raw))
	if !ok || r.Sign() <= 0 {
		return cohortPrice{}, false
	}
	return cohortPrice{Text: raw, Rat: r}, true
}

// cohortAssetKind classifies a canonical asset id the way the lake spells
// it: 'native', 'CODE-ISSUER' (classic), 'pool:<hex>' (pool_share), or a
// C… contract id (contract).
func cohortAssetKind(asset string) string {
	switch {
	case asset == "native":
		return "native"
	case strings.HasPrefix(asset, "pool:"):
		return "pool_share"
	case strings.HasPrefix(asset, "C") && !strings.Contains(asset, "-"):
		return "contract"
	default:
		return "classic"
	}
}

// stroops7 renders a stroop count as a whole-unit decimal string with
// seven places, sign preserved, no float in the path.
func stroops7(v *big.Int) string {
	if v == nil {
		return "0"
	}
	neg := v.Sign() < 0
	abs := new(big.Int).Abs(v)
	q, r := new(big.Int).QuoRem(abs, big.NewInt(10_000_000), new(big.Int))
	s := q.String() + "." + strings.Repeat("0", 7-len(r.String())) + r.String()
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" {
		s = "0"
	}
	if neg && s != "0" {
		s = "-" + s
	}
	return s
}

// usdOfStroops values a stroop count at an exact USD rate:
// stroops / 10^7 × price, as a big.Rat (ADR-0003 — exact arithmetic for
// every served dollar; the same path listingValueUSD and the market-cap
// figures take). A nil count or price is zero dollars.
func usdOfStroops(v *big.Int, price *big.Rat) *big.Rat {
	if v == nil || price == nil {
		return new(big.Rat)
	}
	out := new(big.Rat).SetFrac(v, big.NewInt(10_000_000))
	return out.Mul(out, price)
}

// formatUSD renders an exact dollar amount to two places, rounded
// half away from zero (big.Rat.FloatString's rule — the same rendering
// every other served value_usd uses).
func formatUSD(r *big.Rat) string {
	if r == nil {
		return "0.00"
	}
	return r.FloatString(2)
}
