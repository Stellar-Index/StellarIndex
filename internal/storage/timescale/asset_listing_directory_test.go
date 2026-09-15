// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Real addresses from the live upstream catalogue, kept verbatim because
// the classification tests below are about forms that actually occur —
// not about forms a test author found convenient to invent.
const (
	// A Soroban contract C-strkey (the native XLM SAC).
	testListingContract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	// A second contract, so the contract arm is never a set of one.
	testListingContract2 = "CB44W727WSLHPXJ47A6DHF5D34RKWSOZAMEDXO3CF5TEEEQ2ZX4V3VRI"
	// A classic CODE-GISSUER pair.
	testListingClassic = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	// A classic pair whose CODE BEGINS WITH C — the row that a
	// `LIKE 'C%'` contract test silently misfiles as a contract.
	testListingClassicCCode = "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	// A classic pair whose code carries a LOWERCASE letter — the row an
	// `[A-Z0-9]`-only code class rejects as malformed.
	testListingClassicLower = "sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH"
)

func listingEntriesN(n int) []ListingEntry {
	out := make([]ListingEntry, n)
	for i := range out {
		out[i] = ListingEntry{
			Address:   fmt.Sprintf("C%055d", i),
			ListingID: fmt.Sprintf("coin-%d", i),
			Symbol:    fmt.Sprintf("sym%d", i),
			PriceUSD:  "1.5",
			PricedAt:  time.Unix(1_700_000_000, 0).UTC(),
			Source:    "coingecko",
		}
	}
	return out
}

// TestBuildListingUpsert_PlaceholderLayout — 5 params per row plus
// exactly one shared source param at the tail position every row
// references. A drifted placeholder count corrupts column/value
// alignment silently (the driver reports nothing when the counts still
// happen to match), and here the misalignment would be between a price
// and a timestamp.
func TestBuildListingUpsert_PlaceholderLayout(t *testing.T) {
	t.Parallel()

	chunk := listingEntriesN(3)
	q, args := buildListingUpsert(chunk, "coingecko")

	if got, want := len(args), 3*5+1; got != want {
		t.Fatalf("len(args) = %d, want %d (5 per row + 1 shared source)", got, want)
	}
	if args[len(args)-1] != "coingecko" {
		t.Errorf("last arg = %v, want the shared source", args[len(args)-1])
	}
	// Every row must reference the shared source placeholder ($16 for a
	// 3-row chunk) in the pre-now() position — once per VALUES tuple.
	if got := strings.Count(q, "$16, now())"); got != 3 {
		t.Errorf("shared source placeholder used %d times, want 3 (once per row)", got)
	}
	// Highest per-row placeholder is $15; $17 must not exist.
	if strings.Contains(q, "$17") {
		t.Error("statement references $17 — placeholder math drifted past the arg list")
	}
	if !strings.Contains(q, "ON CONFLICT (address) DO UPDATE") {
		t.Error("statement lost its upsert arm")
	}
	// synced_at is now() per row — transaction-stable, which is what the
	// prune's "same source, older synced_at" depends on. A per-row
	// clock_timestamp() or a Go-side time would leave some rows of one
	// sync prunable by that same sync.
	if strings.Contains(q, "clock_timestamp()") {
		t.Error("synced_at must be transaction-stable now(), never clock_timestamp()")
	}
	// The price/time placeholders carry explicit casts. An untyped bind
	// parameter is left for Postgres to infer, which is the whole
	// 42883-class trap this tree has been bitten by.
	for _, want := range []string{"$4::numeric", "$5::timestamptz"} {
		if !strings.Contains(q, want) {
			t.Errorf("statement must bind %s explicitly; got:\n%s", want, q)
		}
	}
}

