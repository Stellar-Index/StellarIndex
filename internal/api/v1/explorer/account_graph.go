package explorer

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// AccountGraphView is the wire response for GET
// /v1/accounts/{g_strkey}/graph — one account's neighbourhood in the two
// relationships #351 asks for, traversable in both directions.
//
// EVERY FIGURE HERE IS HISTORY, and the two relationships are kept apart
// because they are not the same kind of fact:
//
//   - CREATION is immutable. A CreateAccount happened or it did not, and
//     it never un-happens. The only subtlety is that an address can be
//     created more than once — created, merged away, created again — so
//     `created_by` is a LIST, not a single value.
//
//   - SPONSORSHIP is revocable, and this surface cannot see the live
//     set. Every sponsorship figure counts arrangements STARTED. It is
//     NOT a count of arrangements in force, and no field here may be
//     read as one: a revocation names the entry it revokes inside XDR
//     the rollup does not decode, so it is attributable to the account
//     that ISSUED it and to no individual edge, and an arrangement also
//     lapses silently when the sponsored entry is deleted or the account
//     merges away. `revocations_issued` is therefore carried at account
//     level, and there is deliberately no per-edge "current" flag.
//
// Stroops-denominated values are decimal STRINGS (ADR-0003); counts are
// JSON numbers.
type AccountGraphView struct {
	Account string `json:"account"`
	// Inbound is where this account came from. Bounded by construction:
	// each side returns at most AccountGraphInboundCap edges with the
	// EXACT total beside it, so truncation is visible rather than
	// inferred.
	Inbound struct {
		CreatedBy   AccountGraphInboundV `json:"created_by"`
		SponsoredBy AccountGraphInboundV `json:"sponsored_by"`
	} `json:"inbound"`
	// Outbound is what this account has done to others, as whole-history
	// summaries. The edge LISTS behind these are paged separately
	// (?relation=), because the busiest sponsor on the network covers
	// 785,543 distinct accounts — a number no response and no rendering
	// may attempt to hold.
	Outbound struct {
		Created   AccountGraphCreatedSideV   `json:"created"`
		Sponsored AccountGraphSponsoredSideV `json:"sponsored"`
	} `json:"outbound"`
	// Relation echoes which outbound direction was paged ("created" or
	// "sponsored"); absent when none was requested, in which case `edges`
	// is absent too.
	Relation string `json:"relation,omitempty"`
	// Edges is the requested outbound direction's page, ordered by the
	// counterparty's account id. Non-nil whenever Relation is set — an
	// account with no edges serves an empty array, not null.
	Edges      []AccountGraphEdgeV `json:"edges,omitempty"`
	NextCursor string              `json:"next_cursor,omitempty"`
	// Coverage carries the two arms' spans separately because they ARE
	// separate: creation history reaches genesis, sponsorship history
	// only reaches protocol 14's activation, where the feature began to
	// exist. Merging them would present the sponsorship floor as a gap.
	Coverage struct {
		Creation    AccountGraphCoverageV `json:"creation"`
		Sponsorship AccountGraphCoverageV `json:"sponsorship"`
	} `json:"coverage"`
	Note string `json:"note"`
}

// AccountGraphInboundV is one inbound direction: the capped edge slice
// plus the exact number of edges that exist.
type AccountGraphInboundV struct {
	Edges []AccountGraphEdgeV `json:"edges"`
	Total uint64              `json:"total"`
	// Truncated is true when Total exceeds the cap, so a consumer never
	// has to compare lengths to find out whether it saw everything.
	Truncated bool `json:"truncated"`
}

// AccountGraphEdgeV is one edge — a counterparty and the weight of the
// relationship. One row per distinct pair, never per operation.
type AccountGraphEdgeV struct {
	Account string `json:"account"`
	// Creations is set on creation edges: how many CreateAccount
	// operations sit behind this pair. Above 1 means the address was
	// created, merged away and created again by the same funder.
	Creations uint64 `json:"creations,omitempty"`
	// FundedStroops is the starting balance summed over those creations,
	// as a decimal string. "0" is a real value — a CAP-33 sponsored
	// creation pays no reserve of its own — so the field is present on
	// every creation edge and absent on sponsorship edges, which move no
	// balance.
	FundedStroops string `json:"funded_stroops,omitempty"`
	// SponsorshipsStarted is set on sponsorship edges: arrangements this
	// pair BEGAN. Never a count of arrangements still in force.
	SponsorshipsStarted uint64 `json:"sponsorships_started,omitempty"`
	FirstLedger         uint32 `json:"first_ledger"`
	LastLedger          uint32 `json:"last_ledger"`
	FirstAt             string `json:"first_at"`
	LastAt              string `json:"last_at"`
}

