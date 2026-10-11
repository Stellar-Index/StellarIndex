// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// The account graph's activity history: one account's creation and sponsorship
// activity resolved into calendar months.
//
// The served tier stores EDGES, one row per distinct (creator, created) or
// (sponsor, sponsored) pair, with only first_at and last_at. Per-event times exist
// only in the rollups' working tables (account_creators_ops, account_sponsors_ops),
// which each cycle TRUNCATEs and refills, so reading one mid-cycle serves a partial
// archive (deploy/clickhouse/account_creators_rollup.sql). Two facts are derivable and kept apart:
//
//   - NEW ACCOUNTS per month: counterparties first created/sponsored that month.
//     COMPLETE: summed over all months it equals the distinct-counterparty total.
//   - EVENTS per month: an edge with N > 1 events places exactly two (first_at,
//     last_at); the other N-2 are counted as UNPLACED, never smeared into a month.
//
// events == placed + unplaced (TestAccountGraphHistoryPlacementIdentity).
//
// A month with nothing in it emits no point and nothing is carried forward. Coverage is the span
// the rollup aggregated (ADR-0031): an absent month inside it was quiet, one outside it was not
// observed.

// accountGraphHistoryPeriodsQuery buckets both arms' edges by month in one trip.
// Each sub-select is a PRIMARY-KEY RANGE READ (tables are ORDER BY (creator, created)
// and (sponsor, sponsored)). The first-event arm has w=1 and the last-event arm w=0,
// so new_accounts counts edges while events counts placed events. toDateTime(..., 'UTC')
// pins the zone because toStartOfMonth yields a zone-less Date.
const accountGraphHistoryPeriodsQuery = `
	SELECT rel, bucket, toUInt64(sum(w)) AS new_accounts, toUInt64(sum(ev)) AS events
	FROM (
	    SELECT '` + GraphRelationCreated + `' AS rel,
	           toDateTime(toStartOfMonth(first_at), 'UTC') AS bucket,
	           toUInt64(1) AS w, toUInt64(1) AS ev
	    FROM stellar.account_creator_edges
	    WHERE creator = ?
	    UNION ALL
	    SELECT '` + GraphRelationCreated + `' AS rel,
	           toDateTime(toStartOfMonth(last_at), 'UTC') AS bucket,
	           toUInt64(0) AS w, toUInt64(1) AS ev
	    FROM stellar.account_creator_edges
	    WHERE creator = ? AND creations > 1
	    UNION ALL
	    SELECT '` + GraphRelationSponsored + `' AS rel,
	           toDateTime(toStartOfMonth(first_at), 'UTC') AS bucket,
	           toUInt64(1) AS w, toUInt64(1) AS ev
	    FROM stellar.account_sponsor_edges
	    WHERE sponsor = ?
	    UNION ALL
	    SELECT '` + GraphRelationSponsored + `' AS rel,
	           toDateTime(toStartOfMonth(last_at), 'UTC') AS bucket,
	           toUInt64(0) AS w, toUInt64(1) AS ev
	    FROM stellar.account_sponsor_edges
	    WHERE sponsor = ? AND sponsorships_started > 1
	)
	GROUP BY rel, bucket
	ORDER BY rel, bucket`

// accountGraphHistoryTotalsQuery is the whole-history denominator, read off the same
// edge rows as the buckets. unplaced = greatest(events - 2, 0) per edge, computed in
// Int64 because a UInt64 `creations - 2` wraps for a single-event edge.
const accountGraphHistoryTotalsQuery = `
	SELECT '` + GraphRelationCreated + `' AS rel,
	       toUInt64(count()) AS accounts,
	       toUInt64(sum(creations)) AS events,
	       toUInt64(sum(greatest(toInt64(creations) - 2, 0))) AS unplaced
	FROM stellar.account_creator_edges
	WHERE creator = ?
	UNION ALL
	SELECT '` + GraphRelationSponsored + `' AS rel,
	       toUInt64(count()) AS accounts,
	       toUInt64(sum(sponsorships_started)) AS events,
	       toUInt64(sum(greatest(toInt64(sponsorships_started) - 2, 0))) AS unplaced
	FROM stellar.account_sponsor_edges
	WHERE sponsor = ?`

// AccountGraphHistoryPoint is one calendar month of one relation's activity; a month
// with neither a new counterparty nor a placed event has no point.
type AccountGraphHistoryPoint struct {
	// Start is the first instant of the month, UTC.
	Start time.Time
	// NewAccounts is counterparties first created/sponsored in the month; complete, it
	// sums to AccountGraphHistorySide.Accounts.
	NewAccounts uint64
	// Events is the events KNOWN to fall in the month; a lower bound when the side has
	// unplaced events.
	Events uint64
}

