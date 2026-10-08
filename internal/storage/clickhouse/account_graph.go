package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"
)

// The two relationships the graph models are never summed: a creation is immutable and once
// per (creator, created) pair per address lifetime; a sponsorship is revocable and repeatable.
const (
	GraphRelationCreated   = "created"
	GraphRelationSponsored = "sponsored"
)

// AccountGraphInboundCap bounds inbound edges per direction: a guard rail, not a page
// (measured: no account has more than 9 distinct creators or 8 sponsors). The served total is
// the exact count, so truncation is visible.
const AccountGraphInboundCap = 25

// AccountGraphEdge is one edge: a counterparty plus the relationship's weight. One row per
// distinct pair, not per operation; Events counts creations or sponsorship arrangements started.
type AccountGraphEdge struct {
	// Account is the other end: creator/sponsor on an inbound edge, created/sponsored outbound.
	Account string
	Events  uint64
	// FundedStroops is the starting balance summed over the creations behind this edge; set on
	// creation edges only (nil on sponsorship). Zero is real: a CAP-33 sponsored creation pays no reserve.
	FundedStroops *big.Int
	FirstLedger   uint32
	LastLedger    uint32
	FirstAt       time.Time
	LastAt        time.Time
}

// AccountGraphSide is the whole-history summary of one outbound direction, independent of
// any page. Accounts counts distinct counterparties, Events operations; they differ sharply
// for a recycling creator, so neither stands in for the other.
type AccountGraphSide struct {
	Accounts      uint64
	Events        uint64
	FundedStroops *big.Int
	FirstLedger   uint32
	LastLedger    uint32
	FirstAt       time.Time
	LastAt        time.Time
}

// AccountGraphCoverage is the span one arm aggregated, read from the cycle that built that
// arm's edges. The arms come from independent cycles and sources, so they stay separate:
// creation reaches genesis, sponsorship only protocol 14.
type AccountGraphCoverage struct {
	FromLedger uint32
	ThruLedger uint32
	FromTime   time.Time
	ThruTime   time.Time
	ComputedAt time.Time
}

// AccountGraph is one account's neighbourhood in the sponsorship and creation graph.
//
// Everything here is history. A sponsorship edge says an arrangement was started, never that
// one is in force: RevokeSponsorship names its entry inside undecoded body_xdr, so a
// revocation attributes to the issuer, not an edge, and arrangements also lapse on entry
// deletion or merge with no operation. No edge field claims a live sponsorship.
//
// Inbound (CreatedBy / SponsoredBy) is capped by AccountGraphInboundCap with an exact total;
// outbound is summarised (Created / Sponsored) and paged (Page) because the busiest sponsor
// covers 785,543 accounts.
type AccountGraph struct {
	// CreatedBy is a list because an address can be created, merged away and created again
	// (by the same or another funder); edges are collapsed to distinct pairs before serving.
	CreatedBy      []AccountGraphEdge
	CreatedByTotal uint64
	// SponsoredBy is every account that has ever begun a sponsorship covering this one.
	SponsoredBy      []AccountGraphEdge
	SponsoredByTotal uint64

	Created   AccountGraphSide
	Sponsored AccountGraphSide
	// RevocationsIssued counts RevokeSponsorship operations this account sourced: an
	// account-level fact and a lower bound on arrangements that ended.
	RevocationsIssued uint64

	CreationCoverage    AccountGraphCoverage
	SponsorshipCoverage AccountGraphCoverage

	// Page is the requested outbound direction's slice, keyset-ordered by counterparty id.
	Page []AccountGraphEdge
}

// accountGraphInboundQuery reads both inbound directions in one round-trip. `count() OVER ()`
// is evaluated before the LIMIT, so each arm carries the exact inbound count beside the capped
// slice. Both are primary-key range reads (ORDER BY (created, creator) / (sponsored, sponsor)).
const accountGraphInboundQuery = `
	SELECT * FROM (
	    SELECT '` + GraphRelationCreated + `' AS rel,
	           creator AS counterparty,
	           creations AS events,
	           funded_stroops AS funded,
	           first_ledger, last_ledger, first_at, last_at,
	           toUInt64(count() OVER ()) AS total
	    FROM stellar.account_creator_edges_by_created
	    WHERE created = ?
	    ORDER BY creator
	    LIMIT ?
	)
	UNION ALL
	SELECT * FROM (
	    SELECT '` + GraphRelationSponsored + `' AS rel,
	           sponsor AS counterparty,
	           sponsorships_started AS events,
	           toInt128(0) AS funded,
	           first_ledger, last_ledger, first_at, last_at,
	           toUInt64(count() OVER ()) AS total
	    FROM stellar.account_sponsor_edges_by_sponsored
	    WHERE sponsored = ?
	    ORDER BY sponsor
	    LIMIT ?
	)`

