package v1_test

import (
	"encoding/binary"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// graphSubject is the account whose neighbourhood every case below reads.
const graphSubject = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

type accountGraphEdgeEnv struct {
	Account             string `json:"account"`
	Creations           uint64 `json:"creations"`
	FundedStroops       string `json:"funded_stroops"`
	SponsorshipsStarted uint64 `json:"sponsorships_started"`
	FirstLedger         uint32 `json:"first_ledger"`
	LastLedger          uint32 `json:"last_ledger"`
	FirstAt             string `json:"first_at"`
	LastAt              string `json:"last_at"`
}

type accountGraphInboundEnv struct {
	Edges     []accountGraphEdgeEnv `json:"edges"`
	Total     uint64                `json:"total"`
	Truncated bool                  `json:"truncated"`
}

type accountGraphCoverageEnv struct {
	FromLedger uint32 `json:"from_ledger"`
	ThruLedger uint32 `json:"thru_ledger"`
	FromTime   string `json:"from_time"`
	ThruTime   string `json:"thru_time"`
	ComputedAt string `json:"computed_at"`
}

type accountGraphEnvelope struct {
	Data struct {
		Account string `json:"account"`
		Inbound struct {
			CreatedBy   accountGraphInboundEnv `json:"created_by"`
			SponsoredBy accountGraphInboundEnv `json:"sponsored_by"`
		} `json:"inbound"`
		Outbound struct {
			Created struct {
				Accounts      uint64 `json:"accounts"`
				Creations     uint64 `json:"creations"`
				FundedStroops string `json:"funded_stroops"`
				FirstLedger   uint32 `json:"first_ledger"`
				LastLedger    uint32 `json:"last_ledger"`
				FirstAt       string `json:"first_at"`
				LastAt        string `json:"last_at"`
			} `json:"created"`
			Sponsored struct {
				Accounts            uint64 `json:"accounts"`
				SponsorshipsStarted uint64 `json:"sponsorships_started"`
				RevocationsIssued   uint64 `json:"revocations_issued"`
				FirstLedger         uint32 `json:"first_ledger"`
				LastLedger          uint32 `json:"last_ledger"`
			} `json:"sponsored"`
		} `json:"outbound"`
		Relation   string                `json:"relation"`
		Edges      []accountGraphEdgeEnv `json:"edges"`
		NextCursor string                `json:"next_cursor"`
		Coverage   struct {
			Creation    accountGraphCoverageEnv `json:"creation"`
			Sponsorship accountGraphCoverageEnv `json:"sponsorship"`
		} `json:"coverage"`
		Note string `json:"note"`
	} `json:"data"`
}

// genesisAdjacentLedger is where account creation actually starts on the
// chain — the first CreateAccount is at ledger 3, not 1. Using the real
// value keeps the coverage assertions pinned to a chain fact.
const genesisAdjacentLedger = 3

func graphTime(day int) time.Time {
	return time.Date(2026, 3, day, 12, 0, 0, 0, time.UTC)
}

// graphSnapshot is the base fixture: an address that was created twice
// by one funder (recycled), sponsored by two accounts, and which itself
// created and sponsored others.
func graphSnapshot() clickhouse.AccountGraph {
	return clickhouse.AccountGraph{
		CreatedBy: []clickhouse.AccountGraphEdge{{
			Account:       "GAUA7XL5K54CC2DDGP77FJ2YBHRJLT36CPZDXWPM6MP7MANOGG77PNJU",
			Events:        2,
			FundedStroops: big.NewInt(30000000),
			FirstLedger:   52651627, LastLedger: 59446198,
			FirstAt: graphTime(1), LastAt: graphTime(2),
		}},
		CreatedByTotal: 1,
		SponsoredBy: []clickhouse.AccountGraphEdge{
			{
				Account:     "GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU",
				Events:      4,
				FirstLedger: 40000000, LastLedger: 41000000,
				FirstAt: graphTime(3), LastAt: graphTime(4),
			},
			{
				Account:     "GDNHPXSFIZQMJJFBAFWUWG3442AHGI3WEWUYRYXIGMJRHDUQOPHTKLDC",
				Events:      1,
				FirstLedger: 42000000, LastLedger: 42000000,
				FirstAt: graphTime(5), LastAt: graphTime(5),
			},
		},
		SponsoredByTotal: 2,
		Created: clickhouse.AccountGraphSide{
			Accounts: 3, Events: 5, FundedStroops: big.NewInt(120000000),
			FirstLedger: 50000000, LastLedger: 60000000,
			FirstAt: graphTime(6), LastAt: graphTime(7),
		},
		Sponsored: clickhouse.AccountGraphSide{
			Accounts: 4, Events: 9,
			FirstLedger: 51000000, LastLedger: 61000000,
			FirstAt: graphTime(8), LastAt: graphTime(9),
		},
		RevocationsIssued: 3,
		CreationCoverage: clickhouse.AccountGraphCoverage{
			FromLedger: genesisAdjacentLedger, ThruLedger: 64346048,
			FromTime: time.Date(2015, 9, 30, 16, 46, 0, 0, time.UTC),
			ThruTime: graphTime(10), ComputedAt: graphTime(10),
		},
		SponsorshipCoverage: clickhouse.AccountGraphCoverage{
			FromLedger: protocol14Floor, ThruLedger: 64346120,
			FromTime: time.Date(2021, 2, 16, 18, 21, 0, 0, time.UTC),
			ThruTime: graphTime(10), ComputedAt: graphTime(10),
		},
	}
}

// graphEdges builds n synthetic outbound edges, ascending by account id so a keyset walk is deterministic.
// Ids are real CRC-checked G-strkeys, since fakes would hit the 400 cursor validation on page two.
func graphEdges(t *testing.T, n int) []clickhouse.AccountGraphEdge {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		var raw [32]byte
		binary.BigEndian.PutUint32(raw[28:], uint32(i))
		id, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
		if err != nil {
			t.Fatalf("encode fixture strkey %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]clickhouse.AccountGraphEdge, 0, n)
	for i, id := range ids {
		out = append(out, clickhouse.AccountGraphEdge{
			Account:     id,
			Events:      1,
			FirstLedger: uint32(50000000 + i), LastLedger: uint32(50000000 + i),
			FirstAt: graphTime(11), LastAt: graphTime(11),
		})
	}
	return out
}

func graphBodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// Both inbound directions must reach the wire, and created_by must be a
// LIST: an address can be created, merged away and created again, so a
// single-valued "creator" field would be wrong for a recycled address.
func TestExplorer_AccountGraph_InboundIsBidirectionalAndPlural(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{accountGraph: graphSnapshot()})

	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env accountGraphEnvelope
	mustDecode(t, resp, &env)

	if env.Data.Account != graphSubject {
		t.Errorf("account = %q", env.Data.Account)
	}
	if got := len(env.Data.Inbound.CreatedBy.Edges); got != 1 {
		t.Fatalf("created_by edges = %d, want 1", got)
	}
	created := env.Data.Inbound.CreatedBy.Edges[0]
	if created.Creations != 2 {
		t.Errorf("created_by[0].creations = %d, want 2 — a recycled address is "+
			"created more than once and the edge must say so", created.Creations)
	}
	if created.FundedStroops != "30000000" {
		t.Errorf("created_by[0].funded_stroops = %q, want the decimal string 30000000",
			created.FundedStroops)
	}
	if created.SponsorshipsStarted != 0 {
		t.Errorf("a creation edge carried sponsorships_started = %d", created.SponsorshipsStarted)
	}
	if got := len(env.Data.Inbound.SponsoredBy.Edges); got != 2 {
		t.Fatalf("sponsored_by edges = %d, want 2", got)
	}
	sponsored := env.Data.Inbound.SponsoredBy.Edges[0]
	if sponsored.SponsorshipsStarted != 4 {
		t.Errorf("sponsored_by[0].sponsorships_started = %d, want 4", sponsored.SponsorshipsStarted)
	}
	if sponsored.FundedStroops != "" {
		t.Errorf("a sponsorship edge carried funded_stroops = %q — sponsorship moves "+
			"no balance, and a zero there would read as a fact", sponsored.FundedStroops)
	}
	// Both outbound summaries are always present, with distinct
	// counterparties and operations kept apart.
	if env.Data.Outbound.Created.Accounts != 3 || env.Data.Outbound.Created.Creations != 5 {
		t.Errorf("outbound.created = %+v, want 3 accounts / 5 creations", env.Data.Outbound.Created)
	}
	if env.Data.Outbound.Sponsored.Accounts != 4 || env.Data.Outbound.Sponsored.SponsorshipsStarted != 9 {
		t.Errorf("outbound.sponsored = %+v", env.Data.Outbound.Sponsored)
	}
}

// The high-cardinality direction must be paged (one sponsor can cover hundreds of thousands of accounts): the default response
// has no outbound edge list, ?limit= bounds it, a full page returns a cursor, and walking cursors terminates.
func TestExplorer_AccountGraph_PaginatesTheHighCardinalityDirection(t *testing.T) {
	reader := &stubExplorerReader{
		accountGraph:        graphSnapshot(),
		graphSponsoredEdges: graphEdges(t, 25),
	}
	base := explorerTestServer(t, reader)

	// 1. Default: summaries only. No edge list, so no unbounded read can
	//    be provoked by the plain per-account request the UI makes.
	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph")
	var env accountGraphEnvelope
	mustDecode(t, resp, &env)
	if len(env.Data.Edges) != 0 || env.Data.Relation != "" {
		t.Fatalf("the default response carried %d outbound edges (relation %q) — the "+
			"unbounded direction must be opt-in", len(env.Data.Edges), env.Data.Relation)
	}
	if env.Data.NextCursor != "" {
		t.Errorf("next_cursor = %q on a response with no edge list", env.Data.NextCursor)
	}

	// 2. Asking for the direction is bounded by ?limit=, and a FULL page
	//    hands back a cursor.
	seen := map[string]bool{}
	pages, cursor := 0, ""
	for {
		url := base + "/v1/accounts/" + graphSubject + "/graph?relation=sponsored&limit=10"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		resp := mustGet(t, url)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page %d: status = %d, want 200", pages, resp.StatusCode)
		}
		var page accountGraphEnvelope
		mustDecode(t, resp, &page)
		if page.Data.Relation != "sponsored" {
			t.Errorf("page %d: relation = %q, want sponsored", pages, page.Data.Relation)
		}
		if len(page.Data.Edges) > 10 {
			t.Fatalf("page %d returned %d edges against ?limit=10 — the page is not bounded",
				pages, len(page.Data.Edges))
		}
		for _, e := range page.Data.Edges {
			if seen[e.Account] {
				t.Errorf("edge %s served twice across pages", e.Account)
			}
			seen[e.Account] = true
		}
		pages++
		if page.Data.NextCursor == "" {
			break
		}
		if page.Data.NextCursor != page.Data.Edges[len(page.Data.Edges)-1].Account {
			t.Errorf("page %d: next_cursor %q is not the last served edge %q",
				pages, page.Data.NextCursor, page.Data.Edges[len(page.Data.Edges)-1].Account)
		}
		cursor = page.Data.NextCursor
		if pages > 10 {
			t.Fatal("the cursor walk did not terminate")
		}
	}
	if pages != 3 || len(seen) != 25 {
		t.Errorf("walk covered %d edges in %d pages, want 25 in 3", len(seen), pages)
	}
	if reader.graphLimit != 10 {
		t.Errorf("the reader was asked for limit %d, not the requested 10", reader.graphLimit)
	}

	// 3. The cap is enforced, not silently widened.
	resp = mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph?relation=sponsored&limit=501")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("?limit=501 status = %d, want 400 — an over-cap page must be refused, "+
			"not clamped in silence", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 4. A relation nobody serves is a 400, not an empty list that reads
	//    as "this account sponsored nothing".
	resp = mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph?relation=funded")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("?relation=funded status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 5. A mangled cursor is refused rather than silently restarting the
	//    walk at the beginning.
	resp = mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph?relation=sponsored&cursor=not-a-strkey")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad cursor status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// Revocations are an ACCOUNT-level count, never an edge attribute: RevokeSponsorship names its
// target inside body_xdr, which the rollup does not decode. A per-edge "current"/"revoked"/
// "active" field would be a claim the data cannot support.
func TestExplorer_AccountGraph_RevocationsAreAccountLevelNotPerEdge(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{
		accountGraph:        graphSnapshot(),
		graphSponsoredEdges: graphEdges(t, 3),
	})

	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph?relation=sponsored")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := graphBodyString(t, resp)

	if !strings.Contains(body, `"revocations_issued":3`) {
		t.Errorf("revocations_issued missing from outbound.sponsored — a revoked "+
			"sponsorship must be visible SOMEWHERE, and account level is the only "+
			"level this data supports; body = %s", body)
	}
	for _, banned := range []string{
		"currently_sponsoring", "active_sponsorships", "sponsoring_now", "live_sponsored",
		"revoked", "is_current", "still_sponsored",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("response carries %q — a revocation cannot be attributed to an "+
				"edge without decoding body_xdr, so no field may imply per-edge "+
				"current-or-revoked state", banned)
		}
	}
	// The disclaimer is part of the payload, not just the docs: this is
	// the field an integrator reads before treating an edge as live.
	if !strings.Contains(body, "History, not live state") {
		t.Error("the response carries no honesty note about history vs live state")
	}
}

