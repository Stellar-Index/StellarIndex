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

// lendingRevenueReader is the lake read behind GET /v1/lending/pools/{pool}/revenue.
type lendingRevenueReader interface {
	LendingPoolRevenue(ctx context.Context, pool, asset string, q clickhouse.LendingHistoryQuery) (clickhouse.LendingRevenue, error)
}

var _ lendingRevenueReader = (*clickhouse.ExplorerReader)(nil)

// The revenue route's exclusion reasons; any one sets lower_bound.
const (
	revenueReasonNotDerived                                                  = "interest_not_derived"    // the pool, or part of the window, has no derived interest
	revenueReasonDeriveStale                                                 = "derive_stale"            // hour rows of another derive revision, left out
	revenueReasonRedoPending                                                 = "redo_pending"            // hours a pending re-derive has yet to rewrite, left out
	revenueReasonAccrual                                                     = "accrual_excluded"        // versions accrual could not order or refused, counted not summed
	revenueReasonSupplyLoss                                                  = "unexplained_supply_loss" // a supplier loss no event explains, not in default_loss
	revenueReasonDrawnPartial                                                = "interest_drawn_partial"  // realised.backstop_drawn omits attributed and unexplained draws
	revenueFeeNotDerivable                                                   = "not_derivable"           // BLNT fee withdrawals are not derived yet
	revenueFeeNotApplicable                                                  = "not_applicable"          // Blend has no protocol fee to withdraw
	revenueKindPool, revenueKindWindow, revenueKindReserve, revenueKindField = "pool", "window", "reserve", "field"
)

// LendingRevenueView is the GET /v1/lending/pools/{pool}/revenue body: interest booked per reserve, in base units.
type LendingRevenueView struct {
	Pool     string  `json:"pool"`
	Protocol *string `json:"protocol"`
	Interval string  `json:"interval"`
	// From and To bound the hours summed: [from, to), to rounded down to its hour.
	From       WireTime                    `json:"from"`
	To         WireTime                    `json:"to"`
	Coverage   LendingRevenueCoverage      `json:"coverage"`
	LowerBound bool                        `json:"lower_bound"`
	Excluded   []LendingRevenueExclusion   `json:"excluded"`
	Reserves   []LendingRevenueReserveView `json:"reserves"`
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
	Kind   string  `json:"kind"` // pool | window | reserve | field
	Asset  *string `json:"asset"`
	Reason string  `json:"reason"`
	Amount *string `json:"amount"` // base units; unexplained_supply_loss only
	Count  *int64  `json:"count"`  // hour rows (derive_stale) or versions (accrual_excluded, unexplained_supply_loss)
}

// LendingRevenueReserveView is one reserve's totals and its buckets that booked interest.
type LendingRevenueReserveView struct {
	Asset           string                `json:"asset"`
	Decimals        *uint32               `json:"decimals"`
	FeeRateInferred bool                  `json:"fee_rate_inferred"`
	Totals          LendingRevenueAmounts `json:"totals"`
	Points          []LendingRevenuePoint `json:"points"`
}

// LendingRevenuePoint is one bucket's sums.
type LendingRevenuePoint struct {
	T WireTime `json:"t"`
	LendingRevenueAmounts
}

// LendingRevenueAmounts are base-unit decimal strings; interest_accrued = supplier_share + backstop_take + protocol_fee.
type LendingRevenueAmounts struct {
	InterestAccrued             string                 `json:"interest_accrued"`
	SupplierShare               string                 `json:"supplier_share"`
	SupplierCredited            string                 `json:"supplier_credited"`
	BackstopTake                string                 `json:"backstop_take"`
	ProtocolFee                 string                 `json:"protocol_fee"`
	ProtocolRevenue             string                 `json:"protocol_revenue"`
	Realised                    LendingRevenueRealised `json:"realised"`
	BackstopGulped              string                 `json:"backstop_gulped"`
	ReconcileBackstopLoss       string                 `json:"reconcile_backstop_loss"`
	DefaultLoss                 string                 `json:"default_loss"`
	DefaultVersions             int64                  `json:"default_versions"`
	UnexplainedSupplyLoss       string                 `json:"unexplained_supply_loss"`
	UnexplainedSupplyVersions   int64                  `json:"unexplained_supply_versions"`
	UnexplainedBackstopResidual string                 `json:"unexplained_backstop_residual"`
	UnexplainedVersions         int64                  `json:"unexplained_versions"`
	UnorderedVersions           int64                  `json:"unordered_versions"`
	AccrualErrors               int64                  `json:"accrual_errors"`
}

