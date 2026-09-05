package v1_test

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// accountCreatorsEnvelope declares the wire field names explicitly, so
// these assertions pin the JSON contract rather than re-reading the
// handler's own struct tags.
type accountCreatorsEnvelope struct {
	Data struct {
		Creators []struct {
			Rank            uint32 `json:"rank"`
			Account         string `json:"account"`
			AccountsCreated uint64 `json:"accounts_created"`
			FundedStroops   string `json:"funded_stroops"`
			LiveAccounts    uint64 `json:"live_accounts"`
			LiveStroops     string `json:"live_stroops"`
			FirstLedger     uint32 `json:"first_ledger"`
			LastLedger      uint32 `json:"last_ledger"`
			FirstCreatedAt  string `json:"first_created_at"`
			LastCreatedAt   string `json:"last_created_at"`
		} `json:"creators"`
		Totals struct {
			Creators        int64 `json:"creators"`
			AccountsCreated int64 `json:"accounts_created"`
			LiveAccounts    int64 `json:"live_accounts"`
		} `json:"totals"`
		Coverage struct {
			FromLedger uint32 `json:"from_ledger"`
			ThruLedger uint32 `json:"thru_ledger"`
			FromTime   string `json:"from_time"`
			ThruTime   string `json:"thru_time"`
		} `json:"coverage"`
		ComputedAt string `json:"computed_at"`
	} `json:"data"`
}

func stroops(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad stroops literal %q", s)
	}
	return v
}

// creatorsSnapshot is a two-row board whose span is deliberately NARROWER
// than the chain: it starts at ledger 33,000,000, not genesis. Every
// coverage assertion below reads that back, so a handler that reported a
// wider span than the projection holds would fail.
func creatorsSnapshot(t *testing.T) clickhouse.AccountCreators {
	t.Helper()
	return clickhouse.AccountCreators{
		Board: []clickhouse.AccountCreatorRow{
			{
				Rank: 1, Creator: "GBMUZ7DCFWJ47CI2FGFR4NIVSZNPPZENJJWNG7THSRWQWFZVNUNZJTR4",
				AccountsCreated: 108730, FundedStroops: stroops(t, "5290690000000"),
				LiveAccounts: 4346, LiveStroops: stroops(t, "71721284463"),
				FirstLedger: 33000001, LastLedger: 50999236,
				FirstCreatedAt: time.Date(2024, 3, 18, 4, 11, 2, 0, time.UTC),
				LastCreatedAt:  time.Date(2024, 5, 2, 22, 7, 44, 0, time.UTC),
			},
			{
				// A creator that only ever made SPONSORED accounts: it paid
				// no starting balance at all. "0" here is a fact, not a gap
				// (CAP-33), and must survive to the wire as "0".
				Rank: 2, Creator: "GCZGSFPITKVJPJERJIVLCQK5YIHYTDXCY45ZHU3IRCUC53SXSCAL44JV",
				AccountsCreated: 52297, FundedStroops: big.NewInt(0),
				LiveAccounts: 52094, LiveStroops: stroops(t, "107391888628"),
				FirstLedger: 33000023, LastLedger: 50999990,
				FirstCreatedAt: time.Date(2024, 3, 18, 5, 0, 0, 0, time.UTC),
				LastCreatedAt:  time.Date(2024, 5, 2, 23, 0, 0, 0, time.UTC),
			},
		},
		CreatorsTotal: 13119, CreationsTotal: 359328, LiveAccountsTotal: 71000,
		FromLedger: 33000001, ThruLedger: 50999990,
		FromTime:   time.Date(2024, 3, 18, 4, 11, 2, 0, time.UTC),
		ThruTime:   time.Date(2024, 5, 2, 23, 0, 0, 0, time.UTC),
		ComputedAt: time.Date(2026, 9, 5, 9, 30, 0, 0, time.UTC),
	}
}

