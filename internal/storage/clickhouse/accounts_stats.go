package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// AccountsStats is the network-wide account analytics snapshot for the
// /accounts hub (operator request 2026-08-08), read from the
// ch-holders-rollup cycle's exchange-swapped tables. Stroops values are
// int64 here and serialized as STRINGS at the API layer (ADR-0003).
type AccountsStats struct {
	TotalAccounts            int64
	TotalTrustlines          int64
	TrustlineHoldingAccounts int64
	XLMTotalStroops          int64
	AvgStroops               int64
	MedianStroops            int64
	P90Stroops               int64
	P99Stroops               int64
	Top100XLMStroops         int64
	WealthHistogram          []WealthBucket
	TrustlineHistogram       []TrustlineBucket
	TopHeldAssets            []HeldAsset
	ComputedAt               time.Time
}

// WealthBucket is one log10 balance bucket: Bucket -1 = "< 1 XLM",
// 0 = 1–10 XLM, … 10 = ">= 10B XLM".
type WealthBucket struct {
	Bucket     int8
	Accounts   uint64
	XLMStroops int64
}

// TrustlineBucket is one trustlines-per-account band ("1", "2-5", …).
type TrustlineBucket struct {
	Bucket   string
	Accounts uint64
}

// HeldAsset is one row of the most-held-assets board.
type HeldAsset struct {
	Asset   string
	Holders int64
}

// topHeldAssetsLimit bounds the most-held board.
const topHeldAssetsLimit = 12

// AccountsStats reads the rollup snapshot. ok=false (not an error) when
// the rollup isn't provisioned/populated — the handler 503s with the
// standard warming message rather than serving zeros as facts.
//
// The four rollup tables below are exchanged as ONE atomic group by the
// writer (asset_holders_rollup.go's holdersRollupStatements, RA-2), but read
// here as four independent round trips — the swap can land between any two
// of them and blend two rollup cycles into one served snapshot (T346). Since
// the swap touches stellar.accounts_stats too, re-reading its computed_at
// stamp after the last query and comparing it to the value the first query
// (readStatsMetrics) captured detects that: a mismatch means a swap landed
// mid-read, so retry the whole read once. Two swaps landing inside one read
// is impossible at the rollup's 30-minute cadence.
func (r *ExplorerReader) AccountsStats(ctx context.Context) (AccountsStats, bool, error) {
	if !r.probeSchema(ctx, &r.accountsStatsProbe,
		`SELECT value FROM stellar.accounts_stats LIMIT 1`, true) {
		return AccountsStats{}, false, nil
	}
	s, consistent, err := r.readAccountsStatsCycle(ctx)
	if err != nil {
		return AccountsStats{}, false, err
	}
	if !consistent {
		s, _, err = r.readAccountsStatsCycle(ctx)
		if err != nil {
			return AccountsStats{}, false, err
		}
	}
	return s, true, nil
}

// readAccountsStatsCycle runs the four rollup reads and reports whether they
// all landed inside the same swap cycle (see AccountsStats).
func (r *ExplorerReader) readAccountsStatsCycle(ctx context.Context) (AccountsStats, bool, error) {
	var s AccountsStats
	if err := r.readStatsMetrics(ctx, &s); err != nil {
		return AccountsStats{}, false, err
	}
	if err := r.readWealthHistogram(ctx, &s); err != nil {
		return AccountsStats{}, false, err
	}
	if err := r.readTrustlineHistogram(ctx, &s); err != nil {
		return AccountsStats{}, false, err
	}
	if err := r.readTopHeldAssets(ctx, &s); err != nil {
		return AccountsStats{}, false, err
	}
	after, err := r.accountsStatsComputedAt(ctx)
	if err != nil {
		return AccountsStats{}, false, err
	}
	return s, after.Equal(s.ComputedAt), nil
}

// accountsStatsComputedAt is the live cycle stamp of stellar.accounts_stats,
// used both to seed ComputedAt (readStatsMetrics) and, re-read here, to
// detect a swap landing during AccountsStats' read sequence.
func (r *ExplorerReader) accountsStatsComputedAt(ctx context.Context) (time.Time, error) {
	var at time.Time
	if err := r.conn.QueryRow(ctx, `SELECT max(computed_at) FROM stellar.accounts_stats`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("clickhouse: accounts stats cycle marker: %w", err)
	}
	return at, nil
}

func (r *ExplorerReader) readStatsMetrics(ctx context.Context, s *AccountsStats) error {
	rows, err := r.conn.Query(ctx, `SELECT metric, value, computed_at FROM stellar.accounts_stats`)
	if err != nil {
		return fmt.Errorf("clickhouse: accounts stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	dst := map[string]*int64{
		"total_accounts":             &s.TotalAccounts,
		"total_trustlines":           &s.TotalTrustlines,
		"trustline_holding_accounts": &s.TrustlineHoldingAccounts,
		"xlm_total_stroops":          &s.XLMTotalStroops,
		"avg_stroops":                &s.AvgStroops,
		"median_stroops":             &s.MedianStroops,
		"p90_stroops":                &s.P90Stroops,
		"p99_stroops":                &s.P99Stroops,
		"top100_xlm_stroops":         &s.Top100XLMStroops,
	}
	for rows.Next() {
		var (
			metric string
			value  int64
			at     time.Time
		)
		if err := rows.Scan(&metric, &value, &at); err != nil {
			return fmt.Errorf("clickhouse: scan stats metric: %w", err)
		}
		if at.After(s.ComputedAt) {
			s.ComputedAt = at
		}
		if p, known := dst[metric]; known {
			*p = value
		}
	}
	return rows.Err()
}

func (r *ExplorerReader) readWealthHistogram(ctx context.Context, s *AccountsStats) error {
	rows, err := r.conn.Query(ctx, `SELECT bucket, accounts, xlm_stroops FROM stellar.accounts_wealth_histogram ORDER BY bucket`)
	if err != nil {
		return fmt.Errorf("clickhouse: wealth histogram: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b WealthBucket
		if err := rows.Scan(&b.Bucket, &b.Accounts, &b.XLMStroops); err != nil {
			return fmt.Errorf("clickhouse: scan wealth bucket: %w", err)
		}
		s.WealthHistogram = append(s.WealthHistogram, b)
	}
	return rows.Err()
}

func (r *ExplorerReader) readTrustlineHistogram(ctx context.Context, s *AccountsStats) error {
	rows, err := r.conn.Query(ctx, `SELECT bucket, accounts FROM stellar.accounts_trustline_histogram ORDER BY bucket`)
	if err != nil {
		return fmt.Errorf("clickhouse: trustline histogram: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b TrustlineBucket
		if err := rows.Scan(&b.Bucket, &b.Accounts); err != nil {
			return fmt.Errorf("clickhouse: scan trustline bucket: %w", err)
		}
		s.TrustlineHistogram = append(s.TrustlineHistogram, b)
	}
	return rows.Err()
}

func (r *ExplorerReader) readTopHeldAssets(ctx context.Context, s *AccountsStats) error {
	rows, err := r.conn.Query(ctx, `SELECT asset, holders FROM stellar.asset_holders_counts ORDER BY holders DESC LIMIT ?`, topHeldAssetsLimit)
	if err != nil {
		return fmt.Errorf("clickhouse: top held assets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h HeldAsset
		if err := rows.Scan(&h.Asset, &h.Holders); err != nil {
			return fmt.Errorf("clickhouse: scan held asset: %w", err)
		}
		s.TopHeldAssets = append(s.TopHeldAssets, h)
	}
	return rows.Err()
}