// LendingRevenueRealised is what left the pool: backstop credit drawn by interest auctions (a lower bound), and BLNT fee
// withdrawals (null until derived, with the reason).
type LendingRevenueRealised struct {
	BackstopDrawn      string  `json:"backstop_drawn"`
	FeeWithdrawn       *string `json:"fee_withdrawn"`
	FeeWithdrawnReason string  `json:"fee_withdrawn_reason"`
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
	var flags Flags
	if rev.Covered {
		flags = s.lendingHistoryFlags(ctx, lendingHistoryCoverageView{HistoryThroughLedger: rev.ThroughLedger})
	}
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: flags})
}

// revenueView maps the read; every exclusion it names sets lower_bound.
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
	exclude(revenueKindField, "", revenueReasonDrawnPartial, nil, nil)
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
	fee := blnt.SourceName == protocol
	for _, res := range rev.Reserves {
		rv := LendingRevenueReserveView{Asset: res.Asset, FeeRateInferred: fee, Points: make([]LendingRevenuePoint, 0, len(res.Buckets))}
		total := clickhouse.NewLendingRevenueSums()
		for _, b := range res.Buckets {
			total.Add(b.Sums)
			rv.Points = append(rv.Points, LendingRevenuePoint{T: WireTime(b.Start), LendingRevenueAmounts: revenueAmounts(b.Sums, fee)})
		}
		rv.Totals = revenueAmounts(total, fee)
		if res.StaleRows > 0 {
			n := int64(res.StaleRows) //nolint:gosec // an hour-row count
			exclude(revenueKindReserve, res.Asset, revenueReasonDeriveStale, nil, &n)
		}
		if n := total.UnorderedVersions + total.AccrualErrors; n > 0 {
			exclude(revenueKindReserve, res.Asset, revenueReasonAccrual, nil, &n)
		}
		if total.UnexplainedSupplyLoss.Sign() < 0 {
			n := total.UnexplainedSupplyVersions
			exclude(revenueKindReserve, res.Asset, revenueReasonSupplyLoss, strRef(total.UnexplainedSupplyLoss.String()), &n)
		}
		v.Reserves = append(v.Reserves, rv)
	}
	v.LowerBound = len(v.Excluded) > 0
	return v
}

func revenueAmounts(s clickhouse.LendingRevenueSums, fee bool) LendingRevenueAmounts {
	a := LendingRevenueAmounts{
		InterestAccrued: s.Gross.String(), SupplierShare: s.SupplierIntended.String(), SupplierCredited: s.SupplierRealised.String(),
		BackstopTake: s.Backstop.String(), ProtocolFee: s.ProtocolFee.String(),
		ProtocolRevenue: new(big.Int).Add(s.Backstop, s.ProtocolFee).String(),
		// interest_drawn is the credit's change, ≤ 0; drawn is its magnitude.
		Realised:              LendingRevenueRealised{BackstopDrawn: new(big.Int).Neg(s.InterestDrawn).String(), FeeWithdrawnReason: revenueFeeNotApplicable},
		BackstopGulped:        s.Gulped.String(),
		ReconcileBackstopLoss: s.ReconcileBackstopLoss.String(),
		DefaultLoss:           s.DefaultLoss.String(), DefaultVersions: s.DefaultVersions,
		UnexplainedSupplyLoss: s.UnexplainedSupplyLoss.String(), UnexplainedSupplyVersions: s.UnexplainedSupplyVersions,
		UnexplainedBackstopResidual: s.UnexplainedBackstopResidual.String(), UnexplainedVersions: s.UnexplainedVersions,
		UnorderedVersions: s.UnorderedVersions, AccrualErrors: s.AccrualErrors,
	}
	if fee {
		a.Realised.FeeWithdrawnReason = revenueFeeNotDerivable
	}
	return a
}
