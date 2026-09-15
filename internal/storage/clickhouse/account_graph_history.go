// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// The account graph's activity history: one account's creation and
// sponsorship activity resolved into calendar months (#351).
//
// # What the served tier can and cannot place in time
//
// The graph is stored as EDGES — one row per distinct (creator, created)
// or (sponsor, sponsored) pair — and an edge carries exactly two
// timestamps, first_at and last_at, whatever the number of events behind
// it. The per-event timestamps exist only in the rollups' WORKING tables
// (stellar.account_creators_ops, stellar.account_sponsors_ops), which
// every cycle TRUNCATEs and refills across 65 walked windows; reading one
// mid-cycle serves a PARTIAL archive, which is why the graph tables were
// staged and EXCHANGEd in the first place (deploy/clickhouse/
// account_creators_rollup.sql, "WHY NOT READ stellar.account_creators_ops
// DIRECTLY"). So a per-event time series is not available from the served
// tier and this reader does not invent one.
//
// What IS exactly derivable is two different facts, and they are kept
// apart on the wire because they have different completeness:
//
//   - NEW ACCOUNTS per month — how many counterparties this account
//     FIRST created or sponsored in that month. Every edge has exactly
//     one first_at, so this series is COMPLETE: summed over every month
//     it equals the account's distinct-counterparty total, exactly.
//
//   - EVENTS per month — how many individual creations or sponsorship
//     arrangements are known to fall in that month. An edge with one
//     event places it (first_at == last_at). An edge with N > 1 events
//     places exactly two of them, at first_at and at last_at; the other
//     N-2 happened somewhere between and the served tier does not record
//     where. Those are counted, exactly, as UNPLACED — never smeared
//     across the interval, never dropped silently, never folded into a
//     neighbouring month.
//
// The identity events == placed + unplaced holds by construction and is
// asserted in TestAccountGraphHistoryPlacementIdentity.
//
// # A month with nothing in it emits no point
//
// Points exist only for months that carry something. No zero row is
// written for a quiet month and no value is carried forward, for the
// reason every series in this API withholds rather than defaults: a
// reader cannot tell a zero that means "nothing happened" from a zero
// that means "we could not see". The distinction the caller needs is
// mechanical rather than guessed — Coverage is the span the rollup cycle
// actually aggregated (ADR-0031, data-derived), so a month absent from
// the points and INSIDE that span had no activity, and a month outside it
// was not observed at all.
//
// # Why calendar months
//
// The grain is fixed, not a parameter, and it is the finest grain the
// SOURCE justifies. Two timestamps per edge is all there is: a daily
// bucket would not place one extra event, it would only spread the same
// placed events over ~4,000 mostly-empty buckets and make the unplaced
// fraction read as scatter. Months give ~135 buckets over the creation
// arm's genesis-to-tip span — a table a person can read — while leaving
// every figure exact.

// accountGraphHistoryPeriodsQuery buckets both arms' edges by month in
// one round-trip.
//
// Each of the four sub-selects is a PRIMARY-KEY RANGE READ: the tables
// are ORDER BY (creator, created) and (sponsor, sponsored), so the cost
// of an arm is that one account's own edges, never the table. The two
// arms per relation are the two placeable events on an edge — its first
// and, when the edge carries more than one event, its last. The `w`
// column is 1 on the first-event arm and 0 on the last-event arm so that
// new_accounts counts EDGES while events counts placed EVENTS, from a
// single grouped pass.
//
// toDateTime(..., 'UTC') wraps each bucket because toStartOfMonth yields
// a Date, which carries no timezone; pinning it here keeps the served
// instant from depending on the driver's or the server's zone.
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

// accountGraphHistoryTotalsQuery is the whole-history denominator each
// series is qualified against, read off the same edge rows the buckets
// came from so the two cannot describe different data.
//
// `unplaced` is the count of events whose month is not recoverable:
// greatest(events - 2, 0) per edge, since an edge places its first and
// its last and nothing else. The subtraction is done in Int64 rather than
// on the UInt64 column because a UInt64 `creations - 2` WRAPS for a
// single-event edge — the surrounding guard would discard the wrapped
// value, but only after computing it, and a future edit to the guard
// would turn a silent wrap into a served figure.
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

