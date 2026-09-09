package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"
)

// The two relationships the account graph models (#351). They are
// deliberately never summed: a creation is immutable and happens once
// per (creator, created) pair per lifetime of the created address, while
// a sponsorship is revocable and repeatable, and one account can
// legitimately sit high in both.
const (
	GraphRelationCreated   = "created"
	GraphRelationSponsored = "sponsored"
)

// AccountGraphInboundCap bounds how many INBOUND edges one direction may
// return. Inbound is small by nature and this is a guard rail, not a
// page: measured on r1 2026-09-09 over lake partition 63, no created
// account has more than 9 distinct creators (p99.99 = 4.4) and no
// sponsored account more than 8 distinct sponsors (p99.9 = 7). The cap
// exists so a future protocol or a pathological address cannot turn a
// bounded response into an unbounded one, and the served total is the
// EXACT count either way, so truncation is visible rather than silent.
const AccountGraphInboundCap = 25

// AccountGraphEdge is one edge of the graph: a counterparty plus the
// weight of the relationship with it.
//
// ONE ROW PER DISTINCT PAIR, not per operation. Events is how many
// operations sit behind the edge — creations for a creation edge,
// sponsorship arrangements STARTED for a sponsorship edge.
type AccountGraphEdge struct {
	// Account is the other end: the creator/sponsor on an inbound edge,
	// the created/sponsored account on an outbound one.
	Account string
	Events  uint64
	// FundedStroops is the starting balance summed over the creations
	// behind this edge, and is set on CREATION edges only (nil on
	// sponsorship edges, which move no balance). Zero is a real, common
	// value: a CAP-33 sponsored creation pays no reserve of its own.
	FundedStroops *big.Int
	FirstLedger   uint32
	LastLedger    uint32
	FirstAt       time.Time
	LastAt        time.Time
}

// AccountGraphSide is the whole-history summary of ONE outbound
// direction — every account this one created, or every account it
// sponsored — independent of any page the caller asked for.
//
// Accounts is the number of distinct counterparties (edges); Events is
// the number of operations across them. The two differ sharply for a
// recycling creator or a re-sponsoring sponsor, and collapsing them
// would misreport both.
type AccountGraphSide struct {
	Accounts      uint64
	Events        uint64
	FundedStroops *big.Int
	FirstLedger   uint32
	LastLedger    uint32
	FirstAt       time.Time
	LastAt        time.Time
}

// AccountGraphCoverage is the span ONE arm of the graph aggregated, read
// off the same cycle that built that arm's edges. The two arms come from
// two independent rollup cycles over two different sources, so they are
// carried separately rather than merged into a single claim: the
// creation arm reaches back to genesis, the sponsorship arm only to
// protocol 14's activation, where sponsorship began to exist.
type AccountGraphCoverage struct {
	FromLedger uint32
	ThruLedger uint32
	FromTime   time.Time
	ThruTime   time.Time
	ComputedAt time.Time
}

// AccountGraph is one account's neighbourhood in the sponsorship and
// account-creation graph (#351).
//
// EVERYTHING HERE IS HISTORY. A creation edge is immutable — a creation
// never un-happens. A sponsorship edge says an arrangement was STARTED,
// never that one is in force: RevokeSponsorship names the entry it
// revokes inside body_xdr, which the rollup does not decode, so a
// revocation is attributable to the account that ISSUED it and to no
// individual edge; and an arrangement also lapses when the sponsored
// entry is deleted or the account merges away, neither of which emits an
// operation at all. RevocationsIssued is carried for that reason — as
// the account-level fact it is — and no field on any edge claims a live
// sponsorship.
//
// The two directions are shaped by their measured cardinality. Inbound
// (CreatedBy / SponsoredBy) is bounded by AccountGraphInboundCap with an
// exact total beside it. Outbound is summarised (Created / Sponsored)
// and paged (Page), because the busiest sponsor on the network covers
// 785,543 distinct accounts and the busiest creator 193,015.
type AccountGraph struct {
	// CreatedBy is normally one edge, and is a LIST rather than a single
	// value because it genuinely can be several: an address can be
	// created, merged away and created again, by the same funder or a
	// different one. Measured on r1 2026-09-09, 41,358 of partition 63's
	// 418,016 created addresses carry more than one creation and the
	// widest carries 29,634 — which is why the edges are collapsed to
	// distinct pairs before they are served.
	CreatedBy      []AccountGraphEdge
	CreatedByTotal uint64
	// SponsoredBy is every account that has ever begun a sponsorship
	// arrangement covering this one. Not "is sponsoring it now".
	SponsoredBy      []AccountGraphEdge
	SponsoredByTotal uint64

	Created   AccountGraphSide
	Sponsored AccountGraphSide
	// RevocationsIssued counts RevokeSponsorship operations this account
	// was the source of. It is an ACCOUNT-level fact, not an edge-level
	// one, and it is a lower bound on arrangements that ended.
	RevocationsIssued uint64

	CreationCoverage    AccountGraphCoverage
	SponsorshipCoverage AccountGraphCoverage

	// Page is the requested outbound direction's slice, keyset-ordered by
	// the counterparty's account id. Empty when no relation was asked
	// for.
	Page []AccountGraphEdge
}

