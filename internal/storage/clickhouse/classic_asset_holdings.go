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
// WHY NO FINAL. ledger_entries_current is a ReplacingMergeTree whose FINAL
// resolves each (entry_type, key_xdr) to its highest-version row. This
// query groups on `asset` and takes min/max over `ledger_seq`, so the
// duplicate versions FINAL would collapse are folded by the GROUP BY
// anyway. Dropping FINAL removes the read-time merge — the single most
// expensive thing about scanning this table — for an answer that differs
// only in the direction of MORE evidence: every row present, superseded or
// not, is a ledger at which a trustline for this asset genuinely existed.
// max() is merge-invariant (the surviving row is the max-version one).
// min() is not — an unmerged older version can lower it — but the writer
// takes LEAST across runs, so successive scans can only move first_seen
// EARLIER, converging toward the truth instead of oscillating.
//
// WHY NO change_type FILTER. The holders rollup beside this one excludes
// `removed` because it is answering "who holds this now". This one is
// answering "does this asset exist", and a deleted trustline is still
// proof that it did. An asset whose every holder has since closed their
// line stays in the registry, which is correct: it existed, it has a
// history, and its issuer is still worth attesting.
//
// WHY THE SPILL SETTINGS ARE INHERITED, NOT RESTATED. [openRead] already
// sets max_bytes_before_external_group_by and max_bytes_before_external_sort.
// This query deliberately does NOT set optimize_aggregation_in_order:
// beside an external-group-by threshold that setting CANCELS the spill
// valve, and an aggregation that was going to spill becomes an OOM
// instead. There is nothing to gain from it here either — `asset` is not
// in the table sort key, so no aggregation order exists to exploit.
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
	   AND NOT startsWith(asset, 'pool')
	 GROUP BY asset
	 ORDER BY asset
	 LIMIT ?`

// TrustlineAssetsAfter returns up to `limit` distinct classic assets whose
// asset string sorts strictly after `after`, in ascending asset order.
//
// The walk is keyset-paginated rather than offset-paginated so a resumed
// run re-enters exactly where the previous one stopped, and so the page a
// caller gets does not shift under concurrent ingest. Pass "" for the
// first page; pass the last Asset of page N for page N+1. An empty result
// means the walk is complete.
//
// entry_type is the FIRST column of the table sort key, so the
// `entry_type = 'trustline'` predicate prunes granules rather than
// filtering rows — which is what makes paging affordable at all. Each page
// still re-aggregates the trustline range (the LIMIT cannot stop an
// aggregation early), so `limit` trades passes against per-pass memory:
// bigger pages mean fewer passes.
//
// Native XLM and pool-share trustlines are excluded in SQL because they
// have no (code, issuer) identity — the registry is keyed on one. The
// `pool` prefix covers both spellings TrustLineAssetID emits, the
// `pool:<hex>` form and the bare `pool` fallback.
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