// AccountGraphHistoryPoint is one calendar month of one relation's
// activity. A month with neither a new counterparty nor a placed event
// produces no point at all.
type AccountGraphHistoryPoint struct {
	// Start is the first instant of the month, UTC.
	Start time.Time
	// NewAccounts is how many counterparties this account first created
	// or first sponsored in the month. Complete: summed over the series
	// it equals AccountGraphHistorySide.Accounts exactly.
	NewAccounts uint64
	// Events is how many individual creations or sponsorship
	// arrangements are KNOWN to fall in the month. A lower bound
	// whenever the side reports unplaced events — see the file comment.
	Events uint64
}

// AccountGraphHistorySide is one relation's series plus the whole-history
// totals that qualify it.
type AccountGraphHistorySide struct {
	Points []AccountGraphHistoryPoint
	// Accounts is distinct counterparties; Events is the operations
	// behind them. Both are whole-history and exact.
	Accounts uint64
	Events   uint64
	// EventsPlaced is how many of Events the points account for, and
	// EventsUnplaced the remainder — repeat events on a multi-event edge,
	// whose month the served tier does not record. The two always sum to
	// Events.
	EventsPlaced   uint64
	EventsUnplaced uint64
	// Coverage is the span the rollup cycle that built this arm actually
	// aggregated. The two arms carry it separately because they are
	// separate cycles over separate sources: creation reaches genesis,
	// sponsorship only reaches protocol 14.
	Coverage AccountGraphCoverage
}

// AccountGraphHistory is one account's activity over time in both graph
// relations. Everything here is history; nothing is a live-state figure.
type AccountGraphHistory struct {
	Created   AccountGraphHistorySide
	Sponsored AccountGraphHistorySide
}

// AccountGraphHistory reads one account's monthly activity in both
// relations.
//
// Cost is that account's own edges, not the tables: every read is a
// primary-key range on (creator, …) or (sponsor, …). The comparable shape
// already on the served path — AccountGraph's outbound summary, the same
// range aggregated without a bucket — measures 63 ms at max_threads=2
// over the busiest creator's 1,569,693 rows on r1 2026-09-09, against a
// busiest OUTBOUND degree of 785,543 sponsorship edges and 193,015
// creation edges. That is why this is not behind an SWR cache: it is a
// fraction of what the same handler family already pays uncached, and a
// per-address cache would add an unbounded key space and a refresh-storm
// surface to buy nothing. If a future degree makes it expensive, the
// per-account detached-refresh pattern in account_state_cache.go is the
// shape to adopt — not a request-scoped scan.
//
// ok=false (not an error) when either graph arm has not been exchanged
// live yet, for the reason AccountGraph refuses the same case: a
// half-provisioned graph would answer "this account has never created
// anything", which is a claim, not an absence.
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
		// Derived rather than read so the served pair cannot disagree
		// with the served total, whatever the bucket pass returns.
		// greatest(events-2, 0) can never exceed sum(events), so the
		// clamp is unreachable on real rows — it is here because the
		// operands are UInt64 and an underflow would not fail, it would
		// serve 18 quintillion placed events.
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

// readAccountGraphHistoryCoverage reads both arms' data-derived spans off
// the same stats tables AccountGraph reads, so the series is qualified by
// the same span the graph beside it is.
func (r *ExplorerReader) readAccountGraphHistoryCoverage(ctx context.Context, out *AccountGraphHistory) error {
	var graph AccountGraph
	if err := r.readAccountGraphCoverage(ctx, &graph); err != nil {
		return err
	}
	out.Created.Coverage = graph.CreationCoverage
	out.Sponsored.Coverage = graph.SponsorshipCoverage
	return nil
}

// sideFor routes a `rel` discriminator onto its side. An unrecognised
// discriminator lands on the sponsorship side only if it equals
// GraphRelationSponsored; anything else is routed to the creation side,
// which is the same fallthrough readAccountGraphCoverage uses, and both
// discriminators are SQL literals in this file rather than user input.
func (h *AccountGraphHistory) sideFor(rel string) *AccountGraphHistorySide {
	if rel == GraphRelationSponsored {
		return &h.Sponsored
	}
	return &h.Created
}
