package v1

import (
	"context"
	"math/big"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingRevenueReader is the lake read behind GET /v1/lending/pools/{pool}/revenue; each reserve's Hours are the rows
// its Buckets sum, so USD can value every hour at its own price.
type lendingRevenueReader interface {
	LendingPoolRevenue(ctx context.Context, pool, asset string, q clickhouse.LendingHistoryQuery) (clickhouse.LendingRevenue, error)
}

var _ lendingRevenueReader = (*clickhouse.ExplorerReader)(nil)

// The revenue route's exclusion reasons; any one sets lower_bound.
const (
	revenueReasonNotDerived  = "interest_not_derived"    // the pool, or part of the window, has no derived interest
	revenueReasonDeriveStale = "derive_stale"            // hour rows of another derive revision, left out
	revenueReasonRedoPending = "redo_pending"            // hours a pending re-derive has yet to rewrite, left out
	revenueReasonAccrual     = "accrual_excluded"        // versions accrual could not order or refused, counted not summed
	revenueReasonSupplyLoss  = "unexplained_supply_loss" // a supplier loss no event explains, not in default_loss
	revenueFeeNotDerivable   = "not_derivable"           // BLNT fee withdrawals are not derived yet
	revenueFeeNotApplicable  = "not_applicable"          // Blend has no protocol fee to withdraw

	revenueKindPool, revenueKindWindow, revenueKindReserve = "pool", "window", "reserve"
)

// The interest pass runs in lending-state.service (lending-state.timer: OnUnitInactiveSec=10min, RandomizedDelaySec=120)
// and writes only whole hours, so an hour is due once it has closed and one run has followed; the grace absorbs run time.
const (
	lendingInterestDeriveCadence = 12 * time.Minute
	lendingRevenueStaleGrace     = 30 * time.Minute
)

// LendingRevenueView is the GET /v1/lending/pools/{pool}/revenue body: interest booked per reserve, in base units.
type LendingRevenueView struct {
	Pool     string  `json:"pool"`
	Protocol *string `json:"protocol"`
	Interval string  `json:"interval"`
	// From and To bound the hours summed: [from, to), to rounded down to its hour.
	From             WireTime                    `json:"from"`
	To               WireTime                    `json:"to"`
	Coverage         LendingRevenueCoverage      `json:"coverage"`
	LowerBound       bool                        `json:"lower_bound"`
	Excluded         []LendingRevenueExclusion   `json:"excluded"`
	PriceMaxAgeHours int                         `json:"price_max_age_hours"`
	TotalsUSD        LendingRevenueTotalsUSD     `json:"totals_usd"`
	Reserves         []LendingRevenueReserveView `json:"reserves"`
}

// LendingRevenueCoverage is what the interest rows cover: from_ledger (the derive floor) through through_ledger, whose
// hour through_hour is the last served; derive_rev is the only revision summed.
type LendingRevenueCoverage struct {
	DeriveRev     uint16    `json:"derive_rev"`
	FromLedger    *uint32   `json:"from_ledger"`
	ThroughLedger *uint32   `json:"through_ledger"`
	ThroughHour   *WireTime `json:"through_hour"`
}

// LendingRevenueExclusion names something left out of, or uncertain in, the served sums.
type LendingRevenueExclusion struct {
	Kind   string  `json:"kind"` // pool | window | reserve
	Asset  *string `json:"asset"`
	Reason string  `json:"reason"`
	Amount *string `json:"amount"` // base units, ≥ 0; unexplained_supply_loss only
	Count  *int64  `json:"count"`  // hour rows (derive_stale) or versions (accrual_excluded, unexplained_supply_loss)
}

// LendingRevenueReserveView is one reserve's totals and its buckets that booked interest. Totals is null when an
// exclusion left nothing summed, so a zero is never served for rows that were left out.
type LendingRevenueReserveView struct {
	Asset           string                 `json:"asset"`
	Decimals        *uint32                `json:"decimals"`
	FeeRateInferred bool                   `json:"fee_rate_inferred"`
	LowerBound      bool                   `json:"lower_bound"`
	Totals          *LendingRevenueAmounts `json:"totals"`
	USD             LendingRevenueUSD      `json:"usd"`
	Points          []LendingRevenuePoint  `json:"points"`
}

// LendingRevenuePoint is one bucket's sums; Partial marks a bucket the window or the coverage cuts short.
type LendingRevenuePoint struct {
	T       WireTime `json:"t"`
	Partial bool     `json:"partial"`
	LendingRevenueAmounts
	USD LendingRevenueUSD `json:"usd"`
}

// LendingRevenueAmounts are base-unit decimal strings; interest_accrued = supplier_share + backstop_take + protocol_fee.
// Losses are magnitudes (≥ 0), so a partial sum of them is a lower bound like every other amount.
type LendingRevenueAmounts struct {
	InterestAccrued               string                 `json:"interest_accrued"`
	SupplierShare                 string                 `json:"supplier_share"`
	SupplierCredited              string                 `json:"supplier_credited"`
	BackstopTake                  string                 `json:"backstop_take"`
	ProtocolFee                   string                 `json:"protocol_fee"`
	Realised                      LendingRevenueRealised `json:"realised"`
	BackstopGulped                string                 `json:"backstop_gulped"`
	ReconcileBackstopLoss         string                 `json:"reconcile_backstop_loss"`
	DefaultLoss                   string                 `json:"default_loss"`
	DefaultVersions               int64                  `json:"default_versions"`
	UnexplainedSupplyLoss         string                 `json:"unexplained_supply_loss"`
	UnexplainedSupplyLossVersions int64                  `json:"unexplained_supply_loss_versions"`
	UnexplainedSupplyGain         string                 `json:"unexplained_supply_gain"`
	UnexplainedSupplyGainVersions int64                  `json:"unexplained_supply_gain_versions"`
	UnexplainedBackstopResidual   string                 `json:"unexplained_backstop_residual"`
	UnexplainedVersions           int64                  `json:"unexplained_versions"`
	UnorderedVersions             int64                  `json:"unordered_versions"`
	AccrualErrors                 int64                  `json:"accrual_errors"`
}

// LendingRevenueRealised is what left the pool: backstop credit drawn by interest auctions (always a lower bound), and
// BLNT fee withdrawals (null until derived, with the reason).
type LendingRevenueRealised struct {
	BackstopDrawn           string  `json:"backstop_drawn"`
	BackstopDrawnLowerBound bool    `json:"backstop_drawn_lower_bound"`
	FeeWithdrawn            *string `json:"fee_withdrawn"`
	FeeWithdrawnReason      string  `json:"fee_withdrawn_reason"`
}

// handleLendingPoolRevenue serves GET /v1/lending/pools/{pool}/revenue: interest accrued per reserve and bucket, in base
// units, from lending_interest_hourly. An unknown pool is a 404; a known pool without interest rows is an empty 200.
func (s *Server) handleLendingPoolRevenue(w http.ResponseWriter, r *http.Request) {
	pool, asset := r.PathValue("pool"), r.URL.Query().Get("asset")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	if asset != "" && !canonical.IsContractID(asset) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset",
			"Invalid asset", http.StatusBadRequest, "asset must be the reserve token's C-strkey contract id")
		return
	}
	q, interval, ok := parseLendingHistoryQuery(w, r)
	if !ok {
		return
	}
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	rr, ok := s.Explorer.(lendingRevenueReader)
	if !ok {
		writeLendingHistoryNotReady(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, _, ok := s.lendingHistoryLineage(ctx, w, r, pool)
	if !ok {
		return
	}
	rev, err := rr.LendingPoolRevenue(ctx, pool, asset, q)
	if err != nil {
		s.writeLendingHistoryError(ctx, w, r, "LendingPoolRevenue", pool, err)
		return
	}
	view := revenueView(pool, protocol, interval, q, rev)
	for i := range view.Reserves {
		st := clickhouse.BlendReserveState{Asset: view.Reserves[i].Asset}
		if dec, known := s.reserveDecimals(ctx, st); known {
			d := uint32(dec) //nolint:gosec // a token's decimals exponent is small
			view.Reserves[i].Decimals = &d
		}
	}
	flags := s.valueRevenue(ctx, &view, rev, s.revenueFlags(ctx, rev))
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: flags})
}

