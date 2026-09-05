package v1_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type accountSponsorsEnvelope struct {
	Data struct {
		Sponsors []struct {
			Rank                uint32 `json:"rank"`
			Account             string `json:"account"`
			SponsorshipsStarted uint64 `json:"sponsorships_started"`
			DistinctSponsored   uint64 `json:"distinct_sponsored"`
			RevocationsIssued   uint64 `json:"revocations_issued"`
			FirstLedger         uint32 `json:"first_ledger"`
			LastLedger          uint32 `json:"last_ledger"`
			FirstSeenAt         string `json:"first_seen_at"`
			LastSeenAt          string `json:"last_seen_at"`
		} `json:"sponsors"`
		Totals struct {
			Sponsors            int64 `json:"sponsors"`
			SponsorshipsStarted int64 `json:"sponsorships_started"`
			DistinctSponsored   int64 `json:"distinct_sponsored"`
			RevocationsIssued   int64 `json:"revocations_issued"`
		} `json:"totals"`
		Coverage struct {
			FromLedger   uint32 `json:"from_ledger"`
			ThruLedger   uint32 `json:"thru_ledger"`
			FromTime     string `json:"from_time"`
			ThruTime     string `json:"thru_time"`
			AmbiguousTxs int64  `json:"ambiguous_transactions"`
		} `json:"coverage"`
		ComputedAt string `json:"computed_at"`
	} `json:"data"`
}

// protocol14Floor is where sponsorship was introduced on the network —
// the first BeginSponsoringFutureReserves operation that exists. It is
// the honest floor of any sponsorship history, and the fixture uses it
// so the coverage assertions pin a real chain fact rather than genesis.
const protocol14Floor = 32747295

func sponsorsSnapshot() clickhouse.AccountSponsors {
	return clickhouse.AccountSponsors{
		Board: []clickhouse.AccountSponsorRow{
			{
				Rank: 1, Sponsor: "GAUA7XL5K54CC2DDGP77FJ2YBHRJLT36CPZDXWPM6MP7MANOGG77PNJU",
				SponsorshipsStarted: 30512, DistinctSponsored: 28011, RevocationsIssued: 0,
				FirstLedger: protocol14Floor, LastLedger: 64277239,
				FirstSeenAt: time.Date(2021, 2, 16, 18, 21, 0, 0, time.UTC),
				LastSeenAt:  time.Date(2026, 9, 5, 0, 14, 59, 0, time.UTC),
			},
			{
				// Re-sponsors the same accounts repeatedly AND revokes:
				// started (20360) is an order of magnitude above distinct
				// (2036), and revocations are non-zero. The two columns must
				// stay distinct on the wire.
				Rank: 2, Sponsor: "GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU",
				SponsorshipsStarted: 20360, DistinctSponsored: 2036, RevocationsIssued: 261,
				FirstLedger: 64000032, LastLedger: 64277224,
				FirstSeenAt: time.Date(2026, 8, 17, 19, 43, 29, 0, time.UTC),
				LastSeenAt:  time.Date(2026, 9, 5, 0, 13, 36, 0, time.UTC),
			},
		},
		SponsorsTotal: 41208, SponsorshipsTotal: 11487143,
		DistinctSponsoredTotal: 9120044, RevocationsTotal: 88525,
		AmbiguousTxs: 0,
		FromLedger:   protocol14Floor, ThruLedger: 64277243,
		FromTime:   time.Date(2021, 2, 16, 18, 21, 0, 0, time.UTC),
		ThruTime:   time.Date(2026, 9, 5, 0, 15, 11, 0, time.UTC),
		ComputedAt: time.Date(2026, 9, 5, 4, 30, 0, 0, time.UTC),
	}
}

func TestExplorer_AccountSponsors_WireShape(t *testing.T) {
	reader := &stubExplorerReader{accountSponsors: sponsorsSnapshot()}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/sponsors")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env accountSponsorsEnvelope
	mustDecode(t, resp, &env)

	if got := len(env.Data.Sponsors); got != 2 {
		t.Fatalf("sponsors = %d, want 2", got)
	}
	top := env.Data.Sponsors[0]
	if top.Rank != 1 || top.Account != "GAUA7XL5K54CC2DDGP77FJ2YBHRJLT36CPZDXWPM6MP7MANOGG77PNJU" {
		t.Errorf("top row = rank %d account %q", top.Rank, top.Account)
	}
	if top.SponsorshipsStarted != 30512 || top.DistinctSponsored != 28011 {
		t.Errorf("started/distinct = %d/%d, want 30512/28011",
			top.SponsorshipsStarted, top.DistinctSponsored)
	}
	// Started and distinct are separate facts and must not be collapsed.
	second := env.Data.Sponsors[1]
	if second.SponsorshipsStarted == second.DistinctSponsored {
		t.Error("a re-sponsoring account must keep started and distinct apart on the wire")
	}
	if second.RevocationsIssued != 261 {
		t.Errorf("revocations_issued = %d, want 261", second.RevocationsIssued)
	}
	if env.Data.Totals.Sponsors != 41208 || env.Data.Totals.RevocationsIssued != 88525 {
		t.Errorf("totals = %+v", env.Data.Totals)
	}
	if env.Data.ComputedAt != "2026-09-05T04:30:00Z" {
		t.Errorf("computed_at = %q", env.Data.ComputedAt)
	}
}