// accountGraphInboundQuery reads both inbound directions in one
// round-trip. `count() OVER ()` is evaluated before the LIMIT, so each
// arm carries the EXACT number of inbound edges alongside the capped
// slice — truncation is then observable rather than inferred from a full
// page. Both reads are primary-key range reads: the tables are ORDER BY
// (created, creator) and (sponsored, sponsor).
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

// accountGraphOutboundQuery summarises both outbound directions in one
// round-trip. Each arm is an aggregate over a primary-key range, so its
// cost is that account's own edges and not the table: measured on r1
// 2026-09-09, the same shape over the busiest creator's 1,569,693 rows
// in stellar.account_creators_ops — an upper bound, since that address
// collapses to 193,015 edges — cost 63 ms at max_threads=2.
//
// An account with no edges in one direction yields that arm's row with
// accounts = 0; callers must read that as "no relationship", not as a
// span starting at ledger 0.
const accountGraphOutboundQuery = `
	SELECT '` + GraphRelationCreated + `' AS rel,
	       toUInt64(count()) AS accounts,
	       toUInt64(sum(creations)) AS events,
	       toInt128(sum(funded_stroops)) AS funded,
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

// accountGraphRevocationsQuery reads the account-level revocation count
// off the sponsor board, which the same cycle EXCHANGEs with the edges,
// so the number cannot describe a different cycle from the graph beside
// it. A scan of the board's one row per distinct sponsor — 2,423 rows on
// r1 2026-09-09, measured at 2 ms.
const accountGraphRevocationsQuery = `
	SELECT toUInt64(sum(revocations_issued))
	FROM stellar.account_sponsors_rollup WHERE sponsor = ?`

// accountGraphCoverageQuery reads both arms' data-derived spans from the
// stats tables their own cycles wrote (ADR-0031). Kept separate per arm
// because they are separate facts: the creation arm's floor is genesis,
// the sponsorship arm's is protocol 14.
const accountGraphCoverageQuery = `
	SELECT '` + GraphRelationCreated + `' AS arm, metric, value, computed_at
	FROM stellar.account_creators_stats
	WHERE metric IN ('from_ledger', 'thru_ledger', 'from_time', 'thru_time')
	UNION ALL
	SELECT '` + GraphRelationSponsored + `' AS arm, metric, value, computed_at
	FROM stellar.account_sponsors_stats
	WHERE metric IN ('from_ledger', 'thru_ledger', 'from_time', 'thru_time')`

// The two outbound page reads. Both are keyset-paged on the
// counterparty's account id, which is the second column of the table's
// ORDER BY, so a page is a primary-key range read bounded by LIMIT
// however many edges the account has. An empty cursor compares as `>
// ”`, which every strkey satisfies, so the first page needs no separate
// statement.
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

// AccountGraph reads one account's neighbourhood in both relationships.
//
// relation selects which OUTBOUND direction is paged: GraphRelationCreated,
// GraphRelationSponsored, or "" for none. The inbound edges and both
// outbound SUMMARIES are always returned — they are bounded by
// construction, so the expensive direction is the one a caller has to
// ask for explicitly.
//
// ok=false (not an error) when either graph arm has not been exchanged
// live yet: a half-provisioned graph would answer "this account was
// created by nobody", which is a claim, not an absence.
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
			// Sponsorship moves no balance, so the column stays nil
			// rather than being served as a zero that reads like a fact.
			out.SponsoredBy = append(out.SponsoredBy, edge)
			out.SponsoredByTotal = total
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// UNION ALL does not promise the arms' inner ordering survives, and a
	// truncated slice must still be a deterministic one, so both are
	// sorted here on the same key the inner statements ordered by.
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