// TestBuildListingUpsert_PriceAndTimeTravelTogether — the writer must
// never bind a price without its publication time, nor a time without a
// price. A price with no verifiable age is exactly the value the 24h
// bound exists to refuse, and binding it with a NULL `priced_at` would
// store a row the price bound can never reject and the table CHECK would
// refuse outright.
func TestBuildListingUpsert_PriceAndTimeTravelTogether(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		entry     ListingEntry
		wantBound bool
	}{
		{
			name: "price with publication time is bound",
			entry: ListingEntry{
				Address: testListingContract, ListingID: "stellar",
				PriceUSD: "0.195274", PricedAt: time.Unix(1_700_000_000, 0).UTC(),
			},
			wantBound: true,
		},
		{
			name: "price with NO publication time is rejected, not fabricated",
			entry: ListingEntry{
				Address: testListingContract, ListingID: "stellar",
				PriceUSD: "0.195274", // PricedAt deliberately zero
			},
			wantBound: false,
		},
		{
			name: "publication time with no price is not bound either",
			entry: ListingEntry{
				Address: testListingContract, ListingID: "stellar",
				PricedAt: time.Unix(1_700_000_000, 0).UTC(),
			},
			wantBound: false,
		},
		{
			name: "neither",
			entry: ListingEntry{
				Address: testListingContract, ListingID: "stellar",
			},
			wantBound: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, args := buildListingUpsert([]ListingEntry{tc.entry}, "coingecko")
			price, pricedAt := args[3], args[4]
			gotPrice := boundIsNonNull(t, price)
			gotTime := boundIsNonNull(t, pricedAt)
			if gotPrice != tc.wantBound || gotTime != tc.wantBound {
				t.Errorf("bound (price=%v, priced_at=%v), want both %v — "+
					"a price and its publication time travel together or not at all",
					gotPrice, gotTime, tc.wantBound)
			}
		})
	}
}

// TestBuildListingUpsert_CarriesPriceAsDecimalString is the ADR-0003
// guard on this path: the upstream price is a JSON number, and any
// float64 anywhere between the wire and the NUMERIC column silently
// rewrites its low digits. The literal below has more significant digits
// than a float64 can hold, so a round-trip through one would show up
// here as changed text rather than as a value someone notices months
// later.
func TestBuildListingUpsert_CarriesPriceAsDecimalString(t *testing.T) {
	t.Parallel()

	const exact = "0.123456789012345678901234567890"
	_, args := buildListingUpsert([]ListingEntry{{
		Address: testListingContract, ListingID: "stellar",
		PriceUSD: exact, PricedAt: time.Unix(1_700_000_000, 0).UTC(),
	}}, "coingecko")

	got := fmt.Sprintf("%v", boundValue(t, args[3]))
	if got != exact {
		t.Fatalf("bound price = %q, want the exact decimal %q — a float64 round-trip "+
			"would have rewritten the tail digits", got, exact)
	}
}

// TestDedupListingEntriesByAddress — two upstream coin ids naming one
// Stellar address would put the same conflict key twice into one
// multi-row upsert chunk, which Postgres rejects ("ON CONFLICT DO UPDATE
// command cannot affect row a second time"), aborting the whole sync
// transaction. ReplaceListingDirectory must collapse them (last-wins)
// before chunking.
func TestDedupListingEntriesByAddress(t *testing.T) {
	t.Parallel()

	in := []ListingEntry{
		{Address: "A", ListingID: "first-a"},
		{Address: "B", ListingID: "only-b"},
		{Address: "A", ListingID: "second-a"}, // dup of A
		{Address: "C", ListingID: "only-c"},
		{Address: "B", ListingID: "second-b"}, // dup of B
	}
	got := dedupListingEntriesByAddress(in)

	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 distinct addresses (%+v)", len(got), got)
	}
	seen := map[string]int{}
	for _, e := range got {
		seen[e.Address]++
		if seen[e.Address] > 1 {
			t.Fatalf("address %s survived twice — the chunk would abort the upsert", e.Address)
		}
	}
	byAddr := map[string]ListingEntry{}
	for _, e := range got {
		byAddr[e.Address] = e
	}
	if byAddr["A"].ListingID != "second-a" {
		t.Errorf("A ListingID = %q, want last-wins %q", byAddr["A"].ListingID, "second-a")
	}
	if byAddr["B"].ListingID != "second-b" {
		t.Errorf("B ListingID = %q, want last-wins %q", byAddr["B"].ListingID, "second-b")
	}
	if got[0].Address != "A" || got[1].Address != "B" || got[2].Address != "C" {
		t.Errorf("order = %s,%s,%s, want A,B,C (first-appearance order preserved)",
			got[0].Address, got[1].Address, got[2].Address)
	}
}