func TestExplorer_AccountCreators_WireShape(t *testing.T) {
	reader := &stubExplorerReader{accountCreators: creatorsSnapshot(t)}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/creators")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env accountCreatorsEnvelope
	mustDecode(t, resp, &env)

	if got := len(env.Data.Creators); got != 2 {
		t.Fatalf("creators = %d, want 2", got)
	}
	top := env.Data.Creators[0]
	if top.Rank != 1 {
		t.Errorf("rank = %d, want 1", top.Rank)
	}
	if top.Account != "GBMUZ7DCFWJ47CI2FGFR4NIVSZNPPZENJJWNG7THSRWQWFZVNUNZJTR4" {
		t.Errorf("account = %q", top.Account)
	}
	if top.AccountsCreated != 108730 {
		t.Errorf("accounts_created = %d, want 108730", top.AccountsCreated)
	}
	// Money is a decimal STRING, exact, never a float (ADR-0003).
	if top.FundedStroops != "5290690000000" {
		t.Errorf("funded_stroops = %q, want %q", top.FundedStroops, "5290690000000")
	}
	if top.LiveStroops != "71721284463" {
		t.Errorf("live_stroops = %q, want %q", top.LiveStroops, "71721284463")
	}
	if top.FirstCreatedAt != "2024-03-18T04:11:02Z" {
		t.Errorf("first_created_at = %q", top.FirstCreatedAt)
	}

	// A sponsored-only creator funds nothing; "0" must reach the wire as
	// a real figure rather than being dropped or rendered as an error.
	if got := env.Data.Creators[1].FundedStroops; got != "0" {
		t.Errorf("sponsored creator funded_stroops = %q, want %q", got, "0")
	}
	if got := env.Data.Creators[1].AccountsCreated; got != 52297 {
		t.Errorf("sponsored creator accounts_created = %d, want 52297", got)
	}

	// Totals describe the whole aggregation, not the returned page.
	if env.Data.Totals.Creators != 13119 {
		t.Errorf("totals.creators = %d, want 13119", env.Data.Totals.Creators)
	}
	if env.Data.Totals.AccountsCreated != 359328 {
		t.Errorf("totals.accounts_created = %d, want 359328", env.Data.Totals.AccountsCreated)
	}
	if env.Data.ComputedAt != "2026-09-05T09:30:00Z" {
		t.Errorf("computed_at = %q", env.Data.ComputedAt)
	}
}

// TestExplorer_AccountCreators_ServedSpanMatchesProjection is the
// regression guard for the defect class this surface is most likely to
// fall into: reporting a coverage span wider than the rows behind it.
//
// The snapshot's span starts at ledger 33,000,001 — NOT genesis — and
// the response must say so. A handler that hardcoded genesis, or that
// derived the span from anything other than the projection, fails here.
func TestExplorer_AccountCreators_ServedSpanMatchesProjection(t *testing.T) {
	snap := creatorsSnapshot(t)
	reader := &stubExplorerReader{accountCreators: snap}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/creators")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env accountCreatorsEnvelope
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
		t.Errorf("coverage.from_ledger = %d — a genesis floor here would be the "+
			"overstatement this test exists to catch", env.Data.Coverage.FromLedger)
	}
	if env.Data.Coverage.FromTime != snap.FromTime.Format(time.RFC3339) {
		t.Errorf("coverage.from_time = %q, want %q",
			env.Data.Coverage.FromTime, snap.FromTime.Format(time.RFC3339))
	}
	if env.Data.Coverage.ThruTime != snap.ThruTime.Format(time.RFC3339) {
		t.Errorf("coverage.thru_time = %q, want %q",
			env.Data.Coverage.ThruTime, snap.ThruTime.Format(time.RFC3339))
	}

	// Every row on the board must sit inside the span the response
	// claims. A row outside it would mean the board and the span came
	// from different cycles.
	for _, c := range env.Data.Creators {
		if c.FirstLedger < env.Data.Coverage.FromLedger {
			t.Errorf("%s first_ledger %d precedes coverage.from_ledger %d",
				c.Account, c.FirstLedger, env.Data.Coverage.FromLedger)
		}
		if c.LastLedger > env.Data.Coverage.ThruLedger {
			t.Errorf("%s last_ledger %d exceeds coverage.thru_ledger %d",
				c.Account, c.LastLedger, env.Data.Coverage.ThruLedger)
		}
	}
}

// A snapshot with rows but no span cannot be qualified honestly, so it
// must not be served as a board. This is the half-swapped-exchange case:
// the board arm landing while the stats arm is still empty.
func TestExplorer_AccountCreators_BoardWithoutSpanIsWarming(t *testing.T) {
	snap := creatorsSnapshot(t)
	snap.FromLedger, snap.ThruLedger = 0, 0
	reader := &stubExplorerReader{accountCreators: snap}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/creators")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a board with no coverage span", resp.StatusCode)
	}
}

func TestExplorer_AccountCreators_WarmingBeforeFirstCycle(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})

	resp := mustGet(t, base+"/v1/accounts/creators")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	// Problem responses must never be cached (cachecontrol.go invariant).
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// The limit is the caller's, clamped by the handler — the reader must
// never be asked for an unbounded or absurd page.
func TestExplorer_AccountCreators_LimitClamped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{"default", "", 50},
		{"explicit", "?limit=2", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubExplorerReader{accountCreators: creatorsSnapshot(t)}
			base := explorerTestServer(t, reader)

			resp := mustGet(t, base+"/v1/accounts/creators"+tc.query)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if reader.creatorsLimit != tc.want {
				t.Errorf("reader asked for limit %d, want %d", reader.creatorsLimit, tc.want)
			}
		})
	}

	t.Run("above the cap is rejected", func(t *testing.T) {
		reader := &stubExplorerReader{accountCreators: creatorsSnapshot(t)}
		base := explorerTestServer(t, reader)

		resp := mustGet(t, base+"/v1/accounts/creators?limit=100000")
		if resp.StatusCode == http.StatusOK && reader.creatorsLimit > 500 {
			t.Errorf("reader asked for limit %d, above the 500 cap", reader.creatorsLimit)
		}
	})
}