// revenueFlags is stale when the lake is, or the lake tip has run past the hour after the coverage by more than one
// derive run and the grace: the watermark moves only at hour ends, so a ledger lag would flicker every hour.
func (s *Server) revenueFlags(ctx context.Context, rev clickhouse.LendingRevenue) Flags {
	_, stale, ok := s.lakeWatermark(ctx)
	if stale || !ok || !rev.Covered {
		return Flags{Stale: stale}
	}
	_, tipTime, _, _ := s.lakeWatermarkCached()
	return Flags{Stale: revenueStale(rev.ThroughHour, tipTime)}
}

func revenueStale(throughHour, tipTime time.Time) bool {
	return tipTime.After(throughHour.Add(time.Hour + lendingInterestDeriveCadence + lendingRevenueStaleGrace))
}

// revenueView maps the read. A window exclusion makes every reserve a lower bound; a reserve's own exclusion makes it
// one, and nulls its totals when nothing of it was summed.
func revenueView(pool, protocol, interval string, q clickhouse.LendingHistoryQuery, rev clickhouse.LendingRevenue) LendingRevenueView {
	q = q.Aligned()
	to := q.To.UTC().Truncate(time.Hour)
	v := LendingRevenueView{
		Pool: pool, Protocol: nonEmptyPtr(protocol), Interval: interval, From: WireTime(q.From.UTC()), To: WireTime(to),
		Coverage: LendingRevenueCoverage{DeriveRev: lendinginterest.DeriveRev},
		Excluded: []LendingRevenueExclusion{}, Reserves: []LendingRevenueReserveView{},
	}
	exclude := func(kind, asset, reason string, amount *string, count *int64) {
		v.Excluded = append(v.Excluded, LendingRevenueExclusion{Kind: kind, Asset: nonEmptyPtr(asset), Reason: reason, Amount: amount, Count: count})
	}
	if !rev.Covered {
		exclude(revenueKindPool, "", revenueReasonNotDerived, nil, nil)
		v.LowerBound = true
		return v
	}
	th := WireTime(rev.ThroughHour)
	v.Coverage.ThroughLedger, v.Coverage.ThroughHour = &rev.ThroughLedger, &th
	if rev.FloorLedger != 0 {
		v.Coverage.FromLedger = &rev.FloorLedger
	}
	// The floor's own hour may hold ledgers before it; a window reaching it is not known complete.
	if rev.FloorLedger == 0 || q.From.Before(rev.FloorTime) || rev.ThroughHour.Add(time.Hour).Before(to) {
		exclude(revenueKindWindow, "", revenueReasonNotDerived, nil, nil)
	}
	if rev.Redo != nil && rev.RedoHour.Before(to) {
		exclude(revenueKindWindow, "", revenueReasonRedoPending, nil, nil)
	}
	windowPartial := len(v.Excluded) > 0
	end := to
	if rev.Before.After(time.Time{}) && rev.Before.Before(end) {
		end = rev.Before
	}
	width := time.Hour
	if q.Daily {
		width = 24 * time.Hour
	}
	fee := blnt.SourceName == protocol
	for _, res := range rev.Reserves {
		rv, own := revenueReserve(res, fee, width, end, windowPartial)
		v.Excluded = append(v.Excluded, own...)
		v.Reserves = append(v.Reserves, rv)
	}
	v.LowerBound = len(v.Excluded) > 0
	return v
}

