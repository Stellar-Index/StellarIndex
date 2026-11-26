package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
)

// LendingRevenueSums are summed lending_interest_hourly columns, in the reserve's base units.
type LendingRevenueSums struct {
	Gross, SupplierIntended, SupplierRealised, Backstop, ProtocolFee *big.Int
	InterestDrawn, Gulped, ReconcileBackstopLoss, DefaultLoss        *big.Int
	// UnexplainedSupplyLoss sums, hour by hour, only the negative part of supply_residual − default_loss: a loss to
	// suppliers no event explains, never offset by another hour's rounding gain.
	UnexplainedSupplyLoss                                                  *big.Int
	UnexplainedSupplyVersions                                              int64
	UnexplainedBackstopResidual                                            *big.Int
	DefaultVersions, UnexplainedVersions, UnorderedVersions, AccrualErrors int64
}

// NewLendingRevenueSums returns zero sums.
func NewLendingRevenueSums() LendingRevenueSums {
	var s LendingRevenueSums
	for _, p := range s.amounts() {
		*p = new(big.Int)
	}
	return s
}

func (s *LendingRevenueSums) amounts() []**big.Int {
	return []**big.Int{
		&s.Gross, &s.SupplierIntended, &s.SupplierRealised, &s.Backstop, &s.ProtocolFee, &s.InterestDrawn,
		&s.Gulped, &s.ReconcileBackstopLoss, &s.DefaultLoss, &s.UnexplainedSupplyLoss, &s.UnexplainedBackstopResidual,
	}
}

func (s *LendingRevenueSums) counts() []*int64 {
	return []*int64{&s.UnexplainedSupplyVersions, &s.DefaultVersions, &s.UnexplainedVersions, &s.UnorderedVersions, &s.AccrualErrors}
}

// Add adds o into s exactly.
func (s *LendingRevenueSums) Add(o LendingRevenueSums) {
	src := o.amounts()
	for i, p := range s.amounts() {
		(*p).Add(*p, *src[i])
	}
	cs := o.counts()
	for i, p := range s.counts() {
		*p += *cs[i]
	}
}

// LendingRevenueBucket is one served bucket (an hour, or a UTC day) that holds interest rows.
type LendingRevenueBucket struct {
	Start time.Time
	Sums  LendingRevenueSums
}

// LendingRevenueReserve is one reserve's buckets; StaleRows counts the hour rows left out for an older derive_rev.
type LendingRevenueReserve struct {
	Asset     string
	Buckets   []LendingRevenueBucket
	StaleRows uint64
}

// LendingRevenue is a pool's interest rows over the hours served: [From, Before), From the query's aligned start, Before
// the earliest of the query's end (rounded down to its hour), the hour after the pool's coverage, and a pending redo's
// first suspect hour.
type LendingRevenue struct {
	// Covered is false while the shared watermark or the pool's own coverage row is absent; nothing is read then.
	Covered       bool
	FloorLedger   uint32
	FloorTime     time.Time
	ThroughLedger uint32 // min(shared watermark, the pool's coverage row): an hour's last ledger
	ThroughHour   time.Time
	Redo          *LendingInterestRedo
	RedoHour      time.Time // the first hour a pending redo leaves suspect; zero when none clips the read
	From, Before  time.Time
	Reserves      []LendingRevenueReserve
}

// lendingRevenueQuery sums one pool's rows per reserve and bucket. FINAL collapses an hour's derives first and the
// derive_rev test sits outside it, so an older derive cannot resurface; rows of another revision are counted, not summed.
const lendingRevenueQuery = `WITH ? AS rev
	SELECT asset, if(? = 1, toStartOfDay(hour), hour) AS bucket, countIf(derive_rev = rev), countIf(derive_rev != rev),
		sumIf(gross, derive_rev = rev), sumIf(supplier_intended, derive_rev = rev), sumIf(supplier_realised, derive_rev = rev),
		sumIf(backstop, derive_rev = rev), sumIf(protocol_fee, derive_rev = rev), sumIf(interest_drawn, derive_rev = rev),
		sumIf(gulped, derive_rev = rev), sumIf(reconcile_backstop_loss, derive_rev = rev), sumIf(default_loss, derive_rev = rev),
		sumIf(supply_residual - default_loss, derive_rev = rev AND supply_residual - default_loss < 0),
		sumIf(toInt64(brate_mismatches) - toInt64(default_versions), derive_rev = rev AND supply_residual - default_loss < 0),
		sumIf(backstop_residual - residual_explained - residual_attributed, derive_rev = rev),
		sumIf(toInt64(default_versions), derive_rev = rev),
		sumIf(toInt64(unreconciled_versions) - toInt64(explained_versions) - toInt64(attributed_versions), derive_rev = rev),
		sumIf(toInt64(unordered_versions), derive_rev = rev), sumIf(toInt64(accrual_errors), derive_rev = rev)
	FROM (
		SELECT asset, hour, gross, supplier_intended, supplier_realised, backstop, protocol_fee, interest_drawn, gulped,
			reconcile_backstop_loss, default_loss, supply_residual, brate_mismatches, default_versions, backstop_residual,
			residual_explained, residual_attributed, unreconciled_versions, explained_versions, attributed_versions,
			unordered_versions, accrual_errors, derive_rev
		FROM stellar.lending_interest_hourly FINAL
		WHERE contract_id = ? AND (? = '' OR asset = ?) AND hour >= toDateTime(?, 'UTC') AND hour < toDateTime(?, 'UTC')
	)
	GROUP BY asset, bucket
	ORDER BY asset, bucket
	LIMIT ? BY asset
	SETTINGS optimize_move_to_prewhere_if_final = 0`

