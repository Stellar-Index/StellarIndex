// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TrustlineAssetSeed is one distinct classic asset the lake holds a
// trustline for, with the ledger bracket the evidence spans.
//
// It answers exactly one question — does this (code, issuer) EXIST on
// Stellar — and deliberately carries no balance, no holder count and no
// supply. Those have an owner already (the asset_holders_* rollup and the
// supply pipeline); duplicating them here, from a scan that runs on an
// operator cadence, would create a second stale answer to a question that
// already has a fresh one.
type TrustlineAssetSeed struct {
	// Asset is the lake's asset string, which for a credit trustline is
	// the canonical `CODE-GISSUER` form (internal/xdrjson.TrustLineAssetID)
	// and therefore parses with canonical.ParseAsset unchanged. The caller
	// parses it rather than splitting it: identity is (code, issuer), and a
	// string split on the first dash is not that.
	Asset string

	// FirstLedger / LastLedger bracket the ledgers at which a trustline
	// entry for this asset was last modified. NOT the asset genesis — see
	// the query note in [HoldingsScanner.TrustlineAssetsAfter].
	FirstLedger uint32
	LastLedger  uint32

	// FirstAt / LastAt are the close times of those ledgers, UTC.
	FirstAt time.Time
	LastAt  time.Time
}

// HoldingsScanner pages the lake's distinct trustline-held classic assets.
//
// It holds one connection from the heavy-read class ([openRead]) for the
// whole walk: unbounded execution time, a per-query memory ceiling, and
// external spill for both the aggregation and the sort, so a walk that
// outgrows memory costs time on the ZFS data pool instead of an OOM.
type HoldingsScanner struct {
	conn driver.Conn
}

// NewHoldingsScanner dials ClickHouse for a holdings walk.
func NewHoldingsScanner(ctx context.Context, addr string) (*HoldingsScanner, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	return &HoldingsScanner{conn: conn}, nil
}

// Close releases the connection.
func (h *HoldingsScanner) Close() error {
	if h == nil || h.conn == nil {
		return nil
	}
	return h.conn.Close()
}

// trustlineAssetsPageQuery is one page of the walk, keyset-paginated on
// the asset string.
//
// NO FINAL: the query groups on `asset` and takes min/max over `ledger_seq`,
// so the duplicate versions FINAL would collapse are folded by the GROUP BY
// anyway, and the read-time merge is the most expensive part of scanning this
// table. The answer differs only toward MORE evidence: any row present,
// superseded or not, is a ledger at which a trustline existed. max() is
// merge-invariant; min() is not, but the writer takes LEAST across runs, so
// first_seen can only move EARLIER, converging on the truth.
//
// NO change_type FILTER: this answers "does this asset exist", and a deleted
// trustline is still proof it did (the holders rollup excludes `removed`
// because it answers "who holds this now").
//
// Spill settings are inherited from [openRead]. Do NOT set
// optimize_aggregation_in_order here: beside an external-group-by threshold
// it CANCELS the spill valve and turns a spill into an OOM.
const trustlineAssetsPageQuery = `
	SELECT asset,
	       min(ledger_seq) AS first_ledger,
	       max(ledger_seq) AS last_ledger,
	       min(close_time) AS first_at,
	       max(close_time) AS last_at
	  FROM stellar.ledger_entries_current
	 WHERE entry_type = 'trustline'
	   AND asset > ?
	   AND asset != ''
	   AND asset != 'native'
	   AND asset NOT LIKE 'pool:%'
	   AND asset != 'pool'
	 GROUP BY asset
	 ORDER BY asset
	 LIMIT ?`

// trustlineAssetCountQuery counts the population the page walk enumerates;
// its predicates MUST match [trustlineAssetsPageQuery].
const trustlineAssetCountQuery = `
	SELECT uniqExact(asset)
	  FROM stellar.ledger_entries_current
	 WHERE entry_type = 'trustline'
	   AND asset != ''
	   AND asset != 'native'
	   AND asset NOT LIKE 'pool:%'
	   AND asset != 'pool'`

// CountTrustlineAssets returns how many distinct classic assets the walk
// would enumerate end to end.
func (h *HoldingsScanner) CountTrustlineAssets(ctx context.Context) (int64, error) {
	var n uint64
	if err := h.conn.QueryRow(ctx, trustlineAssetCountQuery).Scan(&n); err != nil {
		return 0, fmt.Errorf("clickhouse: count trustline assets: %w", err)
	}
	return int64(n), nil //nolint:gosec // a distinct-asset count cannot approach 2^63
}

// TrustlineAssetsAfter returns up to `limit` distinct classic assets whose
// asset string sorts strictly after `after`, in ascending asset order.
//
// Keyset- rather than offset-paginated so a resumed run re-enters exactly
// where the previous one stopped. Pass "" for the first page and the last
// Asset of page N for page N+1; an empty result means the walk is complete.
//
// entry_type is the FIRST sort-key column, so the `entry_type = 'trustline'`
// predicate prunes granules. Each page still re-aggregates the trustline
// range, so bigger pages mean fewer passes at more memory per pass.
//
// Native XLM and pool-share trustlines are excluded in SQL: they have no
// (code, issuer) identity. The two exact-match predicates cover the
// `pool:<hex>` form and the bare `pool` fallback without matching a real
// credit asset code starting with "pool" (codes are case-sensitive).
// (asset codes are case-sensitive and "poolX" etc. are valid).
func (h *HoldingsScanner) TrustlineAssetsAfter(ctx context.Context, after string, limit int) ([]TrustlineAssetSeed, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("clickhouse: TrustlineAssetsAfter: limit must be positive, got %d", limit)
	}
	rows, err := h.conn.Query(ctx, trustlineAssetsPageQuery, after, uint64(limit)) //nolint:gosec // limit is validated positive above
	if err != nil {
		return nil, fmt.Errorf("clickhouse: trustline asset page after %q: %w", after, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]TrustlineAssetSeed, 0, limit)
	for rows.Next() {
		var s TrustlineAssetSeed
		if err := rows.Scan(&s.Asset, &s.FirstLedger, &s.LastLedger, &s.FirstAt, &s.LastAt); err != nil {
			return nil, fmt.Errorf("clickhouse: scan trustline asset: %w", err)
		}
		s.FirstAt = s.FirstAt.UTC()
		s.LastAt = s.LastAt.UTC()
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: trustline asset page after %q: %w", after, err)
	}
	return out, nil
}