// An account nobody created and nobody sponsored must RENDER as that —
// 200 with empty lists and zero totals — not 404, not 500, and not a
// span starting at ledger 0 that reads as "created at genesis".
func TestExplorer_AccountGraph_AccountWithNoSponsorOrCreator(t *testing.T) {
	snap := graphSnapshot()
	snap.CreatedBy, snap.CreatedByTotal = nil, 0
	snap.SponsoredBy, snap.SponsoredByTotal = nil, 0
	snap.Created = clickhouse.AccountGraphSide{}
	snap.Sponsored = clickhouse.AccountGraphSide{}
	snap.RevocationsIssued = 0
	base := explorerTestServer(t, &stubExplorerReader{accountGraph: snap})

	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unconnected account is an answer, not an error",
			resp.StatusCode)
	}
	body := graphBodyString(t, resp)
	// Empty arrays, never null: a client iterating the response must not
	// have to guard for it.
	if !strings.Contains(body, `"created_by":{"edges":[],"total":0`) {
		t.Errorf("created_by is not an empty list with a zero total; body = %s", body)
	}
	if !strings.Contains(body, `"sponsored_by":{"edges":[],"total":0`) {
		t.Errorf("sponsored_by is not an empty list with a zero total; body = %s", body)
	}
	// No span where there is nothing to span. The ledger arm catches a
	// zero on the inbound edges; the TIME arm is what catches it on the
	// outbound summaries, whose ledger fields are omitempty and so vanish
	// on their own while a zero time.Time still renders as year 1.
	if strings.Contains(body, `"first_ledger":0`) {
		t.Errorf("a zero ledger span reached the wire — an account that created and "+
			"sponsored nothing has no span, and 0 reads as genesis; body = %s", body)
	}
	if strings.Contains(body, "0001-01-01") {
		t.Errorf("a zero timestamp reached the wire — an account that created and "+
			"sponsored nothing has no first/last time, and year 1 is not one; body = %s", body)
	}
	// The whole-aggregation coverage still has to be there: "this account
	// has no creator" is only meaningful beside the span that was
	// searched.
	if !strings.Contains(body, `"coverage"`) {
		t.Error("an empty neighbourhood was served without the coverage that qualifies it")
	}
}