// TestReplaceListingDirectory_RefusesEmptySet — an empty set means the
// fetch broke, not that the platform delisted every Stellar asset in one
// hour. Syncing it would prune the whole source and report success. Must
// error before touching the DB (s.db is nil here — a DB call would
// panic, so reaching the guard at all proves the order).
func TestReplaceListingDirectory_RefusesEmptySet(t *testing.T) {
	t.Parallel()

	s := &Store{}
	if _, _, err := s.ReplaceListingDirectory(context.Background(), nil, "coingecko"); err == nil {
		t.Fatal("ReplaceListingDirectory(empty) = nil error, want refusal")
	}
	if _, _, err := s.ReplaceListingDirectory(context.Background(), []ListingEntry{}, "coingecko"); err == nil {
		t.Fatal("ReplaceListingDirectory(empty slice) = nil error, want refusal")
	}
	if _, _, err := s.ReplaceListingDirectory(context.Background(), listingEntriesN(1), ""); err == nil {
		t.Fatal("ReplaceListingDirectory(no source) = nil error, want refusal")
	}
}

// TestListingDirectory_BothBoundsAreEnforcedInSQL pins the fail-closed
// half of the cache's contract, and the separation of the two clocks.
//
// Asserting the RENDERED predicates, not just the constants: a constant
// nothing splices in bounds nothing. That is the same reason
// TestAssetPriceSnapshot_StalenessBoundIsEnforcedInSQL asserts the
// listing's join text rather than the value of assetPriceSnapshotMaxAge.
func TestListingDirectory_BothBoundsAreEnforcedInSQL(t *testing.T) {
	t.Parallel()

	wantRecognition := "synced_at > now() - INTERVAL '" + listingRecognitionMaxAge + "'"
	wantPrice := "priced_at > now() - INTERVAL '" + listingPriceMaxAge + "'"

	for name, q := range map[string]string{
		"contracts read":  listingDirectoryContractsSQL,
		"by-address read": listingDirectoryByAddressSQL,
		"census":          listingDirectoryCensusSQL,
	} {
		if !strings.Contains(q, wantRecognition) {
			t.Errorf("%s: must bound recognition with %q — without it a dead sync's rows "+
				"are served as current recognition forever", name, wantRecognition)
		}
		if !strings.Contains(q, wantPrice) {
			t.Errorf("%s: must bound the price with %q", name, wantPrice)
		}
		// The price bound is measured on the PLATFORM's clock. A
		// `synced_at > now() - INTERVAL '<price bound>'` would mean a
		// successful sync of a FROZEN upstream price launders it as fresh.
		if strings.Contains(q, "synced_at > now() - INTERVAL '"+listingPriceMaxAge+"'") {
			t.Errorf("%s: the price bound is measured on priced_at (the platform's clock), "+
				"never on synced_at (ours)", name)
		}
		// A missing publication time is REJECTED, not waved through.
		if !strings.Contains(q, "priced_at IS NOT NULL") {
			t.Errorf("%s: a NULL priced_at must be rejected explicitly — unverifiable "+
				"freshness is the state a stale price is indistinguishable from", name)
		}
	}

	// The recognition bound belongs to the read's WHERE (it decides
	// whether the row exists for the reader); the price bound belongs to
	// the projected CASE (it decides only whether the row carries a
	// price). Swapping them would delete an address from the recognised
	// set every time its price went quiet.
	whereIdx := strings.Index(listingDirectoryContractsSQL, " WHERE ")
	if whereIdx < 0 {
		t.Fatal("contracts read has no WHERE clause")
	}
	if strings.Index(listingDirectoryContractsSQL, wantRecognition) < whereIdx {
		t.Error("the recognition bound must sit in the WHERE clause")
	}
	if strings.Index(listingDirectoryContractsSQL, wantPrice) > whereIdx {
		t.Error("the price bound must sit in the projected CASE, NOT the WHERE — in the " +
			"WHERE it drops a still-recognised address the moment its price goes stale")
	}
}

