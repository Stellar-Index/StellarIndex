package v1_test

import (
	"net/http"
	"testing"
)

// The league tables are top-N pages over an aggregation of 955,023
// creators and 2,427 sponsors, and `rank` is a property of that whole
// aggregation. Without a keyed lookup a caller wanting one address's
// standing has to pull the cap and hope the address is inside it — and
// an address past the cap is indistinguishable from one that never
// appears at all. Those are different answers and a UI must not conflate
// them. `?account=` is the keyed arm; these tests pin its four cases.

const (
	// On the creator board fixture.
	filterKnownCreator = "GCZGSFPITKVJPJERJIVLCQK5YIHYTDXCY45ZHU3IRCUC53SXSCAL44JV"
	// On the sponsor board fixture.
	filterKnownSponsor = "GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU"
	// Well-formed, checksum-valid, and on neither board.
	filterAbsentAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

func TestExplorer_AccountCreators_AccountFilterServesOneRowWithItsRealRank(t *testing.T) {
	reader := &stubExplorerReader{accountCreators: creatorsSnapshot(t)}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/creators?account="+filterKnownCreator)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env accountCreatorsEnvelope
	mustDecode(t, resp, &env)

	if got := len(env.Data.Creators); got != 1 {
		t.Fatalf("creators = %d, want exactly the filtered row", got)
	}
	row := env.Data.Creators[0]
	if row.Account != filterKnownCreator {
		t.Errorf("account = %q, want %q", row.Account, filterKnownCreator)
	}
	// The rank the ROLLUP holds, not the row's position in the response.
	// Serving 1 here would be the defect the filter exists to avoid.
	if row.Rank != 2 {
		t.Errorf("rank = %d, want 2 — the whole-aggregation rank", row.Rank)
	}
	// Totals and coverage describe the aggregation, never the filtered
	// row. A filtered read that also narrowed these would publish
	// "there is 1 creator on Stellar".
	if env.Data.Totals.Creators != 13119 {
		t.Errorf("totals.creators = %d, want the whole-aggregation 13119", env.Data.Totals.Creators)
	}
	if env.Data.Totals.AccountsCreated != 359328 {
		t.Errorf("totals.accounts_created = %d, want 359328", env.Data.Totals.AccountsCreated)
	}
	if reader.creatorsAccount != filterKnownCreator {
		t.Errorf("reader saw account %q, want %q", reader.creatorsAccount, filterKnownCreator)
	}
}

func TestExplorer_AccountSponsors_AccountFilterServesOneRowWithItsRealRank(t *testing.T) {
	reader := &stubExplorerReader{accountSponsors: sponsorsSnapshot()}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/sponsors?account="+filterKnownSponsor)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env accountSponsorsEnvelope
	mustDecode(t, resp, &env)

	if got := len(env.Data.Sponsors); got != 1 {
		t.Fatalf("sponsors = %d, want exactly the filtered row", got)
	}
	if env.Data.Sponsors[0].Rank != 2 {
		t.Errorf("rank = %d, want 2", env.Data.Sponsors[0].Rank)
	}
	if env.Data.Totals.Sponsors != 41208 {
		t.Errorf("totals.sponsors = %d, want the whole-aggregation figure", env.Data.Totals.Sponsors)
	}
	if reader.sponsorsAccount != filterKnownSponsor {
		t.Errorf("reader saw account %q", reader.sponsorsAccount)
	}
}

// A well-formed address holding no row is an ANSWER — "this account never
// created one" — and answers are 200s. A 404 here would be the surface
// claiming the address does not exist, which it has no basis to say.
func TestExplorer_Boards_WellFormedAccountWithNoRowIsAnEmpty200(t *testing.T) {
	for _, tc := range []struct {
		name, path string
	}{
		{"creators", "/v1/accounts/creators?account=" + filterAbsentAccount},
		{"sponsors", "/v1/accounts/sponsors?account=" + filterAbsentAccount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubExplorerReader{
				accountCreators: creatorsSnapshot(t),
				accountSponsors: sponsorsSnapshot(),
			}
			base := explorerTestServer(t, reader)

			resp := mustGet(t, base+tc.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var env struct {
				Data struct {
					Creators []struct{} `json:"creators"`
					Sponsors []struct{} `json:"sponsors"`
				} `json:"data"`
			}
			mustDecode(t, resp, &env)
			if n := len(env.Data.Creators) + len(env.Data.Sponsors); n != 0 {
				t.Errorf("rows = %d, want 0", n)
			}
		})
	}
}

// A mangled address must not reach the reader and must not come back as
// an empty board: echoing a corrupted value back as "this account did
// nothing" is a false statement about whatever the caller meant.
func TestExplorer_Boards_MalformedAccountIs400AndNeverReachesTheReader(t *testing.T) {
	for _, bad := range []string{
		"not-an-address",
		"GBMUZ7DCFWJ47CI2FGFR4NIVSZNPPZENJJWNG7THSRWQWFZVNUNZJTR5", // checksum flipped
		"CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26YF", // a contract, not an account
		"GBMUZ7DCFWJ47CI2FGFR4NIVSZNPPZENJJWNG7THSRWQWFZVNUNZJTR",  // truncated
	} {
		for _, board := range []string{"creators", "sponsors"} {
			reader := &stubExplorerReader{
				accountCreators: creatorsSnapshot(t),
				accountSponsors: sponsorsSnapshot(),
			}
			base := explorerTestServer(t, reader)

			resp := mustGet(t, base+"/v1/accounts/"+board+"?account="+bad)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s %q: status = %d, want 400", board, bad, resp.StatusCode)
			}
			_ = resp.Body.Close()
			if reader.creatorsAccount != "" || reader.sponsorsAccount != "" {
				t.Errorf("%s %q: a malformed address reached the reader", board, bad)
			}
		}
	}
}

// No filter is the unchanged board: the limit still applies and every
// row is still served.
func TestExplorer_Boards_NoFilterIsTheUnchangedBoard(t *testing.T) {
	reader := &stubExplorerReader{accountCreators: creatorsSnapshot(t)}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/creators")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env accountCreatorsEnvelope
	mustDecode(t, resp, &env)
	if got := len(env.Data.Creators); got != 2 {
		t.Fatalf("creators = %d, want the whole fixture board", got)
	}
	if reader.creatorsAccount != "" {
		t.Errorf("reader saw account %q, want empty", reader.creatorsAccount)
	}
}