// Inbound is capped, and when the cap bites the response says so rather
// than letting a client infer completeness from a short list.
func TestExplorer_AccountGraph_InboundTruncationIsVisible(t *testing.T) {
	snap := graphSnapshot()
	snap.SponsoredByTotal = 40 // more than the cap; the reader returned 2
	base := explorerTestServer(t, &stubExplorerReader{accountGraph: snap})

	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph")
	var env accountGraphEnvelope
	mustDecode(t, resp, &env)

	if env.Data.Inbound.SponsoredBy.Total != 40 {
		t.Errorf("sponsored_by.total = %d, want the exact 40", env.Data.Inbound.SponsoredBy.Total)
	}
	if !env.Data.Inbound.SponsoredBy.Truncated {
		t.Error("sponsored_by.truncated is false while total (40) exceeds the served " +
			"edges (2) — truncation must be stated, not inferred")
	}
	if env.Data.Inbound.CreatedBy.Truncated {
		t.Error("created_by.truncated is true on a complete list")
	}
}

// The two arms are separate cycles over separate sources and their coverage must not be merged: creation reaches genesis,
// sponsorship only protocol 14, and one merged span would present that floor as a gap.
func TestExplorer_AccountGraph_CoverageArmsStaySeparate(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{accountGraph: graphSnapshot()})

	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph")
	var env accountGraphEnvelope
	mustDecode(t, resp, &env)

	if env.Data.Coverage.Creation.FromLedger != genesisAdjacentLedger {
		t.Errorf("coverage.creation.from_ledger = %d, want %d",
			env.Data.Coverage.Creation.FromLedger, genesisAdjacentLedger)
	}
	if env.Data.Coverage.Sponsorship.FromLedger != protocol14Floor {
		t.Errorf("coverage.sponsorship.from_ledger = %d, want the protocol-14 floor %d",
			env.Data.Coverage.Sponsorship.FromLedger, protocol14Floor)
	}
	if env.Data.Coverage.Creation.FromLedger == env.Data.Coverage.Sponsorship.FromLedger {
		t.Error("the two arms served one floor — they cover different history and " +
			"must not be collapsed into a single claim")
	}
	// Creation coverage reaching the tip is what says the P23 boundary is
	// NOT a horizon for this data: the creator rollup reads both sides of
	// it, so the graph is continuous across the protocol change.
	if env.Data.Coverage.Creation.ThruLedger <= clickhouse.P23BoundaryLedger {
		t.Errorf("coverage.creation.thru_ledger = %d stops at or below the Protocol 23 "+
			"boundary %d — creation history would then be truncated a year short of "+
			"the tip", env.Data.Coverage.Creation.ThruLedger, clickhouse.P23BoundaryLedger)
	}
}

