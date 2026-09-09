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

// graphEdges builds n synthetic outbound edges, ascending by account id
// so a keyset walk over them is deterministic.
//
// The ids are REAL G-strkeys (CRC-checked base32), not strkey-shaped
// strings: the handler validates a cursor as an account id, so a fixture
// built from fakes would exercise the 400 path on page two and never
// reach the pagination this file exists to pin.
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

// The high-cardinality direction MUST be paged. The busiest sponsor on
// the network covers 785,543 distinct accounts, so an endpoint that
// returned "accounts sponsored by X" whole is not a feature — this pins
// that the default response carries NO outbound edge list at all, that
// asking for one is bounded by ?limit=, that a full page hands back a
// cursor, and that walking the cursor terminates.
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

// A revoked sponsorship is representable in exactly ONE way here, and
// this pins it: revocations are an ACCOUNT-level count, never an edge
// attribute, because RevokeSponsorship names its target inside body_xdr
// the rollup does not decode. Any per-edge "current"/"revoked"/"active"
// field would be a claim the data cannot support.
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

// The two arms are separate cycles over separate sources and their
// coverage must NOT be merged: creation history reaches genesis,
// sponsorship history only reaches protocol 14, where the feature began
// to exist. One merged span would present that floor as a gap.
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
