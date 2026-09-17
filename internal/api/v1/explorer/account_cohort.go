package explorer

import (
	"context"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// AccountCohortView is GET /v1/accounts/{g}/graph/cohort: what the
// accounts this address created or sponsored went on to hold and do.
// Every figure is the cohort rollup's last cycle (Cycle dates it), read
// from the cohort's own ledger footprint — never from the root's.
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

// AccountCohortSizeV counts the cohort and how much of it is alive and
// recently active.
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

// AccountCohortValuationV totals the priced holdings at the live price.
type AccountCohortValuationV struct {
	TotalUSD         *string `json:"total_usd,omitempty"`
	PricedHoldings   int     `json:"priced_holdings"`
	UnpricedHoldings int     `json:"unpriced_holdings"`
	Basis            string  `json:"basis"`
}

// AccountCohortFlowsV is the cohort's value moved in and out, by month.
type AccountCohortFlowsV struct {
	Granularity string `json:"granularity"`
	// Assets are the assets broken out per month, the cohort's most
	// moved first; a month's Movements and ActiveAccounts count EVERY
	// asset, not only these.
	Assets []string                  `json:"assets"`
	Points []AccountCohortFlowPointV `json:"points"`
}

// AccountCohortFlowPointV is one month.
type AccountCohortFlowPointV struct {
	Period         string                    `json:"period"`
	PeriodStart    string                    `json:"period_start"`
	Movements      uint64                    `json:"movements"`
	ActiveAccounts uint64                    `json:"active_accounts"`
	ByAsset        []AccountCohortAssetFlowV `json:"by_asset"`
}

// AccountCohortAssetFlowV is one asset's month. Inflow and Outflow are
// decimal strings in whole units when Scaled (classic assets, 7 places)
// and the contract's own smallest unit otherwise. The USD figures are AT
// TODAY'S PRICE — the month's quantity valued now, not what it was worth
// then — and absent where nothing prices the asset.
type AccountCohortAssetFlowV struct {
	Asset      string  `json:"asset"`
	Inflow     string  `json:"inflow"`
	Outflow    string  `json:"outflow"`
	Scaled     bool    `json:"scaled"`
	InflowUSD  *string `json:"inflow_usd,omitempty"`
	OutflowUSD *string `json:"outflow_usd,omitempty"`
}

// AccountCohortContractV is one C… contract the cohort moved value
// through; Protocol is set when the roster knows it.
type AccountCohortContractV struct {
	ContractID     string `json:"contract_id"`
	Protocol       string `json:"protocol,omitempty"`
	Movements      uint64 `json:"movements"`
	ActiveAccounts uint64 `json:"active_accounts"`
	FirstAt        string `json:"first_at"`
	LastAt         string `json:"last_at"`
}

// AccountCohortPositionV is the cohort's aggregate in one DeFi venue.
// Amount is the fold's own unit summed across holders — a magnitude.
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
	"never the root's. holdings and valuation are current balances at the live price; a pool " +
	"share is a classic liquidity-pool position and is never priced. flows are derived from the " +
	"movements archive — received minus sent per asset per calendar month (UTC) — so a month " +
	"with no movement emits no point, and the USD figures value each month's quantity at " +
	"today's price, not that month's. active_accounts is a uniqCombined estimate, movements is " +
	"exact. contracts are the C… counterparties of cohort movements: the value-moving subset " +
	"of interaction — a call that moved no balance is not counted. positions are the served " +
	"tier's per-protocol folds joined to the cohort; amount is the fold's own unit summed " +
	"across holders. covered:false means the rollup does not carry this root (a creator under " +
	"the floor, or no edges in this relation), not that the cohort holds nothing."

const accountCohortValuationBasis = "live_vwap_current"

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
	h.WriteJSON(w, h.accountCohortView(ctx, cohort), false)
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
		Valuation: AccountCohortValuationV{Basis: accountCohortValuationBasis},
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
	price := h.cohortPricer(ctx)
	out.Holdings, out.Valuation = cohortHoldingsView(c.Holdings, price)
	out.HoldingsTruncated = len(c.Holdings) >= clickhouse.CohortHoldingsLimit
	out.Flows = cohortFlowsView(c.Flows, price)
	out.Contracts = h.cohortContractsView(ctx, c.Contracts)
	out.Positions = h.cohortPositionsView(ctx, c.Positions)
	return out
}