// Before either cycle has exchanged a graph live, the endpoint 503s.
// Serving zeros would answer "this account was created by nobody", which
// is a claim rather than an absence.
func TestExplorer_AccountGraph_WarmingBeforeFirstCycle(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})

	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body := graphBodyString(t, resp)
	if !strings.Contains(body, "warming") {
		t.Errorf("503 body does not say the graph is warming: %s", body)
	}
}

// A non-strkey path segment is a 400, like every sibling account route.
func TestExplorer_AccountGraph_RejectsNonStrkey(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{accountGraph: graphSnapshot()})

	resp := mustGet(t, base+"/v1/accounts/not-an-account/graph")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

type graphHistoryPointEnv struct {
	Period      string `json:"period"`
	PeriodStart string `json:"period_start"`
	NewAccounts uint64 `json:"new_accounts"`
	Events      uint64 `json:"events"`
}

type graphHistoryUnplacedEnv struct {
	Reason string `json:"reason"`
	Events uint64 `json:"events"`
	Detail string `json:"detail"`
}

type graphHistorySeriesEnv struct {
	Points []graphHistoryPointEnv `json:"points"`
	Totals struct {
		Accounts       uint64 `json:"accounts"`
		Events         uint64 `json:"events"`
		EventsPlaced   uint64 `json:"events_placed"`
		EventsUnplaced uint64 `json:"events_unplaced"`
	} `json:"totals"`
	LowerBound bool                      `json:"lower_bound"`
	Unplaced   []graphHistoryUnplacedEnv `json:"unplaced"`
	Coverage   accountGraphCoverageEnv   `json:"coverage"`
}

type graphHistoryEnvelope struct {
	Data struct {
		Account     string `json:"account"`
		Granularity string `json:"granularity"`
		Series      struct {
			Created   graphHistorySeriesEnv `json:"created"`
			Sponsored graphHistorySeriesEnv `json:"sponsored"`
		} `json:"series"`
		Note string `json:"note"`
	} `json:"data"`
}

func histMonth(year int, month time.Month) time.Time {
	return time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
}

// graphHistorySnapshot is the base fixture, consistent with the reader's contract: created has 3 edges (3 new accounts) across
// 3 months, 7 creations of which 5 are placed; sponsored has 2 edges across 2 months, 2 arrangements, all placed.
// The deliberate gap is 2022 in the creation arm (active 2021-03 and 2023-07); no point may be invented for quiet months.
func graphHistorySnapshot() clickhouse.AccountGraphHistory {
	return clickhouse.AccountGraphHistory{
		Created: clickhouse.AccountGraphHistorySide{
			Points: []clickhouse.AccountGraphHistoryPoint{
				{Start: histMonth(2021, time.March), NewAccounts: 2, Events: 2},
				{Start: histMonth(2023, time.July), NewAccounts: 1, Events: 2},
				// A month in which nothing was FIRST created but a
				// repeated edge's last event landed.
				{Start: histMonth(2024, time.January), NewAccounts: 0, Events: 1},
			},
			Accounts: 3, Events: 7, EventsPlaced: 5, EventsUnplaced: 2,
			Coverage: clickhouse.AccountGraphCoverage{
				FromLedger: genesisAdjacentLedger, ThruLedger: 64346048,
				FromTime: time.Date(2015, 9, 30, 16, 46, 0, 0, time.UTC),
				ThruTime: graphTime(10), ComputedAt: graphTime(10),
			},
		},
		Sponsored: clickhouse.AccountGraphHistorySide{
			Points: []clickhouse.AccountGraphHistoryPoint{
				{Start: histMonth(2023, time.July), NewAccounts: 1, Events: 1},
				{Start: histMonth(2023, time.August), NewAccounts: 1, Events: 1},
			},
			Accounts: 2, Events: 2, EventsPlaced: 2, EventsUnplaced: 0,
			Coverage: clickhouse.AccountGraphCoverage{
				FromLedger: protocol14Floor, ThruLedger: 64346120,
				FromTime: time.Date(2021, 2, 16, 18, 21, 0, 0, time.UTC),
				ThruTime: graphTime(10), ComputedAt: graphTime(10),
			},
		},
	}
}