// AccountGraphCreatedSideV summarises every account this one created.
type AccountGraphCreatedSideV struct {
	// Accounts is distinct addresses created; Creations is CreateAccount
	// operations. They diverge for a funder that recycles addresses, and
	// collapsing them would misreport both.
	Accounts      uint64 `json:"accounts"`
	Creations     uint64 `json:"creations"`
	FundedStroops string `json:"funded_stroops"`
	FirstLedger   uint32 `json:"first_ledger,omitempty"`
	LastLedger    uint32 `json:"last_ledger,omitempty"`
	FirstAt       string `json:"first_at,omitempty"`
	LastAt        string `json:"last_at,omitempty"`
}

// AccountGraphSponsoredSideV summarises every account this one has
// sponsored. Nothing here is a live-state figure.
type AccountGraphSponsoredSideV struct {
	Accounts            uint64 `json:"accounts"`
	SponsorshipsStarted uint64 `json:"sponsorships_started"`
	// RevocationsIssued counts RevokeSponsorship operations this account
	// was the source of. It is an account-level fact and CANNOT be
	// attributed to any edge above: the revoked entry is named inside
	// body_xdr, which the rollup does not decode. It is also a lower
	// bound on arrangements that ended, because an entry stops being
	// sponsored when it is deleted too, and that emits no operation.
	RevocationsIssued uint64 `json:"revocations_issued"`
	FirstLedger       uint32 `json:"first_ledger,omitempty"`
	LastLedger        uint32 `json:"last_ledger,omitempty"`
	FirstAt           string `json:"first_at,omitempty"`
	LastAt            string `json:"last_at,omitempty"`
}

// AccountGraphCoverageV is one arm's data-derived span (ADR-0031) and
// the time the cycle behind it ran.
type AccountGraphCoverageV struct {
	FromLedger uint32 `json:"from_ledger"`
	ThruLedger uint32 `json:"thru_ledger"`
	FromTime   string `json:"from_time"`
	ThruTime   string `json:"thru_time"`
	ComputedAt string `json:"computed_at"`
}

const (
	// graphDefaultLimit is one screen of outbound edges.
	graphDefaultLimit = 50
	// graphMaxLimit bounds ONE page, not the traversal. The outbound
	// direction is genuinely unbounded — 785,543 distinct sponsored
	// accounts for the busiest sponsor measured on r1 2026-09-09 — so a
	// caller that wants the whole set walks the cursor. Matched to the
	// sibling league-table endpoints' cap.
	graphMaxLimit = 500
)

// accountGraphNote is served on every response. The endpoint's whole
// risk is being read as live state, so the disclaimer is part of the
// payload rather than only of the documentation.
const accountGraphNote = "History, not live state. Creation edges are immutable. " +
	"Sponsorship edges count arrangements STARTED and are never a count of " +
	"sponsorships in force: a revocation names its target inside XDR this " +
	"index does not decode, so revocations_issued is an account-level figure " +
	"that cannot be attributed to an edge, and an arrangement also ends " +
	"silently when the sponsored entry is deleted or the account merges away."

// parseGraphRelation reads the optional ?relation= selector. Empty means
// "no outbound page", which is the bounded default: the caller has to
// name the direction whose edge list can run to hundreds of thousands of
// rows.
func (h *Handler) parseGraphRelation(w http.ResponseWriter, r *http.Request) (string, bool) {
	rel := r.URL.Query().Get("relation")
	switch rel {
	case "", clickhouse.GraphRelationCreated, clickhouse.GraphRelationSponsored:
		return rel, true
	}
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-relation",
		"Invalid relation", http.StatusBadRequest,
		`relation must be "created" or "sponsored", or be omitted for summaries only`)
	return "", false
}

// parseGraphCursor reads the opaque ?cursor=. The cursor IS the last
// served counterparty's account id — the second column of both edge
// tables' ORDER BY — so it is validated as a G-strkey rather than
// accepted as free text: a caller echoing back a mangled value gets a
// 400 instead of a silently shifted page.
func (h *Handler) parseGraphCursor(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return "", true
	}
	if !canonical.IsAccountID(raw) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be an opaque value returned in a prior next_cursor")
		return "", false
	}
	return raw, true
}

// AccountGraph serves GET /v1/accounts/{g_strkey}/graph — who created
// and sponsored this account, and whom it created and sponsored (#351).
//
// The default response is BOUNDED BY CONSTRUCTION: inbound edges are
// capped, outbound is summarised, and the unbounded direction is served
// only when a caller names it with ?relation= and is keyset-paged from
// there. That asymmetry is the endpoint's whole shape, and it follows
// the measured data — inbound tops out in single digits, outbound at
// three quarters of a million.
func (h *Handler) AccountGraph(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	relation, ok := h.parseGraphRelation(w, r)
	if !ok {
		return
	}
	limit, ok := h.ParseLimit(w, r, graphDefaultLimit, graphMaxLimit)
	if !ok {
		return
	}
	cursor, ok := h.parseGraphCursor(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	graph, ok, err := h.Reader.AccountGraph(ctx, g, relation, limit, cursor)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-graph-timeout",
				"Account graph timed out")
			return
		}
		h.Logger.Error("explorer AccountGraph failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !ok {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/account-graph-warming",
			"Account graph warming", http.StatusServiceUnavailable,
			"the sponsorship/creation graph hasn't completed its first cycle on this "+
				"deployment yet; retry shortly")
		return
	}

	h.WriteJSON(w, accountGraphView(g, relation, limit, graph), false)
}