// revenueReserve maps one reserve and returns the exclusions naming it.
func revenueReserve(res clickhouse.LendingRevenueReserve, fee bool, width time.Duration, end time.Time, windowPartial bool) (LendingRevenueReserveView, []LendingRevenueExclusion) {
	rv := LendingRevenueReserveView{Asset: res.Asset, FeeRateInferred: fee, Points: make([]LendingRevenuePoint, 0, len(res.Buckets))}
	total := clickhouse.NewLendingRevenueSums()
	for _, b := range res.Buckets {
		total.Add(b.Sums)
		rv.Points = append(rv.Points, LendingRevenuePoint{
			T: WireTime(b.Start), Partial: b.Start.Add(width).After(end),
			LendingRevenueAmounts: revenueAmounts(b.Sums, fee),
		})
	}
	var own []LendingRevenueExclusion
	exclude := func(reason string, amount *string, count int64) {
		own = append(own, LendingRevenueExclusion{Kind: revenueKindReserve, Asset: strRef(res.Asset), Reason: reason, Amount: amount, Count: &count})
	}
	if res.StaleRows > 0 {
		exclude(revenueReasonDeriveStale, nil, int64(res.StaleRows)) //nolint:gosec // an hour-row count
	}
	if n := total.UnorderedVersions + total.AccrualErrors; n > 0 {
		exclude(revenueReasonAccrual, nil, n)
	}
	if total.UnexplainedSupplyLoss.Sign() < 0 {
		exclude(revenueReasonSupplyLoss, strRef(new(big.Int).Neg(total.UnexplainedSupplyLoss).String()), total.UnexplainedSupplyLossVersions)
	}
	rv.LowerBound = windowPartial || len(own) > 0
	if !rv.LowerBound || total.Gross.Sign() != 0 {
		a := revenueAmounts(total, fee)
		rv.Totals = &a
	}
	return rv, own
}

// revenueAmounts serves the stored sums, the ≤ 0 columns as magnitudes.
func revenueAmounts(s clickhouse.LendingRevenueSums, fee bool) LendingRevenueAmounts {
	neg := func(x *big.Int) string { return new(big.Int).Neg(x).String() }
	a := LendingRevenueAmounts{
		InterestAccrued: s.Gross.String(), SupplierShare: s.SupplierIntended.String(), SupplierCredited: s.SupplierRealised.String(),
		BackstopTake: s.Backstop.String(), ProtocolFee: s.ProtocolFee.String(),
		Realised: LendingRevenueRealised{
			BackstopDrawn: neg(s.InterestDrawn), BackstopDrawnLowerBound: true, FeeWithdrawnReason: revenueFeeNotApplicable,
		},
		BackstopGulped:        s.Gulped.String(),
		ReconcileBackstopLoss: neg(s.ReconcileBackstopLoss),
		DefaultLoss:           neg(s.DefaultLoss), DefaultVersions: s.DefaultVersions,
		UnexplainedSupplyLoss: neg(s.UnexplainedSupplyLoss), UnexplainedSupplyLossVersions: s.UnexplainedSupplyLossVersions,
		UnexplainedSupplyGain: s.UnexplainedSupplyGain.String(), UnexplainedSupplyGainVersions: s.UnexplainedSupplyGainVersions,
		UnexplainedBackstopResidual: s.UnexplainedBackstopResidual.String(), UnexplainedVersions: s.UnexplainedVersions,
		UnorderedVersions: s.UnorderedVersions, AccrualErrors: s.AccrualErrors,
	}
	if fee {
		a.Realised.FeeWithdrawnReason = revenueFeeNotDerivable
	}
	return a
}