func getGraphHistory(t *testing.T, reader *stubExplorerReader, path string) graphHistoryEnvelope {
	t.Helper()
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env graphHistoryEnvelope
	mustDecode(t, resp, &env)
	return env
}

// The series must reproduce the reader's buckets exactly, in order, with
// the month label derived from the bucket's own instant.
func TestAccountGraphHistory_ServesTheBucketsAsGiven(t *testing.T) {
	reader := &stubExplorerReader{graphHistory: graphHistorySnapshot()}
	env := getGraphHistory(t, reader, "/v1/accounts/"+graphSubject+"/graph/history")

	if env.Data.Account != graphSubject {
		t.Errorf("account = %q, want %q", env.Data.Account, graphSubject)
	}
	if env.Data.Granularity != "1M" {
		t.Errorf("granularity = %q, want 1M", env.Data.Granularity)
	}
	want := []graphHistoryPointEnv{
		{Period: "2021-03", PeriodStart: "2021-03-01T00:00:00Z", NewAccounts: 2, Events: 2},
		{Period: "2023-07", PeriodStart: "2023-07-01T00:00:00Z", NewAccounts: 1, Events: 2},
		{Period: "2024-01", PeriodStart: "2024-01-01T00:00:00Z", NewAccounts: 0, Events: 1},
	}
	got := env.Data.Series.Created.Points
	if len(got) != len(want) {
		t.Fatalf("created points = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("created point %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Honesty rule: a period with no observation is a gap, not a zero. The fixture is silent from 2021-04 to 2023-06, so no point
// may exist for those months and no value may be carried across them.
func TestAccountGraphHistory_QuietMonthsAreGapsNotZeros(t *testing.T) {
	reader := &stubExplorerReader{graphHistory: graphHistorySnapshot()}
	env := getGraphHistory(t, reader, "/v1/accounts/"+graphSubject+"/graph/history")

	for _, p := range env.Data.Series.Created.Points {
		if p.Period > "2021-03" && p.Period < "2023-07" {
			t.Errorf("invented a point for quiet month %s (%+v)", p.Period, p)
		}
	}
	// 2022 in particular: a carried-forward series would repeat 2021-03's
	// figures across all twelve of its months.
	for _, p := range env.Data.Series.Created.Points {
		if len(p.Period) >= 4 && p.Period[:4] == "2022" {
			t.Errorf("carried a value into %s — nothing was observed there", p.Period)
		}
	}
}

// A bucket that arrives empty is dropped rather than served: a zero point
// reads as a measurement, and the coverage span is what makes absence
// readable.
func TestAccountGraphHistory_EmptyBucketIsNotServed(t *testing.T) {
	snap := graphHistorySnapshot()
	snap.Created.Points = append(snap.Created.Points,
		clickhouse.AccountGraphHistoryPoint{Start: histMonth(2025, time.May)})
	reader := &stubExplorerReader{graphHistory: snap}
	env := getGraphHistory(t, reader, "/v1/accounts/"+graphSubject+"/graph/history")

	for _, p := range env.Data.Series.Created.Points {
		if p.Period == "2025-05" {
			t.Errorf("served an empty bucket as a point: %+v", p)
		}
	}
}

// new_accounts is the COMPLETE half of the series: summed over the points
// it must equal the whole-history distinct-counterparty total. If it ever
// does not, the series is dropping edges and the totals are hiding it.
func TestAccountGraphHistory_NewAccountsSumToTheTotal(t *testing.T) {
	reader := &stubExplorerReader{graphHistory: graphHistorySnapshot()}
	env := getGraphHistory(t, reader, "/v1/accounts/"+graphSubject+"/graph/history")

	for name, s := range map[string]graphHistorySeriesEnv{
		"created":   env.Data.Series.Created,
		"sponsored": env.Data.Series.Sponsored,
	} {
		var sum uint64
		for _, p := range s.Points {
			sum += p.NewAccounts
		}
		if sum != s.Totals.Accounts {
			t.Errorf("%s: points' new_accounts = %d, totals.accounts = %d — the series lost an edge",
				name, sum, s.Totals.Accounts)
		}
	}
}

// Events the served tier cannot place in a month are declared, counted exactly and flagged as a lower bound, never smeared
// across the interval or dropped silently (same posture as /v1/rwa/assets excluded[]).
func TestAccountGraphHistory_UnplacedEventsAreDeclaredNotSmeared(t *testing.T) {
	reader := &stubExplorerReader{graphHistory: graphHistorySnapshot()}
	env := getGraphHistory(t, reader, "/v1/accounts/"+graphSubject+"/graph/history")

	created := env.Data.Series.Created
	if !created.LowerBound {
		t.Error("created series has unplaced events but is not flagged lower_bound")
	}
	if len(created.Unplaced) != 1 {
		t.Fatalf("created unplaced[] = %d entries, want 1: %+v", len(created.Unplaced), created.Unplaced)
	}
	if created.Unplaced[0].Reason != "repeat-events-not-timestamped" {
		t.Errorf("unplaced reason = %q", created.Unplaced[0].Reason)
	}
	if created.Unplaced[0].Events != 2 {
		t.Errorf("unplaced events = %d, want 2", created.Unplaced[0].Events)
	}
	if created.Unplaced[0].Detail == "" {
		t.Error("unplaced entry carries no detail — the reason must say what it refused")
	}

	// The points must account for exactly events_placed and no more: if
	// the handler had spread the 2 unplaced events over the buckets, this
	// sum would be 7.
	var placed uint64
	for _, p := range created.Points {
		placed += p.Events
	}
	if placed != created.Totals.EventsPlaced {
		t.Errorf("points' events = %d, totals.events_placed = %d — events were smeared or lost",
			placed, created.Totals.EventsPlaced)
	}
	if created.Totals.EventsPlaced+created.Totals.EventsUnplaced != created.Totals.Events {
		t.Errorf("placed %d + unplaced %d != events %d",
			created.Totals.EventsPlaced, created.Totals.EventsUnplaced, created.Totals.Events)
	}
}

// A series that CAN place every event says so: no lower_bound flag and no
// unplaced register. A permanently-set flag would be as useless as a
// never-set one.
func TestAccountGraphHistory_FullyPlacedSeriesIsNotFlagged(t *testing.T) {
	reader := &stubExplorerReader{graphHistory: graphHistorySnapshot()}
	env := getGraphHistory(t, reader, "/v1/accounts/"+graphSubject+"/graph/history")

	sponsored := env.Data.Series.Sponsored
	if sponsored.LowerBound {
		t.Error("sponsored series places every event but is flagged lower_bound")
	}
	if len(sponsored.Unplaced) != 0 {
		t.Errorf("sponsored unplaced[] = %+v, want absent", sponsored.Unplaced)
	}
}

// The two arms cover different spans (creation reaches genesis, sponsorship only protocol 14), so each series carries its own;
// merging would present sponsorship's floor as a gap in the creation record.
func TestAccountGraphHistory_ArmsCarrySeparateCoverage(t *testing.T) {
	reader := &stubExplorerReader{graphHistory: graphHistorySnapshot()}
	env := getGraphHistory(t, reader, "/v1/accounts/"+graphSubject+"/graph/history")

	if got := env.Data.Series.Created.Coverage.FromLedger; got != genesisAdjacentLedger {
		t.Errorf("creation coverage from_ledger = %d, want %d", got, genesisAdjacentLedger)
	}
	if got := env.Data.Series.Sponsored.Coverage.FromLedger; got != protocol14Floor {
		t.Errorf("sponsorship coverage from_ledger = %d, want %d", got, protocol14Floor)
	}
	if env.Data.Series.Created.Coverage.ComputedAt == "" {
		t.Error("creation coverage carries no computed_at — the series cannot be dated")
	}
}

// An account with no relationship in a direction serves an empty series,
// not null: an empty list is an answer, null is a question.
func TestAccountGraphHistory_EmptySeriesIsNotNull(t *testing.T) {
	snap := graphHistorySnapshot()
	snap.Sponsored = clickhouse.AccountGraphHistorySide{Coverage: snap.Sponsored.Coverage}
	reader := &stubExplorerReader{graphHistory: snap}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph/history")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := graphBodyString(t, resp)
	if !strings.Contains(body, `"sponsored":{"points":[]`) {
		t.Errorf("empty sponsored series is not an empty array: %s", body)
	}
}

// Before the rollup's first cycle the graph tables are empty, and a series over them would claim "never created anything".
// The route must warm-503, as /graph does.
func TestAccountGraphHistory_WarmingIs503(t *testing.T) {
	reader := &stubExplorerReader{} // no coverage span == no cycle has run
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/accounts/"+graphSubject+"/graph/history")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body := graphBodyString(t, resp)
	if !strings.Contains(body, "account-graph-history-warming") {
		t.Errorf("warming problem type missing: %s", body)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("problem response Cache-Control = %q, want no-store", got)
	}
}

// A malformed address is a 400 with problem+json, never a lake read.
func TestAccountGraphHistory_BadAddressIs400(t *testing.T) {
	reader := &stubExplorerReader{graphHistory: graphHistorySnapshot()}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/accounts/NOTASTRKEY/graph/history")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want problem+json", ct)
	}
}

// The note is the surface's reading instructions and is not optional: it
// is where "an absent month is a gap, not a zero" is stated on the wire.
func TestAccountGraphHistory_NoteStatesTheGapRule(t *testing.T) {
	reader := &stubExplorerReader{graphHistory: graphHistorySnapshot()}
	env := getGraphHistory(t, reader, "/v1/accounts/"+graphSubject+"/graph/history")
	for _, want := range []string{"NO POINT", "coverage", "lower_bound"} {
		if !strings.Contains(env.Data.Note, want) {
			t.Errorf("note does not mention %q: %s", want, env.Data.Note)
		}
	}
}

type cohortHoldingEnv struct {
	Asset    string  `json:"asset"`
	Kind     string  `json:"kind"`
	Holders  uint64  `json:"holders"`
	Balance  string  `json:"balance"`
	PriceUSD *string `json:"price_usd"`
	ValueUSD *string `json:"value_usd"`
}

type cohortAssetFlowEnv struct {
	Asset     string  `json:"asset"`
	Inflow    string  `json:"inflow"`
	Outflow   string  `json:"outflow"`
	Scaled    bool    `json:"scaled"`
	InflowUSD *string `json:"inflow_usd"`
}

type cohortFlowPointEnv struct {
	Period         string               `json:"period"`
	PeriodStart    string               `json:"period_start"`
	Movements      uint64               `json:"movements"`
	ActiveAccounts uint64               `json:"active_accounts"`
	ByAsset        []cohortAssetFlowEnv `json:"by_asset"`
}

type cohortEnvelope struct {
	Data struct {
		Account  string `json:"account"`
		Relation string `json:"relation"`
		Covered  bool   `json:"covered"`
		Cohort   *struct {
			Accounts     uint64 `json:"accounts"`
			LiveAccounts uint64 `json:"live_accounts"`
			Active30d    uint64 `json:"active_30d"`
		} `json:"cohort"`
		Holdings          []cohortHoldingEnv `json:"holdings"`
		HoldingsTruncated bool               `json:"holdings_truncated"`
		Valuation         struct {
			TotalUSD         *string `json:"total_usd"`
			PricedHoldings   int     `json:"priced_holdings"`
			UnpricedHoldings int     `json:"unpriced_holdings"`
			Basis            string  `json:"basis"`
		} `json:"valuation"`
		Flows struct {
			Granularity string               `json:"granularity"`
			Assets      []string             `json:"assets"`
			Points      []cohortFlowPointEnv `json:"points"`
		} `json:"flows"`
		Contracts []struct {
			ContractID     string `json:"contract_id"`
			Protocol       string `json:"protocol"`
			ActiveAccounts uint64 `json:"active_accounts"`
		} `json:"contracts"`
		Positions []struct {
			Protocol string `json:"protocol"`
			Venue    string `json:"venue"`
			Holders  uint64 `json:"holders"`
			Amount   string `json:"amount"`
		} `json:"positions"`
		Cycle *struct {
			ComputedAt string `json:"computed_at"`
			TipLedger  uint32 `json:"tip_ledger"`
		} `json:"cycle"`
		Note string `json:"note"`
	} `json:"data"`
}

// cohortPositionAmountStr is an 18-decimal-scale fold total: past 2^53, so
// it survives the wire only if nothing on the way is a float.
const cohortPositionAmountStr = "8760000000000000000000000001"

func cohortPositionAmount() *big.Int {
	v, _ := new(big.Int).SetString(cohortPositionAmountStr, 10)
	return v
}

const cohortUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func cohortSnapshot() clickhouse.AccountCohort {
	at := time.Date(2026, 9, 17, 6, 0, 0, 0, time.UTC)
	return clickhouse.AccountCohort{
		Root: graphSubject, Covered: true,
		Cycle:          clickhouse.AccountCohortCycle{ComputedAt: at, TipLedger: 64_400_000},
		CohortAccounts: 1200, LiveAccounts: 900,
		Activity: clickhouse.AccountCohortActivity{Active30d: 40, Active90d: 90, Active365d: 300},
		Holdings: []clickhouse.AccountCohortHolding{
			{Asset: "native", Holders: 900, Balance: big.NewInt(12_500_000_000)},  // 1,250 XLM
			{Asset: cohortUSDC, Holders: 300, Balance: big.NewInt(4_000_000_000)}, // 400 USDC
			{Asset: "pool:0a1b", Holders: 5, Balance: big.NewInt(70_000_000)},     // 7 shares
			{Asset: "CCTOKEN", Holders: 2, Balance: big.NewInt(1_000)},            // raw contract units
		},
		Flows: []clickhouse.AccountCohortFlow{
			{Month: histMonth(2026, time.July), Asset: "*", Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 50, ActiveAccounts: 21},
			{Month: histMonth(2026, time.July), Asset: cohortUSDC, Inflow: big.NewInt(1_000_000_000), Outflow: big.NewInt(250_000_000), Movements: 30, ActiveAccounts: 12},
			{Month: histMonth(2026, time.July), Asset: "native", Inflow: big.NewInt(30_000_000), Outflow: big.NewInt(0), Movements: 20, ActiveAccounts: 15},
			{Month: histMonth(2026, time.September), Asset: "*", Inflow: big.NewInt(0), Outflow: big.NewInt(0), Movements: 3, ActiveAccounts: 2},
			{Month: histMonth(2026, time.September), Asset: "CCTOKEN", Inflow: big.NewInt(5_000), Outflow: big.NewInt(0), Movements: 3, ActiveAccounts: 2},
		},
		Contracts: []clickhouse.AccountCohortContract{
			{ContractID: "CBLENDPOOL", Movements: 40, ActiveAccounts: 9, FirstAt: at.AddDate(0, -3, 0), LastAt: at},
			{ContractID: "CUNKNOWN", Movements: 1, ActiveAccounts: 1, FirstAt: at, LastAt: at},
		},
		Positions: []clickhouse.AccountCohortPosition{
			{Protocol: "blend", PositionKind: "lending_supply", Venue: "CBLENDPOOL", Asset: "", Holders: 4, Amount: cohortPositionAmount()},
		},
	}
}

func getCohort(t *testing.T, reader *stubExplorerReader, path string) (cohortEnvelope, int) {
	t.Helper()
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+path)
	var env cohortEnvelope
	if resp.StatusCode == http.StatusOK {
		mustDecode(t, resp, &env)
	} else {
		_ = resp.Body.Close()
	}
	return env, resp.StatusCode
}

