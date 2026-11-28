package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingHistoryReader is the lake read behind the lending history routes.
type lendingHistoryReader interface {
	LendingReserveHistory(ctx context.Context, pool, asset string, q clickhouse.LendingHistoryQuery) (clickhouse.LendingReserveHistory, error)
	LendingBackstopHistory(ctx context.Context, pool string, tiered bool, q clickhouse.LendingHistoryQuery) (clickhouse.LendingBackstopHistory, error)
}

var _ lendingHistoryReader = (*clickhouse.ExplorerReader)(nil)

// registerLendingHistoryRoutes mounts the lending history routes.
func (s *Server) registerLendingHistoryRoutes() {
	s.mux.HandleFunc("GET /v1/lending/pools/{pool}/history", s.handleLendingPoolHistory)
	s.mux.HandleFunc("GET /v1/lending/pools/{pool}/reserves/{asset}/history", s.handleLendingReserveHistory)
	s.mux.HandleFunc("GET /v1/lending/pools/{pool}/backstop/history", s.handleLendingBackstopHistory)
	s.mux.HandleFunc("GET /v1/lending/pools/{pool}/revenue", s.handleLendingPoolRevenue)
	s.mux.HandleFunc("GET /v1/lending/pools/{pool}/bad-debt", s.handleLendingPoolBadDebt)
	s.mux.HandleFunc("GET /v1/lending/pools/{pool}/emissions/history", s.handleLendingPoolEmissionsHistory)
	s.mux.HandleFunc("GET /v1/lending/pools/{pool}/health", s.handleLendingPoolHealth)
	s.mux.HandleFunc("GET /v1/lending/yields/history", s.handleLendingYieldsHistory)
	s.mux.HandleFunc("GET /v1/lending/yields/summary", s.handleLendingYieldsSummary)
}

const (
	// lendingHistoryMaxPoints caps one series; a request past it is a 400, never a truncated series.
	lendingHistoryMaxPoints = 2000
	// The widest windows leave room within the cap for the partial bucket at each end: whole days of hours (83), and days.
	lendingHistoryMaxHourlyWindow    = lendingHistoryMaxPoints / 24 * 24 * time.Hour
	lendingHistoryMaxDailyWindow     = (lendingHistoryMaxPoints - 2) * 24 * time.Hour
	lendingHistoryDefaultDailyWindow = 90 * 24 * time.Hour
	// lendingHistoryLagLedgers is how far the history may trail the lake tip before it is flagged stale.
	lendingHistoryLagLedgers = 1000
)

// LendingReserveHistoryView is the GET /v1/lending/pools/{pool}/reserves/{asset}/history body.
type LendingReserveHistoryView struct {
	Pool     string   `json:"pool"`
	Asset    string   `json:"asset"`
	Protocol *string  `json:"protocol"` // blend_v1 | blend_v2 | blnt; null when no state is recorded
	Interval string   `json:"interval"`
	From     WireTime `json:"from"`
	To       WireTime `json:"to"`
	// Decimals is the reserve token's exponent, as the live reserves endpoint resolves it; null when not established.
	Decimals *uint32 `json:"decimals"`
	// RateDecimals is b_rate's and d_rate's scale: 9 on a Blend V1 pool, else 12.
	RateDecimals   *int `json:"rate_decimals"`
	IRModDecimals  *int `json:"ir_mod_decimals"`
	ConfigDecimals int  `json:"config_decimals"`
	lendingHistoryCoverageView
	BeforeFirstState bool `json:"before_first_state"`
	// PriceMaxAgeHours bounds how far before its bucket a point's USD price may be; USDUnvaluedPoints counts the points left null.
	PriceMaxAgeHours  int                          `json:"price_max_age_hours"`
	USDUnvaluedPoints int                          `json:"usd_unvalued_points"`
	Points            []LendingReserveHistoryPoint `json:"points"`
}

// lendingHistoryCoverageView is how far the history is complete and how many points failed to decode.
type lendingHistoryCoverageView struct {
	HistoryThroughLedger uint32    `json:"history_through_ledger"`
	HistoryThroughTime   *WireTime `json:"history_through_time"`
	DroppedPoints        int       `json:"dropped_points"`
}