// accountGraphView renders the reader snapshot onto the wire contract.
// Split out so the shaping — which is where the honesty rules live — is
// testable without an HTTP round-trip.
func accountGraphView(account, relation string, limit int, g clickhouse.AccountGraph) AccountGraphView {
	out := AccountGraphView{Account: account, Note: accountGraphNote}

	out.Inbound.CreatedBy = inboundView(g.CreatedBy, g.CreatedByTotal, true)
	out.Inbound.SponsoredBy = inboundView(g.SponsoredBy, g.SponsoredByTotal, false)

	out.Outbound.Created = AccountGraphCreatedSideV{
		Accounts:      g.Created.Accounts,
		Creations:     g.Created.Events,
		FundedStroops: stroopsString(g.Created.FundedStroops),
	}
	// A span is served only when there is something to span. An account
	// that created nothing has no first/last ledger, and emitting the
	// zero value would read as "created something at ledger 0".
	if g.Created.Accounts > 0 {
		out.Outbound.Created.FirstLedger = g.Created.FirstLedger
		out.Outbound.Created.LastLedger = g.Created.LastLedger
		out.Outbound.Created.FirstAt = g.Created.FirstAt.UTC().Format(time.RFC3339)
		out.Outbound.Created.LastAt = g.Created.LastAt.UTC().Format(time.RFC3339)
	}
	out.Outbound.Sponsored = AccountGraphSponsoredSideV{
		Accounts:            g.Sponsored.Accounts,
		SponsorshipsStarted: g.Sponsored.Events,
		RevocationsIssued:   g.RevocationsIssued,
	}
	if g.Sponsored.Accounts > 0 {
		out.Outbound.Sponsored.FirstLedger = g.Sponsored.FirstLedger
		out.Outbound.Sponsored.LastLedger = g.Sponsored.LastLedger
		out.Outbound.Sponsored.FirstAt = g.Sponsored.FirstAt.UTC().Format(time.RFC3339)
		out.Outbound.Sponsored.LastAt = g.Sponsored.LastAt.UTC().Format(time.RFC3339)
	}

	out.Coverage.Creation = coverageView(g.CreationCoverage)
	out.Coverage.Sponsorship = coverageView(g.SponsorshipCoverage)

	if relation == "" {
		return out
	}
	out.Relation = relation
	creation := relation == clickhouse.GraphRelationCreated
	// Never `null` on the wire: a relation was asked for, so the field is
	// a list — possibly an empty one.
	out.Edges = make([]AccountGraphEdgeV, 0, len(g.Page))
	for _, e := range g.Page {
		out.Edges = append(out.Edges, edgeView(e, creation))
	}
	// Only a FULL page can have more behind it. Emitting a cursor on a
	// short page costs the caller one empty round-trip and, worse, makes
	// "there is more" indistinguishable from "that was everything".
	if len(out.Edges) == limit && limit > 0 {
		out.NextCursor = out.Edges[len(out.Edges)-1].Account
	}
	return out
}

func inboundView(edges []clickhouse.AccountGraphEdge, total uint64, creation bool) AccountGraphInboundV {
	v := AccountGraphInboundV{
		Edges:     make([]AccountGraphEdgeV, 0, len(edges)),
		Total:     total,
		Truncated: total > uint64(len(edges)),
	}
	for _, e := range edges {
		v.Edges = append(v.Edges, edgeView(e, creation))
	}
	return v
}

func edgeView(e clickhouse.AccountGraphEdge, creation bool) AccountGraphEdgeV {
	v := AccountGraphEdgeV{
		Account:     e.Account,
		FirstLedger: e.FirstLedger,
		LastLedger:  e.LastLedger,
		FirstAt:     e.FirstAt.UTC().Format(time.RFC3339),
		LastAt:      e.LastAt.UTC().Format(time.RFC3339),
	}
	if creation {
		v.Creations = e.Events
		v.FundedStroops = stroopsString(e.FundedStroops)
		return v
	}
	v.SponsorshipsStarted = e.Events
	return v
}

func coverageView(c clickhouse.AccountGraphCoverage) AccountGraphCoverageV {
	return AccountGraphCoverageV{
		FromLedger: c.FromLedger,
		ThruLedger: c.ThruLedger,
		FromTime:   c.FromTime.UTC().Format(time.RFC3339),
		ThruTime:   c.ThruTime.UTC().Format(time.RFC3339),
		ComputedAt: c.ComputedAt.UTC().Format(time.RFC3339),
	}
}
