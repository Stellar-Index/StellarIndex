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
)

// listingCoinsFixture is a trimmed copy of a real
// `/coins/list?include_platform=true` response (fetched 2026-09-15),
// cut to seven elements and kept byte-faithful in shape: the real field
// names, the real `platforms` objects with their other chains left in,
// and the `name` field this sync does not read.
//
// The seven are chosen so every path through parseListingCatalogue is exercised
// by a row that either occurs live or plausibly could:
//
//  1. bitcoin              — platforms is EMPTY. Skipped, and not
//     counted as a Stellar row at all.
//  2. stellar              — a contract C-strkey.
//  3. aquarius             — a classic CODE-GISSUER pair.
//  4. stellar-synthetic-usd — a classic code with a LOWERCASE letter
//     (sUSD). Live, and the row an
//     `[A-Z0-9]`-only code class would discard.
//  5. cetes                — a classic code BEGINNING WITH C, alongside
//     a Solana address that also begins with C.
//     Live, and the row a `LIKE 'C%'`-style
//     contract test misfiles.
//  6. broken-listing       — a malformed Stellar address. Skipped AND
//     counted; the run still succeeds.
//  7. empty-platform       — `platforms.stellar` present but empty,
//     which the upstream does emit. Not a
//     Stellar row and not a skip.
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

// listingMarketsFixture is a trimmed `/coins/markets` response for the
// ids above, again shaped like the real one (which carries ~26 fields
// per element; four are kept, plus one unread field to prove the decode
// ignores the rest).
//
// Three of the five rows are the cases that matter:
//
//   - stellar               — a normal priced row.
//   - aquarius              — a price with MORE SIGNIFICANT DIGITS than
//     a float64 holds. It must survive verbatim.
//   - stellar-synthetic-usd — a price with NO `last_updated`. REJECTED:
//     a price whose age cannot be verified is
//     not served, and no timestamp is invented
//     for it.
//   - cetes                 — `current_price: null`. Unpriced, which is
//     a normal state, NOT a rejection.
//   - broken-listing        — priced, but its address never made it into
//     the entry set, so the price has nothing to
//     attach to.
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

// TestParseCoinsList_KeepsStellarRowsAndCountsTheRest walks the fixture
// and pins every bucket of the census. The counts are the only evidence
// an operator has that the parser still recognises the upstream, so they
// are asserted individually rather than as "did it return something".
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