// LendingReserveHistoryPoint is a reserve's state at the close of a bucket in which it changed; amounts are base-unit
// decimal strings, null only on a removal.
type LendingReserveHistoryPoint struct {
	T              WireTime `json:"t"`
	StateLedger    uint32   `json:"state_ledger"`
	ConfigLedger   *uint32  `json:"config_ledger"`
	Removed        bool     `json:"removed"`
	Supplied       *string  `json:"supplied"`
	Borrowed       *string  `json:"borrowed"`
	BRate          *string  `json:"b_rate"`
	DRate          *string  `json:"d_rate"`
	IRMod          *string  `json:"ir_mod"`
	BackstopCredit *string  `json:"backstop_credit"`
	UtilizationPct *float64 `json:"utilization_pct"`
	BorrowAPR      *float64 `json:"borrow_apr"`
	//floatmoney:ok a yield rate/fraction, not a principal amount
	SupplyAPR        *float64 `json:"supply_apr"`
	CollateralFactor *string  `json:"collateral_factor"`
	LiabilityFactor  *string  `json:"liability_factor"`
	SupplyCap        *string  `json:"supply_cap"`
	// ConfigUncertain: the reserve config or pool instance changed later in this state's hour; what depends on it is null.
	ConfigUncertain bool `json:"config_uncertain"`
	LendingHistoryUSD
}

// LendingBackstopHistoryView is the GET /v1/lending/pools/{pool}/backstop/history body.
type LendingBackstopHistoryView struct {
	Pool              string   `json:"pool"`
	Protocol          *string  `json:"protocol"`
	Available         bool     `json:"available"`
	UnavailableReason *string  `json:"unavailable_reason,omitempty"`
	Backstop          *string  `json:"backstop"`
	Interval          string   `json:"interval"`
	From              WireTime `json:"from"`
	To                WireTime `json:"to"`
	lendingHistoryCoverageView
	Tiers []LendingBackstopHistoryTier `json:"tiers"`
}

// LendingBackstopHistoryTier is one backstop tier's series; Tier is null for Blend's single tier.
type LendingBackstopHistoryTier struct {
	Tier             *string                       `json:"tier"`
	BeforeFirstState bool                          `json:"before_first_state"`
	Points           []LendingBackstopHistoryPoint `json:"points"`
}

// LendingBackstopHistoryPoint is a tier's PoolBalance at the close of a bucket in which it changed.
type LendingBackstopHistoryPoint struct {
	T            WireTime `json:"t"`
	Ledger       uint32   `json:"ledger"`
	Removed      bool     `json:"removed"`
	Tokens       *string  `json:"tokens"`
	Shares       *string  `json:"shares"`
	QueuedShares *string  `json:"queued_shares"`
	QueuedPct    *float64 `json:"queued_pct"`
}

// parseLendingHistoryQuery reads interval (1h|1d, default 1d) and from/to (default the last 90 days, 83 at 1h), with from
// rounded down to its bucket.
func parseLendingHistoryQuery(w http.ResponseWriter, r *http.Request) (clickhouse.LendingHistoryQuery, string, bool) {
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "1d"
	}
	if interval != "1h" && interval != "1d" {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-interval",
			"Invalid interval", http.StatusBadRequest, "interval must be 1h or 1d")
		return clickhouse.LendingHistoryQuery{}, "", false
	}
	from, to, ok := parseFromTo(w, r)
	if !ok {
		return clickhouse.LendingHistoryQuery{}, "", false
	}
	daily := interval == "1d"
	maxWindow, defWindow := lendingHistoryMaxHourlyWindow, lendingHistoryMaxHourlyWindow
	if daily {
		maxWindow, defWindow = lendingHistoryMaxDailyWindow, lendingHistoryDefaultDailyWindow
	}
	if r.URL.Query().Get("from") == "" && r.URL.Query().Get("window") == "" {
		from = to.Add(-defWindow)
	}
	if to.Sub(from) > maxWindow {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-window",
			"Window too wide for interval="+interval, http.StatusBadRequest,
			"interval="+interval+" allows at most a "+strconv.Itoa(int(maxWindow/(24*time.Hour)))+"-day window")
		return clickhouse.LendingHistoryQuery{}, "", false
	}
	q := clickhouse.LendingHistoryQuery{From: from, To: to, Daily: daily, MaxPoints: lendingHistoryMaxPoints}
	return q.Aligned(), interval, true
}

// lendingHistoryReaderFor returns the history reader, having written the 503 when this deployment has none.
func (s *Server) lendingHistoryReaderFor(w http.ResponseWriter, r *http.Request) (lendingHistoryReader, bool) {
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return nil, false
	}
	hr, ok := s.Explorer.(lendingHistoryReader)
	if !ok {
		writeLendingHistoryNotReady(w, r)
	}
	return hr, ok
}

func writeLendingHistoryNotReady(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, "https://api.stellarindex.io/errors/history-unavailable",
		"Lending history not derived", http.StatusServiceUnavailable,
		"this deployment has not derived lending storage history yet; retry later")
}

