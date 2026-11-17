package v1

import (
	"cmp"
	"context"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// lendingRatesMaxRows caps the rate table; the indexed markets are tens of pools of a few reserves each.
const lendingRatesMaxRows = 2000

// LendingRatesView is the GET /v1/lending/rates body: every indexed lending reserve's current rates, one row per (pool, reserve).
type LendingRatesView struct {
	Rates []LendingRate `json:"rates"`
	// OmittedPools are listed pools whose reserves could not be read; their reserves are absent from Rates.
	OmittedPools []LendingOmittedPool `json:"omitted_pools"`
	// Partial is true when a listed pool, or the BLNT pool list, is missing from Rates.
	Partial bool `json:"partial"`
	// Truncated is true when Rates was cut at lendingRatesMaxRows.
	Truncated bool `json:"truncated"`
	// ConfigDecimals is the fixed-point scale of collateral_factor and liability_factor.
	ConfigDecimals int `json:"config_decimals"`
}

// LendingOmittedPool names a pool left out of a rates response and why.
type LendingOmittedPool struct {
	Protocol string `json:"protocol"`
	Pool     string `json:"pool"`
	Reason   string `json:"reason"`
}

// LendingRate is one reserve of one pool, copied from that pool's cached GET /v1/lending/pools/{pool}/reserves view.
type LendingRate struct {
	Protocol       string  `json:"protocol"`
	PoolVersion    *string `json:"pool_version"`
	Pool           string  `json:"pool"`
	PoolStatus     *string `json:"pool_status"`
	PoolStatusCode *uint32 `json:"pool_status_code"`
	Asset          string  `json:"asset"`
	// AssetID is the classic or native asset a SAC reserve wraps; null for a token that is not a known SAC.
	AssetID     *string `json:"asset_id"`
	AssetCode   *string `json:"asset_code"`
	AssetIssuer *string `json:"asset_issuer"`
	//floatmoney:ok yield rates and a utilisation percentage, not amounts
	SupplyAPR        *float64  `json:"supply_apr"`
	BorrowAPR        *float64  `json:"borrow_apr"`
	UtilizationPct   float64   `json:"utilization_pct"`
	Decimals         *uint32   `json:"decimals"`
	Supplied         string    `json:"supplied"`
	Borrowed         string    `json:"borrowed"`
	SuppliedUSD      *string   `json:"supplied_usd"`
	BorrowedUSD      *string   `json:"borrowed_usd"`
	PriceUSD         *string   `json:"price_usd"`
	PriceStale       bool      `json:"price_stale"`
	CollateralFactor *string   `json:"collateral_factor"`
	LiabilityFactor  *string   `json:"liability_factor"`
	SupplyCap        *string   `json:"supply_cap"`
	Enabled          *bool     `json:"enabled"`
	LastUpdate       *WireTime `json:"last_update"`
	// Obtainable is false when any Exclusions apply: the APR is not a yield a new position can earn or pay.
	Obtainable bool     `json:"obtainable"`
	Exclusions []string `json:"exclusions"`
}

// Exclusion reasons on a LendingRate.
const (
	rateExclPoolNotActive     = "pool_not_active"
	rateExclPoolStatusUnknown = "pool_status_unknown"
	rateExclReserveDisabled   = "reserve_disabled"
	rateExclConfigUnknown     = "reserve_config_unknown"
	rateExclRateUnavailable   = "rate_unavailable"
)

type lendingRatesSort string

const (
	sortSupplyAPR   lendingRatesSort = "supply_apr"
	sortBorrowAPR   lendingRatesSort = "borrow_apr"
	sortSuppliedUSD lendingRatesSort = "supplied_usd"
)

type lendingRatesQuery struct {
	protocol   string
	assetForms map[string]struct{} // every alias spelling of the asset filter; nil when unfiltered
	sort       lendingRatesSort
}

// ratePool is one pool from the lending pool list.
type ratePool struct{ protocol, pool string }

// ratePoolRead is one pool's cached reserves view, or why it is omitted.
type ratePoolRead struct {
	ratePool
	res        poolReserves
	at         time.Time
	cacheStale bool
	omitted    string
}

// handleLendingRates serves GET /v1/lending/rates: the reserves of every listed pool from the same per-pool cache as
// /v1/lending/pools/{pool}/reserves, so the two never disagree. A pool whose read fails is named in omitted_pools and the
// response is degraded.
func (s *Server) handleLendingRates(w http.ResponseWriter, r *http.Request) {
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	q, ok := parseLendingRatesQuery(w, r)
	if !ok {
		return
	}
	view := LendingRatesView{Rates: []LendingRate{}, OmittedPools: []LendingOmittedPool{}, ConfigDecimals: lendingConfigDecimals}
	if s.Lending == nil {
		writeJSON(w, view, Flags{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	var vintage dataVintage
	pools, flags, err := s.lendingRatePools(ctx, q.protocol, &vintage)
	if err != nil {
		s.writeListBlendPoolsError(ctx, w, r, err)
		return
	}
	view.Partial = flags.Degraded
	reads := make([]ratePoolRead, len(pools))
	forEachBounded(s.logger, len(pools), readFanoutConcurrency, func(i int) {
		reads[i] = s.readRatePool(ctx, pools[i]) // distinct indices — no mutex needed
	})
	for _, pr := range reads {
		if pr.omitted != "" {
			view.OmittedPools = append(view.OmittedPools, LendingOmittedPool{Protocol: pr.protocol, Pool: pr.pool, Reason: pr.omitted})
			flags.Degraded, view.Partial = true, true
			continue
		}
		vintage.note(pr.at)
		flags.Stale = flags.Stale || pr.res.flags.Stale || pr.cacheStale
		flags.Degraded = flags.Degraded || pr.res.flags.Degraded
		view.Rates = append(view.Rates, s.lendingRateRows(pr, q)...)
	}
	sortLendingRates(view.Rates, q.sort)
	if len(view.Rates) > lendingRatesMaxRows {
		view.Rates, view.Truncated = view.Rates[:lendingRatesMaxRows], true
	}
	writeEnvelope(w, Envelope{Data: view, AsOf: vintage.asOf(), Flags: flags})
}

func parseLendingRatesQuery(w http.ResponseWriter, r *http.Request) (lendingRatesQuery, bool) {
	v := r.URL.Query()
	q := lendingRatesQuery{protocol: v.Get("protocol"), sort: lendingRatesSort(cmp.Or(v.Get("sort"), string(sortSupplyAPR)))}
	switch q.protocol {
	case "", "blend", blnt.SourceName:
	default:
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid parameter", http.StatusBadRequest, "protocol must be blend or blnt")
		return q, false
	}
	switch q.sort {
	case sortSupplyAPR, sortBorrowAPR, sortSuppliedUSD:
	default:
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid parameter", http.StatusBadRequest, "sort must be supply_apr, borrow_apr or supplied_usd")
		return q, false
	}
	if raw := v.Get("asset"); raw != "" {
		a, err := canonical.ParseAsset(raw)
		if err != nil {
			writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset",
				"Invalid asset", http.StatusBadRequest, "asset must be a canonical asset id or token contract C-strkey")
			return q, false
		}
		q.assetForms = map[string]struct{}{}
		for _, f := range canonical.AssetAliasStrings(a) {
			q.assetForms[f] = struct{}{}
		}
	}
	return q, true
}

// lendingRatePools lists the pools /v1/lending/pools lists; a failed BLNT list leaves those pools out as degraded.
func (s *Server) lendingRatePools(ctx context.Context, protocol string, vintage *dataVintage) ([]ratePool, Flags, error) {
	listCtx, cancel := context.WithTimeout(ctx, lendingPoolListBudget)
	defer cancel()
	var out []ratePool
	var flags Flags
	if protocol != blnt.SourceName {
		rows, stale, err := listBlendPools(listCtx, s.Lending, vintage)
		if err != nil {
			return nil, flags, err
		}
		flags.Stale = stale
		for _, p := range rows {
			out = append(out, ratePool{protocol: "blend", pool: p.Pool})
		}
	}
	if protocol != "blend" {
		rows, stale, failed := s.listBLNTPools(listCtx, s.Lending, vintage)
		flags.Stale, flags.Degraded = flags.Stale || stale, failed
		for _, p := range rows {
			out = append(out, ratePool{protocol: blnt.SourceName, pool: p.Pool})
		}
	}
	return out, flags, nil
}

// readRatePool reads one pool through the reserves cache, holding off a pool whose build failed within lendingSummaryRetry.
func (s *Server) readRatePool(ctx context.Context, p ratePool) ratePoolRead {
	out := ratePoolRead{ratePool: p}
	if s.lendingSummaryFailedRecently(p.pool) && !s.lendingReserves.filled(p.pool) {
		out.omitted = "reserves read failed recently; retried after " + lendingSummaryRetry.String()
		return out
	}
	var err error
	out.res, out.at, out.cacheStale, err = s.lendingReserves.get(ctx, "pool_reserves", p.pool, s.fillPoolReserves(p.pool))
	switch {
	case err == nil:
	case errors.Is(err, errPoolLineageUnknown):
		out.omitted = "pool lineage unknown"
	case handlerTimedOut(ctx, err):
		out.omitted = "reserves read timed out"
	default:
		s.logger.Warn("lending rates: pool reserves read failed", "err", err, "pool", p.pool)
		out.omitted = "reserves read failed"
	}
	return out
}

// lendingRateRows maps one pool's reserves to rate rows, keeping those that match the asset filter.
func (s *Server) lendingRateRows(pr ratePoolRead, q lendingRatesQuery) []LendingRate {
	v := pr.res.view
	base := LendingRate{Protocol: pr.protocol, PoolVersion: ratePoolVersion(pr.protocol, pr.res.version), Pool: v.Pool}
	if v.PoolConfig != nil {
		base.PoolStatus, base.PoolStatusCode = v.PoolConfig.StatusLabel, v.PoolConfig.Status
	}
	out := make([]LendingRate, 0, len(v.Reserves))
	for _, rv := range v.Reserves {
		row := base
		s.fillRateAsset(&row, rv.Asset)
		if !q.matchesAsset(row) {
			continue
		}
		fillRateReserve(&row, rv)
		row.Exclusions = rateExclusions(row, rv.Config)
		row.Obtainable = len(row.Exclusions) == 0
		out = append(out, row)
	}
	return out
}

func ratePoolVersion(protocol string, v blend.PoolVersion) *string {
	var s string
	switch {
	case protocol == blnt.SourceName:
		s = "v3"
	case v == blend.PoolV1:
		s = "v1"
	case v == blend.PoolV2:
		s = "v2"
	default:
		return nil
	}
	return &s
}

// fillRateAsset resolves a SAC reserve to the classic or native asset it wraps, the identity pricing uses.
func (s *Server) fillRateAsset(row *LendingRate, assetC string) {
	row.Asset = assetC
	id, ok := s.resolveSACAsset(assetC)
	if !ok {
		return
	}
	a, err := canonical.ParseAsset(id)
	if err != nil {
		return
	}
	canon := a.String()
	row.AssetID = &canon
	if a.Type == canonical.AssetClassic {
		row.AssetCode, row.AssetIssuer = &a.Code, &a.Issuer
	}
}

func fillRateReserve(row *LendingRate, rv ReserveView) {
	row.SupplyAPR, row.BorrowAPR, row.UtilizationPct = rv.SupplyAPR, rv.BorrowAPR, rv.UtilizationPct
	row.Decimals, row.Supplied, row.Borrowed = rv.Decimals, rv.Supplied, rv.Borrowed
	row.SuppliedUSD, row.BorrowedUSD, row.PriceUSD, row.PriceStale = rv.SuppliedUSD, rv.BorrowedUSD, rv.PriceUSD, rv.PriceStale
	row.LastUpdate = rv.LastUpdate
	if c := rv.Config; c != nil {
		cf, lf, enabled := c.CollateralFactor, c.LiabilityFactor, c.Enabled
		row.CollateralFactor, row.LiabilityFactor, row.SupplyCap, row.Enabled = &cf, &lf, c.SupplyCap, &enabled
	}
}

// rateExclusions lists why a row's APR is not obtainable; a V1 pool's status codes are unverified, so its status is unknown.
func rateExclusions(row LendingRate, cfg *LendingReserveConfigView) []string {
	out := []string{}
	switch {
	case row.PoolStatus == nil:
		out = append(out, rateExclPoolStatusUnknown)
	case *row.PoolStatus != "active" && *row.PoolStatus != "admin_active":
		out = append(out, rateExclPoolNotActive)
	}
	switch {
	case cfg == nil:
		out = append(out, rateExclConfigUnknown)
	case !cfg.Enabled:
		out = append(out, rateExclReserveDisabled)
	}
	if row.SupplyAPR == nil || row.BorrowAPR == nil {
		out = append(out, rateExclRateUnavailable)
	}
	return out
}

func (q lendingRatesQuery) matchesAsset(row LendingRate) bool {
	if q.assetForms == nil {
		return true
	}
	if _, ok := q.assetForms[row.Asset]; ok {
		return true
	}
	if row.AssetID == nil {
		return false
	}
	_, ok := q.assetForms[*row.AssetID]
	return ok
}

// sortLendingRates orders rows by the requested key, rows without it last, then by pool and asset for a stable order.
func sortLendingRates(rows []LendingRate, by lendingRatesSort) {
	slices.SortStableFunc(rows, func(a, b LendingRate) int {
		return cmp.Or(compareRateKey(a, b, by), cmp.Compare(a.Pool, b.Pool), cmp.Compare(a.Asset, b.Asset))
	})
}

func compareRateKey(a, b LendingRate, by lendingRatesSort) int {
	switch by {
	case sortSupplyAPR:
		return nilsLast(a.SupplyAPR, b.SupplyAPR, func(x, y *float64) int { return cmp.Compare(*y, *x) })
	case sortBorrowAPR:
		return nilsLast(a.BorrowAPR, b.BorrowAPR, func(x, y *float64) int { return cmp.Compare(*x, *y) })
	case sortSuppliedUSD:
		x, _ := parseUSD(a.SuppliedUSD) // nil when absent or unparseable
		y, _ := parseUSD(b.SuppliedUSD)
		return nilsLast(x, y, func(x, y *big.Rat) int { return y.Cmp(x) })
	}
	return 0
}

// nilsLast compares two optional values with cmpFn, ordering a missing value after any present one.
func nilsLast[T any](a, b *T, cmpFn func(x, y *T) int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return cmpFn(a, b)
}