// TestClassifyListingAddress is the form table, including the two shapes
// that occur live and defeat the obvious shortcuts.
func TestClassifyListingAddress(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		addr     string
		wantForm listingAddressForm
		wantOK   bool
	}{
		{"contract strkey", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA", listingFormContract, true},
		{"classic pair", "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", listingFormClassic, true},
		{"classic code with a lowercase letter", "sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH", listingFormClassic, true},
		{"classic code beginning with C", "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC", listingFormClassic, true},
		{"empty", "", listingFormUnknown, false},
		{"free text", "NOT-A-STELLAR-ADDRESS", listingFormUnknown, false},
		{"an EVM address", "0x834df4c1d8f51be24322e39e4766697be015512f", listingFormUnknown, false},
		{"a G account, which is an ISSUER and not an asset", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA", listingFormUnknown, false},
		{"contract strkey one char short", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWM", listingFormUnknown, false},
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

// TestParseCoinsList_RefusesEmptyParse — a catalogue with no Stellar
// rows means the upstream layout changed or the fetch was truncated, not
// that a platform delisted ~50 assets inside an hour. It must error
// rather than hand ReplaceListingDirectory a set that prunes the table
// to nothing.
func TestParseCoinsList_RefusesEmptyParse(t *testing.T) {
	t.Parallel()

	_, _, err := parseListingCatalogue(strings.NewReader(
		`[{"id":"bitcoin","symbol":"btc","platforms":{}}]`))
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a refusal on 0 parsed entries", err)
	}
}

// TestParseCoinsList_MalformedDocumentIsFatal draws the other half of
// that line: a malformed ADDRESS is data and is skipped, but a JSON
// decode failure leaves the stream unsynchronised and nothing after it
// can be trusted.
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

// TestParseMarkets_RejectsAPriceWithNoPublicationTime is the core price
// rule. The storage layer's 24-hour price bound is measured on the
// PLATFORM's own `last_updated`; a price that arrives without one has no
// verifiable age, and inventing a timestamp for it — `time.Now()` being
// the obvious and wrong choice — would defeat that bound entirely and
// launder an arbitrarily old number as fresh.
func TestParseMarkets_RejectsAPriceWithNoPublicationTime(t *testing.T) {
	t.Parallel()

	prices, rejected, err := parseMarkets(strings.NewReader(listingMarketsFixture))
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

// TestParseMarkets_CarriesThePriceLiteralWithNoFloatRoundTrip is the
// ADR-0003 guard at the wire boundary. The fixture's aquarius price has
// far more significant digits than a float64 holds; decoding it through
// one and printing it back yields a DIFFERENT, entirely plausible-looking
// decimal, which is precisely why the loss is undetectable downstream.
// json.Number keeps the literal.
func TestParseMarkets_CarriesThePriceLiteralWithNoFloatRoundTrip(t *testing.T) {
	t.Parallel()

	const exact = "0.00012345678901234567890123456789"

	prices, _, err := parseMarkets(strings.NewReader(listingMarketsFixture))
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

// TestApplyListingPrices_LeavesUnpricedEntriesUnpriced — an entry with
// no upstream price keeps an empty price and a zero time, which is what
// the storage layer writes as NULL/NULL and what the table CHECK
// requires. The count returned must be the number actually filled.
func TestApplyListingPrices_LeavesUnpricedEntriesUnpriced(t *testing.T) {
	t.Parallel()

	entries, _, err := parseListingCatalogue(strings.NewReader(listingCoinsFixture))
	if err != nil {
		t.Fatalf("parseListingCatalogue: %v", err)
	}
	prices, _, err := parseMarkets(strings.NewReader(listingMarketsFixture))
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

// TestListingClient_SendsTheKeyInAHeaderNotTheQuery pins finding G10-04
// on this path. A transport error's *url.Error embeds the request URL in
// its message, so a key in the query string ends up in every log line
// that reports a failed fetch — and the failed fetches are exactly the
// ones that get logged.
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

// TestNewListingClient_HostFollowsTheKeyMode — a Pro key authenticates
// ONLY against the pro host (the public host answers a Pro key with HTTP
// 400 / error_code 10010), so the host has to follow the key rather than
// the operator having to know they move together. An explicit -base-url
// still wins.
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

// TestListingClient_HTTPErrorIsFatal — a non-200 must not be parsed as
// an empty catalogue, which would be a refusal at best and a prune at
// worst.
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

// TestFetchListingPrices_ChunksTheIDList — the markets endpoint caps a
// request at 250 ids, so a longer list must arrive as several requests
// each asking a FIXED question, and every id must be represented exactly
// once across them.
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

// TestListingSync_FailsClosedByDefault pins the ops write-gate
// convention for this command: it previews by DEFAULT and mutates
// Postgres only on an explicit -write, announced by a loud stderr
// banner. The automated hourly caller passes -write; nothing else
// should have to.
//
// The banner is emitted after flag validation and BEFORE config load or
// any network fetch, so a nonexistent config lets the run announce its
// mode and then error out without touching Postgres or the network.
func TestListingSync_FailsClosedByDefault(t *testing.T) {
	const cfg = "/nonexistent/stellarindex-listing-sync-gate-test.toml"

	_, stderrDefault := runListingSyncCapturingStderr(t, []string{"-config", cfg})
	if !strings.Contains(stderrDefault, "DRY RUN — no writes; pass -write to apply") {
		t.Errorf("default run must announce the fail-closed DRY RUN banner on stderr; got:\n%s", stderrDefault)
	}
	if strings.Contains(stderrDefault, "WRITING — applying changes") {
		t.Errorf("default run must NOT announce WRITING; got:\n%s", stderrDefault)
	}

	_, stderrWrite := runListingSyncCapturingStderr(t, []string{"-config", cfg, "-write"})
	if !strings.Contains(stderrWrite, "WRITING — applying changes") {
		t.Errorf("-write must announce the WRITING banner on stderr; got:\n%s", stderrWrite)
	}
	if strings.Contains(stderrWrite, "DRY RUN") {
		t.Errorf("-write must NOT report DRY RUN; got:\n%s", stderrWrite)
	}
}

// TestListingSync_RequiresConfigAndHTTPS — the two flag validations that
// run before anything else.
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

// TestListingClient_RefusesARedirectThatWouldCarryTheKey pins the paid
// key to the origin the run dialled.
//
// Go's redirect header copier strips ONLY Authorization,
// WWW-Authenticate and Cookie when a hop crosses hosts, so
// x-cg-pro-api-key is otherwise re-sent verbatim to whatever a 302
// names — a vendor redirect, a hijacked edge, or a mistyped -base-url,
// including an https:// -> http:// downgrade. Keeping the key out of
// the query string is only half of keeping it out of a stranger's logs.
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

// TestListingClient_FollowsASameOriginRedirect — the policy is "the key
// does not leave this origin", not "no redirects": a hop that keeps the
// scheme and host is still followed, carrying the key as before.
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

// TestListingClient_StopsASelfRedirectLoop — a same-origin 302 to itself
// must end at the hop cap, not re-send the key until the 60 s client
// timeout.
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