// accountGraphOutboundQuery summarises both outbound directions in one round-trip; each arm
// aggregates a primary-key range, so cost follows that account's edges, not the table.
// An arm with accounts = 0 means "no relationship", not a span starting at ledger 0.
const accountGraphOutboundQuery = `
	SELECT '` + GraphRelationCreated + `' AS rel,
	       toUInt64(count()) AS accounts,
	       toUInt64(sum(creations)) AS events,
	       sum(toInt128(funded_stroops)) AS funded,
	       toUInt32(min(first_ledger)) AS first_ledger,
	       toUInt32(max(last_ledger)) AS last_ledger,
	       min(first_at) AS first_at,
	       max(last_at) AS last_at
	FROM stellar.account_creator_edges
	WHERE creator = ?
	UNION ALL
	SELECT '` + GraphRelationSponsored + `' AS rel,
	       toUInt64(count()) AS accounts,
	       toUInt64(sum(sponsorships_started)) AS events,
	       toInt128(0) AS funded,
	       toUInt32(min(first_ledger)) AS first_ledger,
	       toUInt32(max(last_ledger)) AS last_ledger,
	       min(first_at) AS first_at,
	       max(last_at) AS last_at
	FROM stellar.account_sponsor_edges
	WHERE sponsor = ?`

// accountGraphRevocationsQuery reads the revocation count off the sponsor board, which the
// same cycle EXCHANGEs with the edges, so it cannot describe a different cycle than the graph.
const accountGraphRevocationsQuery = `
	SELECT toUInt64(sum(revocations_issued))
	FROM stellar.account_sponsors_rollup WHERE sponsor = ?`

// accountGraphCoverageQuery reads both arms' data-derived spans from the stats tables their
// own cycles wrote (ADR-0031); separate per arm because the floors differ (genesis vs protocol 14).
const accountGraphCoverageQuery = `
	SELECT '` + GraphRelationCreated + `' AS arm, metric, value, computed_at
	FROM stellar.account_creators_stats
	WHERE metric IN ('from_ledger', 'thru_ledger', 'from_time', 'thru_time')
	UNION ALL
	SELECT '` + GraphRelationSponsored + `' AS arm, metric, value, computed_at
	FROM stellar.account_sponsors_stats
	WHERE metric IN ('from_ledger', 'thru_ledger', 'from_time', 'thru_time')`

// The outbound page reads are keyset-paged on the counterparty id (second ORDER BY column), so
// a page is a primary-key range bounded by LIMIT. An empty cursor sorts below every strkey,
// so the first page needs no separate statement.
const (
	accountGraphCreatedPageQuery = `
	SELECT created, creations, funded_stroops, first_ledger, last_ledger, first_at, last_at
	FROM stellar.account_creator_edges
	WHERE creator = ? AND created > ?
	ORDER BY created
	LIMIT ?`

	accountGraphSponsoredPageQuery = `
	SELECT sponsored, sponsorships_started, first_ledger, last_ledger, first_at, last_at
	FROM stellar.account_sponsor_edges
	WHERE sponsor = ? AND sponsored > ?
	ORDER BY sponsored
	LIMIT ?`
)

// AccountGraph reads one account's neighbourhood. relation selects the paged OUTBOUND
// direction (GraphRelationCreated, GraphRelationSponsored, or "" for none); inbound edges and
// both outbound summaries are always returned. ok=false (not an error) when either arm has
// not been exchanged live: a half-provisioned graph would claim "created by nobody".
func (r *ExplorerReader) AccountGraph(ctx context.Context, account, relation string, limit int, cursor string) (AccountGraph, bool, error) {
	if !r.probeSchema(ctx, &r.accountCreatorEdgesProbe,
		`SELECT creator FROM stellar.account_creator_edges LIMIT 1`, true) {
		return AccountGraph{}, false, nil
	}
	if !r.probeSchema(ctx, &r.accountSponsorEdgesProbe,
		`SELECT sponsor FROM stellar.account_sponsor_edges LIMIT 1`, true) {
		return AccountGraph{}, false, nil
	}

	var out AccountGraph
	if err := r.readAccountGraphInbound(ctx, &out, account); err != nil {
		return AccountGraph{}, false, err
	}
	if err := r.readAccountGraphOutbound(ctx, &out, account); err != nil {
		return AccountGraph{}, false, err
	}
	if err := r.readAccountGraphRevocations(ctx, &out, account); err != nil {
		return AccountGraph{}, false, err
	}
	if err := r.readAccountGraphCoverage(ctx, &out); err != nil {
		return AccountGraph{}, false, err
	}
	if relation != "" {
		if err := r.readAccountGraphPage(ctx, &out, account, relation, limit, cursor); err != nil {
			return AccountGraph{}, false, err
		}
	}
	return out, true, nil
}