// LendingPoolRevenue reads a pool's interest per reserve (asset "" for all) and bucket over the hours wholly inside
// q's window that the interest rows cover; ErrLendingHistoryTooManyPoints past q.MaxPoints buckets for a reserve.
func (r *ExplorerReader) LendingPoolRevenue(ctx context.Context, pool, asset string, q LendingHistoryQuery) (LendingRevenue, error) {
	q = q.Aligned()
	out := LendingRevenue{From: q.From.UTC()}
	marks, err := lendingInterestKeyLedgers(ctx, r.conn, `SELECT name, max(thru_ledger) FROM stellar.entry_history_watermark
		WHERE name IN (?, ?, ?) GROUP BY name`, lendingInterestWatermarkName, lendingInterestFloorName, lendingInterestPoolPrefix+pool)
	if err != nil {
		return out, err
	}
	wm, poolThru := marks[lendingInterestWatermarkName], marks[lendingInterestPoolPrefix+pool]
	if wm == 0 || poolThru == 0 {
		return out, nil
	}
	out.Covered, out.ThroughLedger, out.FloorLedger = true, min(wm, poolThru), marks[lendingInterestFloorName]
	through, err := r.closeTimeOf(ctx, out.ThroughLedger)
	if err != nil {
		return out, err
	}
	out.ThroughHour = through.Truncate(time.Hour)
	if out.FloorLedger != 0 {
		if out.FloorTime, err = r.closeTimeOf(ctx, out.FloorLedger); err != nil {
			return out, err
		}
	}
	out.Before = minTime(q.To.UTC().Truncate(time.Hour), out.ThroughHour.Add(time.Hour))
	redo, pending, err := pendingLendingInterestRedo(ctx, r.conn)
	if err != nil {
		return out, err
	}
	if pending && redo.Next() <= out.ThroughLedger {
		out.Redo = &redo
		at, err := r.closeTimeOf(ctx, redo.Next())
		if err != nil {
			return out, err
		}
		out.RedoHour = at.Truncate(time.Hour)
		out.Before = minTime(out.Before, out.RedoHour)
	}
	if !out.Before.After(out.From) {
		return out, nil
	}
	out.Reserves, err = r.lendingRevenueRows(ctx, pool, asset, q, out.From, out.Before)
	return out, err
}

func (r *ExplorerReader) closeTimeOf(ctx context.Context, ledger uint32) (time.Time, error) {
	at, found, err := r.CloseTimeForLedger(ctx, ledger)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return time.Time{}, fmt.Errorf("clickhouse: lending revenue: the lake has no close time for ledger %d", ledger)
	}
	return at, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func (r *ExplorerReader) lendingRevenueRows(ctx context.Context, pool, asset string, q LendingHistoryQuery, from, before time.Time) ([]LendingRevenueReserve, error) {
	daily := 0
	if q.Daily {
		daily = 1
	}
	rows, err := r.conn.Query(ctx, lendingRevenueQuery, lendinginterest.DeriveRev, daily, pool, asset, asset,
		from.Unix(), before.Unix(), q.MaxPoints+1)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending revenue %s: %w", pool, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingRevenueReserve
	read := 0 // buckets of the current reserve, stale-only ones included: LIMIT BY counts them too
	for rows.Next() {
		var a string
		var bucket time.Time
		var current, stale uint64
		s := NewLendingRevenueSums()
		if err := rows.Scan(&a, &bucket, &current, &stale, s.Gross, s.SupplierIntended, s.SupplierRealised, s.Backstop,
			s.ProtocolFee, s.InterestDrawn, s.Gulped, s.ReconcileBackstopLoss, s.DefaultLoss, s.UnexplainedSupplyLoss,
			&s.UnexplainedSupplyVersions, s.UnexplainedBackstopResidual, &s.DefaultVersions, &s.UnexplainedVersions,
			&s.UnorderedVersions, &s.AccrualErrors); err != nil {
			return nil, fmt.Errorf("clickhouse: scan lending revenue %s: %w", pool, err)
		}
		if len(out) == 0 || out[len(out)-1].Asset != a {
			out, read = append(out, LendingRevenueReserve{Asset: a}), 0
		}
		if read++; read > q.MaxPoints {
			return nil, ErrLendingHistoryTooManyPoints
		}
		res := &out[len(out)-1]
		res.StaleRows += stale
		if current == 0 {
			continue
		}
		res.Buckets = append(res.Buckets, LendingRevenueBucket{Start: bucket.UTC(), Sums: s})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: lending revenue %s rows: %w", pool, err)
	}
	return out, nil
}