// AccountGraphHistorySide is one relation's series plus the whole-history
// totals that qualify it.
type AccountGraphHistorySide struct {
	Points []AccountGraphHistoryPoint
	// Accounts is distinct counterparties; Events the operations behind them. Exact.
	Accounts uint64
	Events   uint64
	// EventsPlaced is the events the points account for, EventsUnplaced the repeat events
	// on multi-event edges whose month is not recorded; they sum to Events.
	EventsPlaced   uint64
	EventsUnplaced uint64
	// Coverage is the span the rollup cycle that built this arm aggregated; the arms
	// differ (creation reaches genesis, sponsorship only protocol 14).
	Coverage AccountGraphCoverage
}

// AccountGraphHistory is one account's activity over time in both graph
// relations. Everything here is history; nothing is a live-state figure.
type AccountGraphHistory struct {
	Created   AccountGraphHistorySide
	Sponsored AccountGraphHistorySide
}

// AccountGraphHistory reads one account's monthly activity in both relations.
// Every read is a primary-key range on (creator, ...) or (sponsor, ...), so cost is
// that account's own edges (63 ms at max_threads=2 for the busiest creator's 1.57M
// rows); an SWR cache would add an unbounded key space for nothing.
// ok=false (not an error) when either arm is not exchanged live yet: a
// half-provisioned graph would claim the account never created anything.
func (r *ExplorerReader) AccountGraphHistory(ctx context.Context, account string) (AccountGraphHistory, bool, error) {
	if !r.probeSchema(ctx, &r.accountCreatorEdgesProbe,
		`SELECT creator FROM stellar.account_creator_edges LIMIT 1`, true) {
		return AccountGraphHistory{}, false, nil
	}
	if !r.probeSchema(ctx, &r.accountSponsorEdgesProbe,
		`SELECT sponsor FROM stellar.account_sponsor_edges LIMIT 1`, true) {
		return AccountGraphHistory{}, false, nil
	}

	var out AccountGraphHistory
	if err := r.readAccountGraphHistoryTotals(ctx, &out, account); err != nil {
		return AccountGraphHistory{}, false, err
	}
	if err := r.readAccountGraphHistoryPeriods(ctx, &out, account); err != nil {
		return AccountGraphHistory{}, false, err
	}
	if err := r.readAccountGraphHistoryCoverage(ctx, &out); err != nil {
		return AccountGraphHistory{}, false, err
	}
	return out, true, nil
}

func (r *ExplorerReader) readAccountGraphHistoryTotals(ctx context.Context, out *AccountGraphHistory, account string) error {
	rows, err := r.conn.Query(ctx, accountGraphHistoryTotalsQuery, account, account)
	if err != nil {
		return fmt.Errorf("clickhouse: account graph history totals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			rel                        string
			accounts, events, unplaced uint64
		)
		if err := rows.Scan(&rel, &accounts, &events, &unplaced); err != nil {
			return fmt.Errorf("clickhouse: scan account graph history totals: %w", err)
		}
		side := out.sideFor(rel)
		side.Accounts = accounts
		side.Events = events
		side.EventsUnplaced = unplaced
		// Derived so the pair cannot disagree with the total. The clamp is unreachable on
		// real rows but UInt64 underflow would serve ~18 quintillion placed events.
		if unplaced > events {
			unplaced = events
			side.EventsUnplaced = unplaced
		}
		side.EventsPlaced = events - unplaced
	}
	return rows.Err()
}

func (r *ExplorerReader) readAccountGraphHistoryPeriods(ctx context.Context, out *AccountGraphHistory, account string) error {
	rows, err := r.conn.Query(ctx, accountGraphHistoryPeriodsQuery,
		account, account, account, account)
	if err != nil {
		return fmt.Errorf("clickhouse: account graph history periods: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			rel   string
			point AccountGraphHistoryPoint
		)
		if err := rows.Scan(&rel, &point.Start, &point.NewAccounts, &point.Events); err != nil {
			return fmt.Errorf("clickhouse: scan account graph history period: %w", err)
		}
		point.Start = point.Start.UTC()
		side := out.sideFor(rel)
		side.Points = append(side.Points, point)
	}
	return rows.Err()
}

// readAccountGraphHistoryCoverage reads both arms' spans off the stats tables
// AccountGraph reads, so the series carries the same qualification.
func (r *ExplorerReader) readAccountGraphHistoryCoverage(ctx context.Context, out *AccountGraphHistory) error {
	var graph AccountGraph
	if err := r.readAccountGraphCoverage(ctx, &graph); err != nil {
		return err
	}
	out.Created.Coverage = graph.CreationCoverage
	out.Sponsored.Coverage = graph.SponsorshipCoverage
	return nil
}

// sideFor routes a `rel` literal to its side; anything but GraphRelationSponsored
// falls through to creation, as readAccountGraphCoverage does.
func (h *AccountGraphHistory) sideFor(rel string) *AccountGraphHistorySide {
	if rel == GraphRelationSponsored {
		return &h.Sponsored
	}
	return &h.Created
}