// The served span must be the projection's own, and its floor must be
// the protocol-14 activation the data actually starts at — never genesis
// and never the creators board's floor.
func TestExplorer_AccountSponsors_ServedSpanMatchesProjection(t *testing.T) {
	snap := sponsorsSnapshot()
	base := explorerTestServer(t, &stubExplorerReader{accountSponsors: snap})

	resp := mustGet(t, base+"/v1/accounts/sponsors")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env accountSponsorsEnvelope
	mustDecode(t, resp, &env)

	if env.Data.Coverage.FromLedger != snap.FromLedger {
		t.Errorf("coverage.from_ledger = %d, want %d (the projection's own floor)",
			env.Data.Coverage.FromLedger, snap.FromLedger)
	}
	if env.Data.Coverage.ThruLedger != snap.ThruLedger {
		t.Errorf("coverage.thru_ledger = %d, want %d",
			env.Data.Coverage.ThruLedger, snap.ThruLedger)
	}
	if env.Data.Coverage.FromLedger <= 1 {
		t.Errorf("coverage.from_ledger = %d — sponsorship cannot predate protocol 14, "+
			"and a genesis floor here would be the overstatement this test catches",
			env.Data.Coverage.FromLedger)
	}
	if env.Data.Coverage.FromTime != snap.FromTime.Format(time.RFC3339) {
		t.Errorf("coverage.from_time = %q", env.Data.Coverage.FromTime)
	}
	for _, s := range env.Data.Sponsors {
		if s.FirstLedger < env.Data.Coverage.FromLedger {
			t.Errorf("%s first_ledger %d precedes coverage.from_ledger %d",
				s.Account, s.FirstLedger, env.Data.Coverage.FromLedger)
		}
		if s.LastLedger > env.Data.Coverage.ThruLedger {
			t.Errorf("%s last_ledger %d exceeds coverage.thru_ledger %d",
				s.Account, s.LastLedger, env.Data.Coverage.ThruLedger)
		}
	}
}

// The attribution exclusion must reach the wire. If ambiguous
// transactions are ever non-zero, a consumer has to be able to see it.
func TestExplorer_AccountSponsors_PublishesAmbiguousExclusion(t *testing.T) {
	snap := sponsorsSnapshot()
	snap.AmbiguousTxs = 7
	base := explorerTestServer(t, &stubExplorerReader{accountSponsors: snap})

	resp := mustGet(t, base+"/v1/accounts/sponsors")
	var env accountSponsorsEnvelope
	mustDecode(t, resp, &env)

	if env.Data.Coverage.AmbiguousTxs != 7 {
		t.Errorf("coverage.ambiguous_transactions = %d, want 7 — excluded transactions "+
			"must be visible, not silent", env.Data.Coverage.AmbiguousTxs)
	}
}

// No live-state field may appear on this surface. Sponsorship is
// revocable and the operation stream cannot observe the current set, so
// a field implying it would be the defect this endpoint is scoped
// against.
func TestExplorer_AccountSponsors_ClaimsNoLiveState(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{accountSponsors: sponsorsSnapshot()})

	resp := mustGet(t, base+"/v1/accounts/sponsors")
	body := sponsorsBodyString(t, resp)
	for _, banned := range []string{
		"currently_sponsoring", "live_sponsored", "active_sponsorships", "sponsoring_now",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("response carries %q — the operation stream cannot observe live "+
				"sponsorship state, so no field may imply it", banned)
		}
	}
}

func TestExplorer_AccountSponsors_WarmingBeforeFirstCycle(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})

	resp := mustGet(t, base+"/v1/accounts/sponsors")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
}

func TestExplorer_AccountSponsors_BoardWithoutSpanIsWarming(t *testing.T) {
	snap := sponsorsSnapshot()
	snap.FromLedger, snap.ThruLedger = 0, 0
	base := explorerTestServer(t, &stubExplorerReader{accountSponsors: snap})

	resp := mustGet(t, base+"/v1/accounts/sponsors")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a board with no coverage span", resp.StatusCode)
	}
}

func TestExplorer_AccountSponsors_LimitClamped(t *testing.T) {
	reader := &stubExplorerReader{accountSponsors: sponsorsSnapshot()}
	base := explorerTestServer(t, reader)

	if resp := mustGet(t, base+"/v1/accounts/sponsors"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if reader.sponsorsLimit != 50 {
		t.Errorf("default limit = %d, want 50", reader.sponsorsLimit)
	}
	if resp := mustGet(t, base+"/v1/accounts/sponsors?limit=100000"); resp.StatusCode == http.StatusOK && reader.sponsorsLimit > 500 {
		t.Errorf("reader asked for limit %d, above the 500 cap", reader.sponsorsLimit)
	}
}

// sponsorsBodyString reads the response body for the field-absence
// assertion above.
func sponsorsBodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
