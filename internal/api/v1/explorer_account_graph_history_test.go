// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

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

// graphHistorySnapshot is the base fixture, and every figure in it is
// arithmetically consistent with the reader's contract:
//
//	created:   3 edges (3 new accounts) across 3 months, 7 creations of
//	           which 5 are placed and 2 are not.
//	sponsored: 2 edges across 2 months, 2 arrangements, all placed.
//
// The deliberate GAP is 2022 in the creation arm: the account is active
// in 2021-03 and 2023-07 and silent in between. Nothing may invent a
// point for the quiet months.
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

// THE honesty rule of this surface: a period with no observation is a
// gap, not a zero. The fixture is silent from 2021-04 to 2023-06, so no
// point may exist for any of those months and no value may be carried
// across them.
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

// Events the served tier cannot place in a month are declared, counted
// exactly, and flagged as a lower bound — never smeared across the
// interval and never dropped silently. This is the same posture
// /v1/rwa/assets takes with excluded[].
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

// The two arms cover different spans — creation reaches genesis,
// sponsorship only reaches protocol 14 — so each series carries its own,
// and merging them would present sponsorship's floor as a gap in the
// creation record.
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

// Before the rollup's first cycle the graph tables are empty, and a
// series over them would answer "this account has never created
// anything" — a claim, not an absence. The route must warm-503 instead,
// exactly as /graph does.
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
