// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// listingCoinsFixture is a trimmed, shape-faithful `/coins/list?include_platform=true` response whose seven rows each exercise a parseListingCatalogue path:
// bitcoin (empty platforms, not a Stellar row), stellar (C-strkey), aquarius (CODE-GISSUER), stellar-synthetic-usd (lowercase code sUSD, defeats an `[A-Z0-9]`-only class),
// cetes (code beginning with C beside a Solana C-address, defeats a `LIKE 'C%'` contract test), broken-listing (malformed address, skipped AND counted), empty-platform (`platforms.stellar` present but empty; not a row, not a skip).
const listingCoinsFixture = `[
  {"id": "bitcoin", "symbol": "btc", "name": "Bitcoin", "platforms": {}},
  {"id": "stellar", "symbol": "xlm", "name": "Stellar",
   "platforms": {"stellar": "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"}},
  {"id": "aquarius", "symbol": "aqua", "name": "Aquarius",
   "platforms": {"stellar": "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"}},
  {"id": "stellar-synthetic-usd", "symbol": "susd", "name": "Stellar Synthetic USD",
   "platforms": {"stellar": "sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH"}},
  {"id": "cetes", "symbol": "cetes", "name": "Etherfuse CETES",
   "platforms": {"solana": "CETES7CKqqKQizuSN6iWQwmTeFRjbJR6Vw2XRKfEDR8f",
                 "base": "0x834df4c1d8f51be24322e39e4766697be015512f",
                 "stellar": "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"}},
  {"id": "broken-listing", "symbol": "brk", "name": "Broken Listing",
   "platforms": {"stellar": "NOT-A-STELLAR-ADDRESS"}},
  {"id": "empty-platform", "symbol": "emp", "name": "Empty Platform",
   "platforms": {"stellar": ""}}
]`

// listingMarketsFixture is a trimmed `/coins/markets` response for those ids, with one unread field to prove the decode ignores the rest. The cases that matter:
// aquarius has more significant digits than a float64 holds and must survive verbatim; stellar-synthetic-usd has no `last_updated` and is REJECTED (an unverifiable age is not served, no timestamp invented);
// cetes has `current_price: null` (unpriced, not a rejection); broken-listing is priced but its address is not in the entry set.
const listingMarketsFixture = `[
  {"id": "stellar", "symbol": "xlm", "current_price": 0.195274,
   "market_cap": 6814637810, "last_updated": "2026-09-15T14:05:00.000Z"},
  {"id": "aquarius", "symbol": "aqua", "current_price": 0.00012345678901234567890123456789,
   "market_cap": 12345678, "last_updated": "2026-09-15T14:05:00.000Z"},
  {"id": "stellar-synthetic-usd", "symbol": "susd", "current_price": 1.000123,
   "market_cap": 5000000},
  {"id": "cetes", "symbol": "cetes", "current_price": null,
   "market_cap": null, "last_updated": "2026-09-15T14:05:00.000Z"},
  {"id": "broken-listing", "symbol": "brk", "current_price": 7.5,
   "market_cap": 1, "last_updated": "2026-09-15T14:05:00.000Z"}
]`

// listingFixtureNow is the sync's clock for the fixtures above: 25
// minutes after the upstream stamped its prices.
var listingFixtureNow = time.Date(2026, 9, 15, 14, 30, 0, 0, time.UTC)