// cohortHoldingsView renders holdings in whole units, classified, and
// valued only where the live price exists; the valuation sums exactly
// what it priced.
func cohortHoldingsView(holdings []clickhouse.AccountCohortHolding, price func(string) (float64, bool)) ([]AccountCohortHoldingV, AccountCohortValuationV) {
	out := make([]AccountCohortHoldingV, 0, len(holdings))
	val := AccountCohortValuationV{Basis: accountCohortValuationBasis}
	total := new(big.Float)
	for _, hd := range holdings {
		v := AccountCohortHoldingV{Asset: hd.Asset, Kind: cohortAssetKind(hd.Asset), Holders: hd.Holders, Balance: stroops7(hd.Balance)}
		if p, ok := price(hd.Asset); ok {
			ps := strconv.FormatFloat(p, 'f', -1, 64)
			v.PriceUSD = &ps
			usd := usdOfStroops(hd.Balance, p)
			s := formatUSD(usd)
			v.ValueUSD = &s
			total.Add(total, usd)
			val.PricedHoldings++
		} else {
			val.UnpricedHoldings++
		}
		out = append(out, v)
	}
	if val.PricedHoldings > 0 {
		s := formatUSD(total)
		val.TotalUSD = &s
	}
	return out, val
}

// cohortFlowsView groups the reader's rows — ascending by (month, asset),
// the all-assets row keyed CohortAllAssets — into one point per month.
func cohortFlowsView(flows []clickhouse.AccountCohortFlow, price func(string) (float64, bool)) AccountCohortFlowsV {
	out := AccountCohortFlowsV{Granularity: "1M", Assets: []string{}, Points: []AccountCohortFlowPointV{}}
	shown := map[string]struct{}{}
	var cur *AccountCohortFlowPointV
	for _, f := range flows {
		period := f.Month.UTC().Format("2006-01")
		if cur == nil || cur.Period != period {
			out.Points = append(out.Points, AccountCohortFlowPointV{
				Period: period, PeriodStart: f.Month.UTC().Format(time.RFC3339), ByAsset: []AccountCohortAssetFlowV{},
			})
			cur = &out.Points[len(out.Points)-1]
		}
		if f.Asset == clickhouse.CohortAllAssets {
			cur.Movements, cur.ActiveAccounts = f.Movements, f.ActiveAccounts
			continue
		}
		if _, seen := shown[f.Asset]; !seen {
			shown[f.Asset] = struct{}{}
			out.Assets = append(out.Assets, f.Asset)
		}
		cur.ByAsset = append(cur.ByAsset, cohortAssetFlowView(f, price))
	}
	return out
}

// cohortAssetFlowView renders one asset's month: whole units for classic
// keys, the contract's own unit otherwise, USD at today's price where
// the asset is priced.
func cohortAssetFlowView(f clickhouse.AccountCohortFlow, price func(string) (float64, bool)) AccountCohortAssetFlowV {
	scaled := cohortAssetKind(f.Asset) != "contract"
	af := AccountCohortAssetFlowV{Asset: f.Asset, Scaled: scaled}
	if scaled {
		af.Inflow, af.Outflow = stroops7(f.Inflow), stroops7(f.Outflow)
	} else {
		af.Inflow, af.Outflow = f.Inflow.String(), f.Outflow.String()
	}
	if p, ok := price(f.Asset); ok && scaled {
		in, o := formatUSD(usdOfStroops(f.Inflow, p)), formatUSD(usdOfStroops(f.Outflow, p))
		af.InflowUSD, af.OutflowUSD = &in, &o
	}
	return af
}

// cohortContractsView labels each contract where the roster knows it.
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
			Holders: p.Holders, Amount: strconv.FormatFloat(p.Amount, 'f', -1, 64),
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

// cohortPricer prices a canonical asset id at the live USD rate, once per
// asset per request. Pool shares and C… ids are never priced here.
func (h *Handler) cohortPricer(ctx context.Context) func(asset string) (float64, bool) {
	cache := map[string]float64{}
	miss := map[string]struct{}{}
	return func(asset string) (float64, bool) {
		if !h.PricingEnabled || h.LookupUSDPrice == nil {
			return 0, false
		}
		if p, ok := cache[asset]; ok {
			return p, true
		}
		if _, ok := miss[asset]; ok {
			return 0, false
		}
		kind := cohortAssetKind(asset)
		if kind != "native" && kind != "classic" {
			miss[asset] = struct{}{}
			return 0, false
		}
		parsed, err := canonical.ParseAsset(asset)
		if err != nil {
			miss[asset] = struct{}{}
			return 0, false
		}
		raw, ok := h.LookupUSDPrice(ctx, parsed)
		if !ok {
			miss[asset] = struct{}{}
			return 0, false
		}
		p, err := strconv.ParseFloat(raw, 64)
		if err != nil || p <= 0 {
			miss[asset] = struct{}{}
			return 0, false
		}
		cache[asset] = p
		return p, true
	}
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

func usdOfStroops(v *big.Int, price float64) *big.Float {
	if v == nil {
		return new(big.Float)
	}
	f := new(big.Float).SetInt(v)
	f.Quo(f, big.NewFloat(1e7))
	return f.Mul(f, big.NewFloat(price))
}

func formatUSD(f *big.Float) string {
	return f.Text('f', 2)
}