func (r *ExplorerReader) readAccountGraphInbound(ctx context.Context, out *AccountGraph, account string) error {
	rows, err := r.conn.Query(ctx, accountGraphInboundQuery,
		account, AccountGraphInboundCap, account, AccountGraphInboundCap)
	if err != nil {
		return fmt.Errorf("clickhouse: account graph inbound: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			rel    string
			edge   AccountGraphEdge
			funded *big.Int
			total  uint64
		)
		if err := rows.Scan(&rel, &edge.Account, &edge.Events, &funded,
			&edge.FirstLedger, &edge.LastLedger, &edge.FirstAt, &edge.LastAt, &total); err != nil {
			return fmt.Errorf("clickhouse: scan account graph inbound edge: %w", err)
		}
		switch rel {
		case GraphRelationCreated:
			edge.FundedStroops = funded
			out.CreatedBy = append(out.CreatedBy, edge)
			out.CreatedByTotal = total
		case GraphRelationSponsored:
			// Sponsorship moves no balance; nil rather than a zero that reads like a fact.
			out.SponsoredBy = append(out.SponsoredBy, edge)
			out.SponsoredByTotal = total
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// UNION ALL does not keep the arms' inner ordering, so sort here on the inner key to make
	// a truncated slice deterministic.
	sortGraphEdges(out.CreatedBy)
	sortGraphEdges(out.SponsoredBy)
	return nil
}

func sortGraphEdges(edges []AccountGraphEdge) {
	sort.Slice(edges, func(i, j int) bool { return edges[i].Account < edges[j].Account })
}

func (r *ExplorerReader) readAccountGraphOutbound(ctx context.Context, out *AccountGraph, account string) error {
	rows, err := r.conn.Query(ctx, accountGraphOutboundQuery, account, account)
	if err != nil {
		return fmt.Errorf("clickhouse: account graph outbound: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			rel    string
			side   AccountGraphSide
			funded *big.Int
		)
		if err := rows.Scan(&rel, &side.Accounts, &side.Events, &funded,
			&side.FirstLedger, &side.LastLedger, &side.FirstAt, &side.LastAt); err != nil {
			return fmt.Errorf("clickhouse: scan account graph outbound side: %w", err)
		}
		switch rel {
		case GraphRelationCreated:
			side.FundedStroops = funded
			out.Created = side
		case GraphRelationSponsored:
			out.Sponsored = side
		}
	}
	return rows.Err()
}

func (r *ExplorerReader) readAccountGraphRevocations(ctx context.Context, out *AccountGraph, account string) error {
	rows, err := r.conn.Query(ctx, accountGraphRevocationsQuery, account)
	if err != nil {
		return fmt.Errorf("clickhouse: account graph revocations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := rows.Scan(&out.RevocationsIssued); err != nil {
			return fmt.Errorf("clickhouse: scan account graph revocations: %w", err)
		}
	}
	return rows.Err()
}

func (r *ExplorerReader) readAccountGraphCoverage(ctx context.Context, out *AccountGraph) error {
	rows, err := r.conn.Query(ctx, accountGraphCoverageQuery)
	if err != nil {
		return fmt.Errorf("clickhouse: account graph coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			arm, metric string
			value       int64
			computedAt  time.Time
		)
		if err := rows.Scan(&arm, &metric, &value, &computedAt); err != nil {
			return fmt.Errorf("clickhouse: scan account graph coverage: %w", err)
		}
		cov := &out.CreationCoverage
		if arm == GraphRelationSponsored {
			cov = &out.SponsorshipCoverage
		}
		cov.ComputedAt = computedAt
		switch metric {
		case "from_ledger":
			cov.FromLedger = clampLedger(value)
		case "thru_ledger":
			cov.ThruLedger = clampLedger(value)
		case "from_time":
			cov.FromTime = time.Unix(value, 0).UTC()
		case "thru_time":
			cov.ThruTime = time.Unix(value, 0).UTC()
		}
	}
	return rows.Err()
}

func (r *ExplorerReader) readAccountGraphPage(ctx context.Context, out *AccountGraph, account, relation string, limit int, cursor string) error {
	query := accountGraphSponsoredPageQuery
	if relation == GraphRelationCreated {
		query = accountGraphCreatedPageQuery
	}
	rows, err := r.conn.Query(ctx, query, account, cursor, limit)
	if err != nil {
		return fmt.Errorf("clickhouse: account graph page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var edge AccountGraphEdge
		if relation == GraphRelationCreated {
			var funded *big.Int
			if err := rows.Scan(&edge.Account, &edge.Events, &funded,
				&edge.FirstLedger, &edge.LastLedger, &edge.FirstAt, &edge.LastAt); err != nil {
				return fmt.Errorf("clickhouse: scan account graph created edge: %w", err)
			}
			edge.FundedStroops = funded
		} else if err := rows.Scan(&edge.Account, &edge.Events,
			&edge.FirstLedger, &edge.LastLedger, &edge.FirstAt, &edge.LastAt); err != nil {
			return fmt.Errorf("clickhouse: scan account graph sponsored edge: %w", err)
		}
		out.Page = append(out.Page, edge)
	}
	return rows.Err()
}