// TestParseCoinsList_KeepsStellarRowsAndCountsTheRest pins every census bucket individually; the counts are an operator's only evidence the parser still recognises the upstream.
func TestParseCoinsList_KeepsStellarRowsAndCountsTheRest(t *testing.T) {
	t.Parallel()

	entries, counts, err := parseListingCatalogue(strings.NewReader(listingCoinsFixture))
	if err != nil {
		t.Fatalf("parseListingCatalogue: %v", err)
	}

	if counts.Scanned != 7 {
		t.Errorf("Scanned = %d, want 7 (every element, Stellar-carrying or not)", counts.Scanned)
	}
	// bitcoin (no platforms) and empty-platform (empty string) are NOT
	// Stellar rows; broken-listing is, and is then skipped.
	if counts.Stellar != 5 {
		t.Errorf("Stellar = %d, want 5 (4 usable + 1 malformed)", counts.Stellar)
	}
	if counts.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the malformed address) — and the run must still succeed", counts.Skipped)
	}
	if counts.Contracts != 1 {
		t.Errorf("Contracts = %d, want 1", counts.Contracts)
	}
	if counts.Classic != 3 {
		t.Errorf("Classic = %d, want 3 (AQUA, sUSD, CETES)", counts.Classic)
	}
	if got := counts.Contracts + counts.Classic; got != len(entries) {
		t.Errorf("Contracts+Classic = %d but %d entries returned — the forms must account for every entry",
			got, len(entries))
	}

	byID := map[string]string{}
	for _, e := range entries {
		byID[e.ListingID] = e.Address
		if e.Source != listingSource {
			t.Errorf("entry %s source = %q, want %q", e.ListingID, e.Source, listingSource)
		}
		if e.PriceUSD != "" || !e.PricedAt.IsZero() {
			t.Errorf("entry %s arrived from the catalogue already priced (%q/%v) — "+
				"the catalogue carries no prices", e.ListingID, e.PriceUSD, e.PricedAt)
		}
	}
	for id, want := range map[string]string{
		"stellar":               "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		"aquarius":              "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
		"stellar-synthetic-usd": "sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH",
		"cetes":                 "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
	} {
		if byID[id] != want {
			t.Errorf("entry %s address = %q, want %q", id, byID[id], want)
		}
	}
	// The Solana address on the cetes row also begins with C. Reading any
	// platform key but `stellar` would put it in this table.
	for _, e := range entries {
		if strings.HasPrefix(e.Address, "CETES7CK") {
			t.Errorf("a non-Stellar platform address reached the entry set: %q", e.Address)
		}
	}
	for _, id := range []string{"bitcoin", "broken-listing", "empty-platform"} {
		if _, ok := byID[id]; ok {
			t.Errorf("%s must not produce an entry", id)
		}
	}
}

// TestClassifyListingAddress is the form table, including two live shapes that defeat obvious shortcuts.
func TestClassifyListingAddress(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		addr     string
		wantForm listingAddressForm
		wantOK   bool
	}{
		{"contract strkey", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA", listingFormContract, true}, // gitleaks:allow
		{"classic pair", "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", listingFormClassic, true},
		{"classic code with a lowercase letter", "sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH", listingFormClassic, true},
		{"classic code beginning with C", "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC", listingFormClassic, true},
		{"empty", "", listingFormUnknown, false},
		{"free text", "NOT-A-STELLAR-ADDRESS", listingFormUnknown, false},
		{"an EVM address", "0x834df4c1d8f51be24322e39e4766697be015512f", listingFormUnknown, false},
		{"a G account, which is an ISSUER and not an asset", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", listingFormUnknown, false},
		{"contract strkey one char short", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWM", listingFormUnknown, false}, // gitleaks:allow
		{"classic code past 12 chars", "THIRTEENCHARS-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", listingFormUnknown, false},
		{"base32 alphabet excludes 0 and 1", "C0S3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA", listingFormUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			form, ok := classifyListingAddress(tc.addr)
			if ok != tc.wantOK || form != tc.wantForm {
				t.Errorf("classifyListingAddress(%q) = (%v, %v), want (%v, %v)",
					tc.addr, form, ok, tc.wantForm, tc.wantOK)
			}
		})
	}
}

// TestParseCoinsList_RefusesEmptyParse: a catalogue with no Stellar rows means a changed layout or truncated fetch, so it must error rather than let ReplaceListingDirectory prune the table to nothing.
func TestParseCoinsList_RefusesEmptyParse(t *testing.T) {
	t.Parallel()

	_, _, err := parseListingCatalogue(strings.NewReader(
		`[{"id":"bitcoin","symbol":"btc","platforms":{}}]`))
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a refusal on 0 parsed entries", err)
	}
}