// TestListingBounds_AreParseableIntervalsAndCorrectlyOrdered — the two
// constants are spliced into SQL as literal intervals, so a typo is a
// runtime syntax error on a timer-driven read path rather than a compile
// failure. And their ORDER is the design: recognition (an identity
// mapping, human cadence) must be the wider of the two, price (a market
// number) the tighter. A price bound wider than the recognition bound
// would be unreachable — no row could ever be recognised long enough to
// exercise it.
func TestListingBounds_AreParseableIntervalsAndCorrectlyOrdered(t *testing.T) {
	t.Parallel()

	parse := func(name, v string) time.Duration {
		t.Helper()
		if regexp.MustCompile(`^(\d+) (minutes|hours)$`).FindStringSubmatch(v) == nil {
			t.Fatalf("%s = %q, want `<n> minutes` or `<n> hours`", name, v)
		}
		d, err := time.ParseDuration(strings.NewReplacer(
			" minutes", "m", " hours", "h").Replace(v))
		if err != nil {
			t.Fatalf("%s %q does not parse: %v", name, v, err)
		}
		return d
	}
	recognition := parse("listingRecognitionMaxAge", listingRecognitionMaxAge)
	price := parse("listingPriceMaxAge", listingPriceMaxAge)

	if recognition <= price {
		t.Errorf("recognition bound %s must be WIDER than the price bound %s — an "+
			"address→instrument mapping ages far more slowly than a price", recognition, price)
	}
	// The sync runs hourly. A recognition bound under a handful of missed
	// passes makes the admitted set flap on a single rate limit.
	const syncCadence = time.Hour
	if recognition < 12*syncCadence {
		t.Errorf("recognition bound %s is under 12 sync cadences (%s) — the admitted set "+
			"would flap on a bad afternoon rather than on a dead sync",
			recognition, 12*syncCadence)
	}
}

// TestListingAddressFormsSQL_AreExhaustiveAndNotPrefixTests is the
// misclassification guard.
//
// `LIKE 'C%'` looks like a perfectly good contract test and is not: a
// classic asset CODE may itself begin with C, and the live upstream
// carries two that do. The census would still balance while filing both
// under the wrong arm — the quiet kind of wrong.
//
// The predicates are Postgres POSIX regexes; the Go RE2 engine agrees
// with Postgres on these (anchors, character classes and bounded
// repetition only, no backreferences or lookaround), so compiling the
// same literal here tests the expression that actually ships.
func TestListingAddressFormsSQL_AreExhaustiveAndNotPrefixTests(t *testing.T) {
	t.Parallel()

	if strings.Contains(listingIsContractSQL, "LIKE") || strings.Contains(listingIsClassicSQL, "LIKE") {
		t.Fatal("address-form tests must be anchored regexes, not LIKE prefixes — " +
			"a classic code beginning with C (CETES-, C1USD-) defeats LIKE 'C%'")
	}
	reOf := func(pred string) *regexp.Regexp {
		t.Helper()
		start := strings.Index(pred, "'")
		end := strings.LastIndex(pred, "'")
		if start < 0 || end <= start {
			t.Fatalf("no regex literal in %q", pred)
		}
		return regexp.MustCompile(pred[start+1 : end])
	}
	contract, classic := reOf(listingIsContractSQL), reOf(listingIsClassicSQL)

	for _, tc := range []struct {
		addr     string
		wantForm string
	}{
		{testListingContract, "contract"},
		{testListingContract2, "contract"},
		{testListingClassic, "classic"},
		{testListingClassicCCode, "classic"}, // the LIKE 'C%' trap
		{testListingClassicLower, "classic"}, // the [A-Z0-9]-only trap
	} {
		isContract, isClassic := contract.MatchString(tc.addr), classic.MatchString(tc.addr)
		// Exhaustive AND disjoint: the census arithmetic
		// (Contracts + Classic == Entries) is only sound if every stored
		// address matches exactly one arm.
		if isContract == isClassic {
			t.Errorf("%s: contract=%v classic=%v — the two forms must be disjoint and "+
				"together exhaustive", tc.addr, isContract, isClassic)
			continue
		}
		gotForm := "classic"
		if isContract {
			gotForm = "contract"
		}
		if gotForm != tc.wantForm {
			t.Errorf("%s classified as %s, want %s", tc.addr, gotForm, tc.wantForm)
		}
	}
}