func (s *Server) writeLendingHistoryError(ctx context.Context, w http.ResponseWriter, r *http.Request, stage, pool string, err error) {
	switch {
	case errors.Is(err, clickhouse.ErrLendingHistoryNotReady):
		writeLendingHistoryNotReady(w, r)
	case errors.Is(err, clickhouse.ErrLendingReserveNotFound):
		writeLendingHistoryNotFound(w, r, "nothing is recorded for this reserve asset in this pool")
	case errors.Is(err, clickhouse.ErrLendingHistoryTooManyPoints):
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-window",
			"Too many points", http.StatusBadRequest,
			"the series holds more than "+strconv.Itoa(lendingHistoryMaxPoints)+" points; use interval=1d or a narrower window")
	default:
		s.writeLendingLookupError(ctx, w, r, stage, pool, err)
	}
}

func writeLendingHistoryNotFound(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, "https://api.stellarindex.io/errors/not-found", "Not found", http.StatusNotFound, detail)
}

// lendingHistoryLineage is the pool's protocol and backstop unavailable reason as the live backstop route resolves them,
// having written the 404 for a pool no lending source knows, or the 503 when this deployment has no lending reader.
func (s *Server) lendingHistoryLineage(ctx context.Context, w http.ResponseWriter, r *http.Request, pool string) (protocol, reason string, ok bool) {
	if s.Lending == nil {
		writeLendingHistoryNotReady(w, r)
		return "", "", false
	}
	protocol, reason, err := s.backstopLineage(ctx, pool)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending history lineage", pool, err)
		return "", "", false
	}
	if reason == backstopReasonUnknown {
		writeLendingHistoryNotFound(w, r, "no lending pool with this id is known")
		return "", "", false
	}
	return protocol, reason, true
}

// lendingHistoryFlags is stale when the lake is, or the history trails the lake tip; degraded when a point was dropped.
func (s *Server) lendingHistoryFlags(ctx context.Context, cov lendingHistoryCoverageView) Flags {
	tip, stale, ok := s.lakeWatermark(ctx)
	return Flags{
		Stale:    stale || (ok && tip > cov.HistoryThroughLedger+lendingHistoryLagLedgers),
		Degraded: cov.DroppedPoints > 0,
	}
}

func coverageView(c clickhouse.LendingHistoryCoverage, dropped int) lendingHistoryCoverageView {
	v := lendingHistoryCoverageView{HistoryThroughLedger: c.ThroughLedger, DroppedPoints: dropped}
	if !c.ThroughTime.IsZero() {
		t := WireTime(c.ThroughTime.UTC())
		v.HistoryThroughTime = &t
	}
	return v
}

// handleLendingReserveHistory serves GET /v1/lending/pools/{pool}/reserves/{asset}/history: the reserve's on-chain state at
// each bucket in which it changed, with the live reserves endpoint's rate model; clients carry a point forward.
func (s *Server) handleLendingReserveHistory(w http.ResponseWriter, r *http.Request) {
	pool, asset := r.PathValue("pool"), r.PathValue("asset")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	if !canonical.IsContractID(asset) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset",
			"Invalid asset", http.StatusBadRequest, "asset must be the reserve token's C-strkey contract id")
		return
	}
	q, interval, ok := parseLendingHistoryQuery(w, r)
	if !ok {
		return
	}
	hr, ok := s.lendingHistoryReaderFor(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	if _, _, ok := s.lendingHistoryLineage(ctx, w, r, pool); !ok {
		return
	}
	h, err := hr.LendingReserveHistory(ctx, pool, asset, q)
	if err != nil {
		s.writeLendingHistoryError(ctx, w, r, "LendingReserveHistory", pool, err)
		return
	}
	view := reserveHistoryView(pool, asset, interval, q, h)
	flags := s.valueReserveHistory(ctx, &view, h, q, s.lendingHistoryFlags(ctx, view.lendingHistoryCoverageView))
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: flags})
}

func lastReserveState(points []clickhouse.LendingReservePoint) (clickhouse.BlendReserveState, bool) {
	for i := len(points) - 1; i >= 0; i-- {
		if !points[i].Removed {
			return points[i].State, true
		}
	}
	return clickhouse.BlendReserveState{}, false
}

func reserveHistoryView(pool, asset, interval string, q clickhouse.LendingHistoryQuery, h clickhouse.LendingReserveHistory) LendingReserveHistoryView {
	v := LendingReserveHistoryView{
		Pool: pool, Asset: asset, Protocol: nonEmptyPtr(h.Protocol), Interval: interval,
		From: WireTime(q.From.UTC()), To: WireTime(q.To.UTC()), ConfigDecimals: lendingConfigDecimals,
		lendingHistoryCoverageView: coverageView(h.Coverage, h.Undecodable),
		BeforeFirstState:           h.BeforeFirstState,
		Points:                     make([]LendingReserveHistoryPoint, 0, len(h.Points)),
	}
	if h.Protocol != "" {
		version := blend.PoolV2
		if h.Protocol == clickhouse.LendingBlendV1 {
			version = blend.PoolV1
		}
		rate, irMod := 12, irModDecimals(version)
		if version == blend.PoolV1 {
			rate = 9
		}
		v.RateDecimals, v.IRModDecimals = &rate, &irMod
	}
	for _, p := range h.Points {
		v.Points = append(v.Points, reserveHistoryPoint(p))
	}
	return v
}