// TestParseCoinsList_MalformedDocumentIsFatal: a malformed address is skipped data, but a JSON decode failure leaves the stream unsynchronised and is fatal.
func TestParseCoinsList_MalformedDocumentIsFatal(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"id":"stellar"}`, // an object, not the documented array
		`[{"id":"stellar","platforms":{"stellar":"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"}},{"id":`,
	} {
		if _, _, err := parseListingCatalogue(strings.NewReader(body)); err == nil {
			t.Errorf("parseListingCatalogue(%.40q…) = nil error, want a decode failure", body)
		}
	}
}

// TestParseMarkets_RejectsAPriceWithNoPublicationTime: the 24-hour price bound uses the platform's `last_updated`, so a price without one is rejected, never stamped time.Now(), which would launder an old number as fresh.
func TestParseMarkets_RejectsAPriceWithNoPublicationTime(t *testing.T) {
	t.Parallel()

	prices, rejected, err := parseMarkets(strings.NewReader(listingMarketsFixture), listingFixtureNow)
	if err != nil {
		t.Fatalf("parseMarkets: %v", err)
	}

	if _, ok := prices["stellar-synthetic-usd"]; ok {
		t.Error("a price with no last_updated must be REJECTED, not carried with a fabricated time")
	}
	if rejected != 1 {
		t.Errorf("rejected = %d, want 1 — a discarded price record must be COUNTED, "+
			"or it is indistinguishable from a coin the platform never priced", rejected)
	}
	// A null price is unpriced, which is a normal state and NOT a
	// rejection — conflating the two would make the rejected counter
	// useless as a signal.
	if _, ok := prices["cetes"]; ok {
		t.Error("a null current_price must not produce a price")
	}
	if got, want := len(prices), 3; got != want {
		t.Errorf("len(prices) = %d, want %d (stellar, aquarius, broken-listing)", got, want)
	}
	p, ok := prices["stellar"]
	if !ok {
		t.Fatal("stellar must be priced")
	}
	if p.USD != "0.195274" {
		t.Errorf("stellar price = %q, want %q", p.USD, "0.195274")
	}
	wantAt := time.Date(2026, 9, 15, 14, 5, 0, 0, time.UTC)
	if !p.At.Equal(wantAt) {
		t.Errorf("stellar priced_at = %v, want %v (the PLATFORM's clock)", p.At, wantAt)
	}
}

// TestParseMarkets_RejectsAPublicationTimeInTheFuture: a future timestamp would keep a frozen price fresh past the 24-hour floor, so beyond clock skew it is rejected and COUNTED, never clamped to now.
func TestParseMarkets_RejectsAPublicationTimeInTheFuture(t *testing.T) {
	t.Parallel()

	skew := timescale.ListingPriceMaxFutureSkew
	stamp := func(d time.Duration) string {
		return listingFixtureNow.Add(d).Format(time.RFC3339)
	}
	body := `[
  {"id": "far-future", "current_price": 1.5, "last_updated": "2099-01-01T00:00:00Z"},
  {"id": "past-skew", "current_price": 2.5, "last_updated": "` + stamp(skew+time.Second) + `"},
  {"id": "at-skew", "current_price": 3.5, "last_updated": "` + stamp(skew) + `"},
  {"id": "an-hour-old", "current_price": 4.5, "last_updated": "` + stamp(-time.Hour) + `"}
]`
	prices, rejected, err := parseMarkets(strings.NewReader(body), listingFixtureNow)
	if err != nil {
		t.Fatalf("parseMarkets: %v", err)
	}
	for _, id := range []string{"far-future", "past-skew"} {
		if p, ok := prices[id]; ok {
			t.Errorf("%s: a last_updated past the skew allowance was kept (priced_at %v) — "+
				"it would count as fresh until the clock caught up with it", id, p.At)
		}
	}
	if rejected != 2 {
		t.Errorf("rejected = %d, want 2 (far-future, past-skew)", rejected)
	}
	if got, want := prices["at-skew"].At, listingFixtureNow.Add(skew); !got.Equal(want) {
		t.Errorf("at-skew priced_at = %v, want %v — clock skew inside the allowance "+
			"must not drop a price", got, want)
	}
	if got, want := prices["an-hour-old"].USD, "4.5"; got != want {
		t.Errorf("an-hour-old price = %q, want %q", got, want)
	}
}

// TestParseMarkets_CarriesThePriceLiteralWithNoFloatRoundTrip is the ADR-0003 wire guard: a float64 round trip yields a plausible but different decimal, so json.Number must keep the literal.
func TestParseMarkets_CarriesThePriceLiteralWithNoFloatRoundTrip(t *testing.T) {
	t.Parallel()

	const exact = "0.00012345678901234567890123456789"

	prices, _, err := parseMarkets(strings.NewReader(listingMarketsFixture), listingFixtureNow)
	if err != nil {
		t.Fatalf("parseMarkets: %v", err)
	}
	got := prices["aquarius"].USD
	if got != exact {
		t.Fatalf("aquarius price = %q, want the exact literal %q — the digits were "+
			"rewritten, which is what a float64 round-trip does", got, exact)
	}
	// The literal carries 29 significant digits; a float64 holds ~17. The
	// equality above is therefore only satisfiable by a value that never
	// became one.
	if len(strings.TrimLeft(got, "0.")) <= 17 {
		t.Fatalf("fixture price %q no longer has more digits than a float64 holds — "+
			"the test has gone vacuous", got)
	}
}

// TestApplyListingPrices_LeavesUnpricedEntriesUnpriced: no upstream price keeps an empty price and zero time (NULL/NULL per the table CHECK), and the returned count is the number actually filled.
func TestApplyListingPrices_LeavesUnpricedEntriesUnpriced(t *testing.T) {
	t.Parallel()

	entries, _, err := parseListingCatalogue(strings.NewReader(listingCoinsFixture))
	if err != nil {
		t.Fatalf("parseListingCatalogue: %v", err)
	}
	prices, _, err := parseMarkets(strings.NewReader(listingMarketsFixture), listingFixtureNow)
	if err != nil {
		t.Fatalf("parseMarkets: %v", err)
	}

	priced := applyListingPrices(entries, prices)
	if priced != 2 {
		t.Errorf("priced = %d, want 2 (stellar + aquarius; sUSD rejected, cetes unpriced)", priced)
	}
	filled := 0
	for _, e := range entries {
		hasPrice, hasTime := e.PriceUSD != "", !e.PricedAt.IsZero()
		if hasPrice != hasTime {
			t.Errorf("entry %s has price=%v time=%v — the two must travel together",
				e.ListingID, hasPrice, hasTime)
		}
		if hasPrice {
			filled++
		}
	}
	if filled != priced {
		t.Errorf("%d entries carry a price but the count reported %d", filled, priced)
	}
	for _, e := range entries {
		if e.ListingID == "stellar-synthetic-usd" && e.PriceUSD != "" {
			t.Error("the rejected price reached an entry")
		}
	}
}

// TestListingClient_SendsTheKeyInAHeaderNotTheQuery: a *url.Error embeds the request URL, so a key in the query would reach every failed-fetch log line.
func TestListingClient_SendsTheKeyInAHeaderNotTheQuery(t *testing.T) {
	proKey := config.CoinGeckoVenueConfig{APIKey: "pro-secret"}

	var gotHeader, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-cg-pro-api-key")
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, listingCoinsFixture)
	}))
	defer srv.Close()

	c := newListingClient(srv.URL, proKey)
	if c.authMode != "pro" {
		t.Errorf("authMode = %q, want pro", c.authMode)
	}
	if _, _, err := c.fetchStellarListings(context.Background()); err != nil {
		t.Fatalf("fetchStellarListings: %v", err)
	}
	if gotHeader != "pro-secret" {
		t.Errorf("x-cg-pro-api-key header = %q, want the key", gotHeader)
	}
	if strings.Contains(gotQuery, "pro-secret") || strings.Contains(gotQuery, "api_key") {
		t.Errorf("the key reached the query string (%q) — it leaks through *url.Error into logs", gotQuery)
	}
	if !strings.Contains(gotQuery, "include_platform=true") {
		t.Errorf("query = %q, want include_platform=true — without it no coin carries an address", gotQuery)
	}
}

// TestNewListingClient_HostFollowsTheKeyMode: a Pro key works only against the pro host (the public host answers HTTP 400 / 10010), so the host follows the key; an explicit -base-url still wins.
func TestNewListingClient_HostFollowsTheKeyMode(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		pro, demo, override     string
		wantBase, wantMode, hdr string
	}{
		{
			name: "pro key selects the pro host", pro: "k",
			wantBase: listingProBaseURL, wantMode: "pro", hdr: "x-cg-pro-api-key",
		},
		{
			name: "demo key selects the public host", demo: "k",
			wantBase: listingDemoBaseURL, wantMode: "demo", hdr: "x-cg-demo-api-key",
		},
		{
			name: "pro wins when both are set", pro: "k", demo: "d",
			wantBase: listingProBaseURL, wantMode: "pro", hdr: "x-cg-pro-api-key",
		},
		{
			name:     "no key is the public host, unauthenticated",
			wantBase: listingDemoBaseURL, wantMode: "none",
		},
		{
			name: "explicit base URL overrides the key-derived host", pro: "k",
			override: "https://mirror.example/", wantBase: "https://mirror.example",
			wantMode: "pro", hdr: "x-cg-pro-api-key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newListingClient(tc.override, config.CoinGeckoVenueConfig{APIKey: tc.pro, DemoAPIKey: tc.demo})
			if c.baseURL != tc.wantBase {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tc.wantBase)
			}
			if c.authMode != tc.wantMode {
				t.Errorf("authMode = %q, want %q", c.authMode, tc.wantMode)
			}
			if c.keyHeader != tc.hdr {
				t.Errorf("keyHeader = %q, want %q", c.keyHeader, tc.hdr)
			}
		})
	}
}

// TestListingClient_HTTPErrorIsFatal: a non-200 must not parse as an empty catalogue, which would refuse at best and prune at worst.
func TestListingClient_HTTPErrorIsFatal(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error_code":10010}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := newListingClient(srv.URL, config.CoinGeckoVenueConfig{})
	_, _, err := c.fetchStellarListings(context.Background())
	if err == nil {
		t.Fatal("non-200 fetch returned nil error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want the status code in the message", err)
	}
}

// TestFetchListingPrices_ChunksTheIDList: the endpoint caps a request at 250 ids, so a longer list is several fixed-question requests covering each id exactly once.
func TestFetchListingPrices_ChunksTheIDList(t *testing.T) {
	t.Parallel()

	const total = 260
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		ids = append(ids, "coin-"+strconv.Itoa(i))
	}

	var (
		mu       sync.Mutex
		seen     []string
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		got := strings.Split(r.URL.Query().Get("ids"), ",")
		seen = append(seen, got...)
		mu.Unlock()
		if len(got) > listingMarketsBatch {
			t.Errorf("request %d carried %d ids, over the %d cap", n, len(got), listingMarketsBatch)
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	c := newListingClient(srv.URL, config.CoinGeckoVenueConfig{})
	if _, _, err := c.fetchListingPrices(context.Background(), ids); err != nil {
		t.Fatalf("fetchListingPrices: %v", err)
	}
	if requests != 2 {
		t.Errorf("requests = %d, want 2 for 260 ids at a %d cap", requests, listingMarketsBatch)
	}
	if len(seen) != len(ids) {
		t.Fatalf("saw %d ids across the requests, want %d — chunking lost or duplicated members",
			len(seen), len(ids))
	}
	for i := range ids {
		if seen[i] != ids[i] {
			t.Fatalf("id %d = %q across the chunks, want %q", i, seen[i], ids[i])
		}
	}
}

// TestListingSync_RequiresConfigAndHTTPS covers the two flag validations that run first.
func TestListingSync_RequiresConfigAndHTTPS(t *testing.T) {
	if err := listingSync(nil); err == nil {
		t.Error("listing-sync with no -config must error")
	}
	err := listingSync([]string{"-config", "/nonexistent.toml", "-base-url", "http://plaintext.invalid"})
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Errorf("err = %v, want a refusal of a non-https -base-url", err)
	}
}

// runListingSyncCapturingStderr runs listingSync with os.Stderr
// redirected to a pipe and returns the run error plus everything written
// to stderr (where the write-gate banner lands).
func runListingSyncCapturingStderr(t *testing.T, args []string) (error, string) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	runErr := listingSync(args)
	_ = w.Close()
	os.Stderr = orig
	return runErr, <-done
}

// TestListingClient_RefusesARedirectThatWouldCarryTheKey pins the paid key to the dialled origin: Go strips only Authorization, WWW-Authenticate and Cookie across hosts, so x-cg-pro-api-key would be re-sent to whatever a 302 names (including an https to http downgrade).
func TestListingClient_RefusesARedirectThatWouldCarryTheKey(t *testing.T) {
	proKey := config.CoinGeckoVenueConfig{APIKey: "pro-secret"}

	var elsewhereHits atomic.Int32
	var elsewhereSawKey atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits.Add(1)
		if r.Header.Get("x-cg-pro-api-key") != "" {
			elsewhereSawKey.Store(true)
		}
		_, _ = io.WriteString(w, listingCoinsFixture)
	}))
	defer elsewhere.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	defer origin.Close()

	_, _, err := newListingClient(origin.URL, proKey).fetchStellarListings(context.Background())
	if err == nil {
		t.Fatal("a cross-host redirect was followed — the run must refuse it, not carry the key over")
	}
	if !strings.Contains(err.Error(), "refusing to follow a redirect") {
		t.Errorf("err = %v, want a refusal naming the redirect", err)
	}
	if elsewhereHits.Load() != 0 || elsewhereSawKey.Load() {
		t.Errorf("the redirect target was dialled %d time(s) and saw the key = %v; the API key must never leave the origin the run dialled",
			elsewhereHits.Load(), elsewhereSawKey.Load())
	}
}

// TestListingClient_FollowsASameOriginRedirect: the policy is that the key does not leave the origin, not no redirects.
func TestListingClient_FollowsASameOriginRedirect(t *testing.T) {
	proKey := config.CoinGeckoVenueConfig{APIKey: "pro-secret"}

	var gotKey string
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-cg-pro-api-key")
		_, _ = io.WriteString(w, listingCoinsFixture)
	})
	mux.HandleFunc(listingCataloguePath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/moved", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	entries, _, err := newListingClient(srv.URL, proKey).fetchStellarListings(context.Background())
	if err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	if len(entries) == 0 {
		t.Error("no entries from the moved resource")
	}
	if gotKey != "pro-secret" {
		t.Errorf("key at the same-origin hop = %q, want it carried", gotKey)
	}
}

func TestListingClient_RedirectPolicy(t *testing.T) {
	proKey := config.CoinGeckoVenueConfig{APIKey: "pro-secret"}
	assertKeyedRedirectPolicy(t, newListingClient("", proKey).http.CheckRedirect)
}

// TestListingClient_StopsASelfRedirectLoop: a same-origin self-redirect must end at the hop cap, not re-send the key until the 60 s timeout.
func TestListingClient_StopsASelfRedirectLoop(t *testing.T) {
	proKey := config.CoinGeckoVenueConfig{APIKey: "pro-secret"}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := newListingClient(srv.URL, proKey).fetchStellarListings(ctx)
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v, want the loop stopped at the hop cap", err)
	}
	if got := hits.Load(); got != 10 {
		t.Errorf("origin saw %d keyed requests, want 10", got)
	}
}