func TestAccountGraphCohort_RequiresARelation(t *testing.T) {
	reader := &stubExplorerReader{cohort: cohortSnapshot(), cohortOK: true}
	for _, q := range []string{"", "?relation=funded"} {
		if _, status := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort"+q); status != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", q, status)
		}
	}
}

func TestAccountGraphCohort_WarmingUntilTheFirstCycle(t *testing.T) {
	reader := &stubExplorerReader{cohortOK: false}
	if _, status := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=created"); status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 before the first cycle", status)
	}
}

// A root the rollup does not carry is a 200 that SAYS so — never an
// empty cohort that reads as "holds nothing".
func TestAccountGraphCohort_UncoveredRootIsNotAnEmptyCohort(t *testing.T) {
	snap := cohortSnapshot()
	snap.Covered = false
	snap.Holdings, snap.Flows, snap.Contracts, snap.Positions = nil, nil, nil, nil
	reader := &stubExplorerReader{cohort: snap, cohortOK: true}
	env, status := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=sponsored")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if env.Data.Covered {
		t.Error("covered = true for a root the rollup does not carry")
	}
	if env.Data.Cohort != nil {
		t.Error("an uncovered root must not carry cohort counts")
	}
	if env.Data.Cycle == nil || env.Data.Cycle.TipLedger != 64_400_000 {
		t.Error("the cycle is still dated so the reader knows the rollup ran")
	}
	if reader.cohortRelation != "sponsored" {
		t.Errorf("relation passed to the reader = %q, want sponsored", reader.cohortRelation)
	}
}

