package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
)

// LendingInterestStoredRow is one lending_interest_hourly row under FINAL: the fold's stored columns in Row (its
// unstored fields zero), the event-reconciliation columns, and the derive revision that wrote it.
type LendingInterestStoredRow struct {
	Row                                   lendinginterest.HourRow
	ResidualExplained, ResidualAttributed *big.Int
	ExplainedVersions, AttributedVersions uint32
	DeriveRev                             uint16
}

const lendingInterestStoredQuery = `SELECT contract_id, protocol, asset, hour, first_ledger, last_ledger, versions,
		accrued_versions, gross, protocol_fee, backstop, supplier_intended, supplier_realised, backstop_credit_delta,
		unreconciled_versions, brate_mismatches, backstop_residual, supply_residual, default_loss, default_versions,
		fee_mismatches, fee_credit_residual, residual_explained, residual_attributed, explained_versions, attributed_versions,
		interest_drawn, gulped, reconcile_backstop_loss, unordered_versions, accrual_errors, error_samples, derive_rev
	FROM stellar.lending_interest_hourly FINAL
	WHERE contract_id IN ? AND hour >= ? AND hour < ?
	ORDER BY contract_id, asset, hour`

// LendingInterestStoredRows reads pools' stored rows of hours [fromHour, toHour) under FINAL, for the proof.
func LendingInterestStoredRows(ctx context.Context, addr string, pools []string, fromHour, toHour time.Time) ([]LendingInterestStoredRow, error) {
	if len(pools) == 0 || !toHour.After(fromHour) {
		return nil, errors.New("clickhouse: lending interest stored rows: no pools or an empty hour range")
	}
	ids := make([]any, 0, len(pools))
	for _, p := range pools {
		ids = append(ids, p)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_execution_time": 3600}))
	rows, err := conn.Query(ctx, lendingInterestStoredQuery, clickhouse.GroupSet{Value: ids}, fromHour.UTC(), toHour.UTC())
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest stored rows [%s,%s): %w", fromHour, toHour, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingInterestStoredRow
	for rows.Next() {
		s, err := scanLendingInterestStored(rows)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest stored rows [%s,%s): %w", fromHour, toHour, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanLendingInterestStored(rows stateRows) (LendingInterestStoredRow, error) {
	var s LendingInterestStoredRow
	var proto string
	var n [15]big.Int
	r := &s.Row
	if err := rows.Scan(&r.Pool, &proto, &r.Asset, &r.Hour, &r.FirstLedger, &r.LastLedger, &r.Versions, &r.AccruedVersions,
		&n[0], &n[1], &n[2], &n[3], &n[4], &n[5], &r.UnreconciledVersions, &r.BRateMismatches, &n[6], &n[7], &n[11],
		&r.DefaultVersions, &r.FeeMismatches, &n[8], &n[9], &n[10], &s.ExplainedVersions, &s.AttributedVersions,
		&n[12], &n[13], &n[14], &r.UnorderedVersions, &r.AccrualErrors, &r.ErrorSamples, &s.DeriveRev); err != nil {
		return s, err
	}
	r.Protocol, r.Hour = lendinginterest.Protocol(proto), r.Hour.UTC()
	r.Gross, r.ProtocolFee, r.Backstop, r.SupplierIntended, r.SupplierRealised = &n[0], &n[1], &n[2], &n[3], &n[4]
	r.BackstopCreditDelta, r.BackstopResidual, r.SupplyResidual, r.FeeCreditResidual = &n[5], &n[6], &n[7], &n[8]
	s.ResidualExplained, s.ResidualAttributed, r.DefaultLoss = &n[9], &n[10], &n[11]
	r.InterestDrawn, r.Gulped, r.ReconcileBackstopLoss = &n[12], &n[13], &n[14]
	return s, nil
}

// LendingInterestRowID names one stored row.
type LendingInterestRowID struct {
	Pool, Asset string
	Hour        time.Time
	DeriveRev   uint16
}

// LendingInterestRowsBeyond lists the stored rows past a proof's reach: hours after `after`, and every row of a
// contract not in pools. The caller sorts out each pool's own cut.
func LendingInterestRowsBeyond(ctx context.Context, addr string, pools []string, after time.Time) ([]LendingInterestRowID, error) {
	if len(pools) == 0 {
		return nil, errors.New("clickhouse: lending interest rows beyond: no pools")
	}
	ids := make([]any, 0, len(pools))
	for _, p := range pools {
		ids = append(ids, p)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.Query(ctx, `SELECT contract_id, asset, hour, derive_rev FROM stellar.lending_interest_hourly FINAL
		WHERE hour > ? OR contract_id NOT IN ? ORDER BY contract_id, asset, hour`, after.UTC(), clickhouse.GroupSet{Value: ids})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: lending interest rows after %s: %w", after, err)
	}
	defer func() { _ = rows.Close() }()
	var out []LendingInterestRowID
	for rows.Next() {
		var r LendingInterestRowID
		if err := rows.Scan(&r.Pool, &r.Asset, &r.Hour, &r.DeriveRev); err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest rows after %s: %w", after, err)
		}
		r.Hour = r.Hour.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