// reserveHistoryPoint maps a point through reserveStateView, the live endpoint's own wire mapping.
func reserveHistoryPoint(p clickhouse.LendingReservePoint) LendingReserveHistoryPoint {
	out := LendingReserveHistoryPoint{
		T: WireTime(p.CloseTime), StateLedger: p.StateLedger, Removed: p.Removed,
		ConfigUncertain: p.ConfigUncertain || p.PoolConfigUncertain,
	}
	if p.ConfigLedger != 0 {
		cl := p.ConfigLedger
		out.ConfigLedger = &cl
	}
	if p.Removed {
		return out
	}
	rv := reserveStateView(p.State)
	util := rv.UtilizationPct
	out.Supplied, out.Borrowed, out.UtilizationPct = &rv.Supplied, &rv.Borrowed, &util
	out.BRate, out.DRate = bigString(p.State.Data.BRate), bigString(p.State.Data.DRate)
	out.IRMod, out.BackstopCredit = rv.IRMod, rv.BackstopCredit
	out.BorrowAPR, out.SupplyAPR = rv.BorrowAPR, rv.SupplyAPR
	if p.PoolConfigUncertain {
		out.SupplyAPR = nil
	}
	if c := rv.Config; c != nil {
		out.CollateralFactor, out.LiabilityFactor, out.SupplyCap = &c.CollateralFactor, &c.LiabilityFactor, c.SupplyCap
	}
	return out
}

// handleLendingBackstopHistory serves GET /v1/lending/pools/{pool}/backstop/history: each tier's PoolBalance at each bucket in
// which it changed. A Blend V1 pool gets the live backstop endpoint's unavailable shape; an unknown pool is a 404.
func (s *Server) handleLendingBackstopHistory(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	q, interval, ok := parseLendingHistoryQuery(w, r)
	if !ok {
		return
	}
	hr, ok := s.lendingHistoryReaderFor(w, r)
	if !ok {
		return
	}
	view := LendingBackstopHistoryView{
		Pool: pool, Interval: interval, From: WireTime(q.From.UTC()), To: WireTime(q.To.UTC()),
		Tiers: []LendingBackstopHistoryTier{},
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, reason, ok := s.lendingHistoryLineage(ctx, w, r, pool)
	if !ok {
		return
	}
	view.Protocol = nonEmptyPtr(protocol)
	if reason != "" {
		view.UnavailableReason = strRef(reason)
		writeJSON(w, view, Flags{})
		return
	}
	h, err := hr.LendingBackstopHistory(ctx, pool, protocol == blnt.SourceName, q)
	if err != nil {
		s.writeLendingHistoryError(ctx, w, r, "LendingBackstopHistory", pool, err)
		return
	}
	fillBackstopHistoryView(&view, h)
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: s.lendingHistoryFlags(ctx, view.lendingHistoryCoverageView)})
}

func fillBackstopHistoryView(view *LendingBackstopHistoryView, h clickhouse.LendingBackstopHistory) {
	view.lendingHistoryCoverageView = coverageView(h.Coverage, h.Undecodable)
	if h.Backstop == "" {
		view.UnavailableReason = strRef(backstopReasonNoState)
		return
	}
	view.Available, view.Backstop = true, strRef(h.Backstop)
	for _, t := range h.Tiers {
		tv := LendingBackstopHistoryTier{Tier: nonEmptyPtr(t.Tier), BeforeFirstState: t.BeforeFirstState, Points: make([]LendingBackstopHistoryPoint, 0, len(t.Points))}
		for _, p := range t.Points {
			pv := LendingBackstopHistoryPoint{T: WireTime(p.CloseTime), Ledger: p.Ledger, Removed: p.Removed}
			if !p.Removed {
				b := p.Balance
				pv.Tokens, pv.Shares, pv.QueuedShares = strRef(b.Tokens.String()), strRef(b.Shares.String()), strRef(b.Q4W.String())
				pv.QueuedPct = sharePct(b.Q4W, b.Shares)
			}
			tv.Points = append(tv.Points, pv)
		}
		view.Tiers = append(view.Tiers, tv)
	}
}