// Holdings are rendered in whole units, classified, and valued only
// where the live price exists: a pool share and a contract token are
// served unpriced, never dropped and never priced at zero.
func TestAccountGraphCohort_HoldingsAreClassifiedAndPricedWhereTheyCanBe(t *testing.T) {
	reader := &stubExplorerReader{cohort: cohortSnapshot(), cohortOK: true}
	env, status := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=created")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if env.Data.Cohort == nil || env.Data.Cohort.Accounts != 1200 || env.Data.Cohort.LiveAccounts != 900 || env.Data.Cohort.Active30d != 40 {
		t.Errorf("cohort counts = %+v", env.Data.Cohort)
	}
	byAsset := map[string]cohortHoldingEnv{}
	for _, h := range env.Data.Holdings {
		byAsset[h.Asset] = h
	}
	if got := byAsset["native"]; got.Kind != "native" || got.Balance != "1250" {
		t.Errorf("native holding = %+v, want kind native balance 1250", got)
	}
	if got := byAsset[cohortUSDC]; got.Kind != "classic" || got.Balance != "400" {
		t.Errorf("USDC holding = %+v, want kind classic balance 400", got)
	}
	if got := byAsset["pool:0a1b"]; got.Kind != "pool_share" || got.Balance != "7" || got.ValueUSD != nil {
		t.Errorf("pool share = %+v, want kind pool_share, balance 7, unpriced", got)
	}
	if got := byAsset["CCTOKEN"]; got.Kind != "contract" || got.ValueUSD != nil {
		t.Errorf("contract token = %+v, want kind contract, unpriced", got)
	}
	// The explorer test server wires no price source, so nothing is
	// priced here — and that is served as "unpriced", never as $0. The
	// priced path is proven in the explorer package's unit test.
	if env.Data.Valuation.UnpricedHoldings != 4 || env.Data.Valuation.PricedHoldings != 0 || env.Data.Valuation.TotalUSD != nil {
		t.Errorf("valuation without a price source = %+v, want 4 unpriced, no total", env.Data.Valuation)
	}
	if env.Data.Valuation.Basis != "live_vwap_current" {
		t.Errorf("valuation basis = %q", env.Data.Valuation.Basis)
	}
	if env.Data.HoldingsTruncated {
		t.Error("four holdings must not read as truncated")
	}
}