// TestListingDirectoryCensus_Check — the census publishes its own
// arithmetic, so the balance rule has to be the thing under test rather
// than a comment beside it.
func TestListingDirectoryCensus_Check(t *testing.T) {
	t.Parallel()

	balanced := ListingDirectoryCensus{Entries: 50, Contracts: 17, Classic: 33, Stale: 4, Priced: 15}
	if got := balanced.Check(); got != "" {
		t.Errorf("balanced census reported %q, want \"\"", got)
	}
	// Stale is deliberately OUTSIDE the Entries sum: it counts rows the
	// recognition bound removed, which are by definition not part of the
	// recognised population.
	if got := (ListingDirectoryCensus{Entries: 50, Contracts: 17, Classic: 32}).Check(); got == "" {
		t.Error("address forms that do not sum to Entries must be reported as unbalanced")
	}
	if got := (ListingDirectoryCensus{Entries: 50, Contracts: 17, Classic: 33, Priced: 18}).Check(); got == "" {
		t.Error("Priced exceeding Contracts must be reported — a priced row is a contract row")
	}
	if got := (ListingDirectoryCensus{Entries: 50, Contracts: 17, Classic: 33, PricedClassic: 34}).Check(); got == "" {
		t.Error("PricedClassic exceeding Classic must be reported — a priced classic row is a classic row")
	}
	// The two priced counts are over DISJOINT halves of the population,
	// so both may be at their own ceiling at once and the census still
	// balances. A Check that summed them against Entries would reject
	// the fully-priced table, which is a legitimate state.
	full := ListingDirectoryCensus{Entries: 50, Contracts: 17, Classic: 33, Priced: 17, PricedClassic: 33}
	if got := full.Check(); got != "" {
		t.Errorf("a fully-priced census reported %q, want \"\"", got)
	}
}

// TestListingDirectoryByAddressSQL_ServesBothForms — the by-address read
// exists BECAUSE the contract read cannot answer for a classic asset,
// and the one way to get it wrong is to copy the contract read's
// address-form predicate along with everything else.
//
// Measured against the live upstream 2026-09-15, six of the ten
// catalogue assets it names are named by their CLASSIC ids (EURC, AQUA,
// SHX, VELO, BLND, yUSDC). A read that kept the contract filter would
// drop every one of them and look like a working query while doing it.
func TestListingDirectoryByAddressSQL_ServesBothForms(t *testing.T) {
	t.Parallel()

	if strings.Contains(listingDirectoryByAddressSQL, listingIsContractSQL) {
		t.Error("the by-address read must NOT filter by address form — a classic " +
			"catalogue asset is named by its CODE-GISSUER id, and six live rows are")
	}
	if strings.Contains(listingDirectoryByAddressSQL, listingIsClassicSQL) {
		t.Error("the by-address read must NOT filter to classic rows either — four " +
			"live catalogue assets are named only by their SAC address")
	}
	// Its recognition bound still belongs in the WHERE and its price
	// bound still belongs in the projected CASE, for the same reasons
	// the contract read's do.
	whereIdx := strings.Index(listingDirectoryByAddressSQL, " WHERE ")
	if whereIdx < 0 {
		t.Fatal("by-address read has no WHERE clause")
	}
	priceIdx := strings.Index(listingDirectoryByAddressSQL,
		"priced_at > now() - INTERVAL '"+listingPriceMaxAge+"'")
	if priceIdx > whereIdx {
		t.Error("the price bound must sit in the projected CASE, NOT the WHERE — in the " +
			"WHERE it drops a still-recognised address the moment its price goes stale")
	}
}

// boundIsNonNull reports whether a bound argument will reach Postgres as
// a value rather than as NULL. It reads through [driver.Valuer] so the
// assertion is about what the DRIVER will send, not about which concrete
// null wrapper the writer happened to choose.
func boundIsNonNull(t *testing.T, arg any) bool {
	t.Helper()
	v, ok := arg.(driver.Valuer)
	if !ok {
		return arg != nil
	}
	got, err := v.Value()
	if err != nil {
		t.Fatalf("Value() on bound arg %#v: %v", arg, err)
	}
	return got != nil
}

// boundValue unwraps a bound argument to the value the driver will send.
func boundValue(t *testing.T, arg any) any {
	t.Helper()
	v, ok := arg.(driver.Valuer)
	if !ok {
		return arg
	}
	got, err := v.Value()
	if err != nil {
		t.Fatalf("Value() on bound arg %#v: %v", arg, err)
	}
	return got
}