// Flows keep the all-assets row as the month's headline and break the
// rest out per asset; a contract token is served in its own raw unit and
// says so; a quiet month emits no point.
func TestAccountGraphCohort_FlowsAreMonthlyAndUnitHonest(t *testing.T) {
	reader := &stubExplorerReader{cohort: cohortSnapshot(), cohortOK: true}
	env, _ := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=created")
	pts := env.Data.Flows.Points
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2 (July, September — August was quiet): %+v", len(pts), pts)
	}
	jul := pts[0]
	if jul.Period != "2026-07" || jul.Movements != 50 || jul.ActiveAccounts != 21 {
		t.Errorf("July headline = %+v, want period 2026-07 movements 50 active 21 (the all-assets row)", jul)
	}
	if len(jul.ByAsset) != 2 {
		t.Fatalf("July by_asset = %+v, want USDC + native only (never the '*' row)", jul.ByAsset)
	}
	var usdc cohortAssetFlowEnv
	for _, a := range jul.ByAsset {
		if a.Asset == cohortUSDC {
			usdc = a
		}
		if a.Asset == "*" {
			t.Error("the all-assets row leaked into by_asset")
		}
	}
	if usdc.Inflow != "100" || usdc.Outflow != "25" || !usdc.Scaled {
		t.Errorf("USDC July flow = %+v, want inflow 100 outflow 25 scaled", usdc)
	}
	sep := pts[1]
	if sep.Period != "2026-09" || len(sep.ByAsset) != 1 || sep.ByAsset[0].Scaled || sep.ByAsset[0].Inflow != "5000" {
		t.Errorf("September = %+v, want one raw contract-unit row of 5000", sep)
	}
	if env.Data.Flows.Granularity != "1M" {
		t.Errorf("granularity = %q", env.Data.Flows.Granularity)
	}
	if len(env.Data.Flows.Assets) != 3 {
		t.Errorf("assets broken out = %v, want 3", env.Data.Flows.Assets)
	}
}

func TestAccountGraphCohort_ContractsAndPositionsAreServedAsRead(t *testing.T) {
	reader := &stubExplorerReader{cohort: cohortSnapshot(), cohortOK: true}
	env, _ := getCohort(t, reader, "/v1/accounts/"+graphSubject+"/graph/cohort?relation=created")
	if len(env.Data.Contracts) != 2 || env.Data.Contracts[0].ContractID != "CBLENDPOOL" || env.Data.Contracts[0].ActiveAccounts != 9 {
		t.Errorf("contracts = %+v", env.Data.Contracts)
	}
	if len(env.Data.Positions) != 1 || env.Data.Positions[0].Protocol != "blend" || env.Data.Positions[0].Holders != 4 || env.Data.Positions[0].Amount != cohortPositionAmountStr {
		t.Errorf("positions = %+v", env.Data.Positions)
	}
	if env.Data.Note == "" {
		t.Error("note must always be present")
	}
}
