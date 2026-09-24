package v1_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/assets — C2's SECOND arm: an independent listing
// directory corroborating an in-repo curated binding.
//
// The arm exists because the curated account directory names NONE of
// the contract addresses this repository holds verified bindings for.
// Measured 2026-09-15: the directory names 387 contract addresses, the
// listing directory names 17 on Stellar, and four are in both — so a
// bound address was not refused by any requirement, it was never
// enumerated at all.
//
// Every test below is about the same thing from a different side: two
// sources are required, neither is sufficient, and the surface says
// which pair let a row in.

// The real addresses the two sources actually agree on, and the real
// ones they do not. Unlike the invented C-strkeys elsewhere in this
// package these are not interchangeable fixtures — the whole claim of
// arm 2 is that two parties who do not read each other produced the
// same 56 characters, and a made-up address cannot express it.
const (
	// Bound in-repo AND named by the listing directory.
	rwaListedBoundEUTBL = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	// Bound in-repo, NOT named by the listing directory. The control.
	rwaBoundNotListedUSTBL = "CARUUX2FZNPH6DGJOEUFSIUQWYHNL5AVDV7PMVSHWL7OBYIBFC76F4TO"
	// Named by the listing directory, bound by NOBODY. The native
	// asset's own Stellar Asset Contract — a real entry in the same
	// map. The negative control, and a permanent one: XLM can never
	// acquire a curated RWA binding, so no later admission can retire
	// this test the way binding the tokenized-gold contract that used
	// to stand here did.
	rwaListedNotBoundXLM = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
)

// stubRWAListings serves a canned listing-directory read. It censuses
// what it serves the way the real query does, so a funnel assertion
// runs against an accounting that closes rather than invented numbers.
type stubRWAListings struct {
	rows []timescale.ListingEntry
	// stale is the count the storage layer refused to serve as past the
	// recognition bound — the visible form of the fail-closed shrink.
	stale int
	// census, when set, is served verbatim instead of being derived
	// from rows. The derivation below counts every served row as a
	// CONTRACT row, which is true of every fixture that serves rows and
	// cannot express the one state that serves none while being
	// perfectly healthy: a directory holding only classic rows.
	census *timescale.ListingDirectoryCensus
	err    error
}

func (s *stubRWAListings) ListingDirectoryContracts(
	context.Context,
) ([]timescale.ListingEntry, timescale.ListingDirectoryCensus, error) {
	if s.err != nil {
		return nil, timescale.ListingDirectoryCensus{}, s.err
	}
	if s.census != nil {
		return s.rows, *s.census, nil
	}
	c := timescale.ListingDirectoryCensus{
		Contracts: len(s.rows),
		Stale:     s.stale,
	}
	for _, r := range s.rows {
		c.Entries++
		if r.PriceUSD != "" {
			c.Priced++
		}
	}
	return s.rows, c, nil
}

// listed builds a listing row with a fresh price.
func listed(addr, id, symbol, price string) timescale.ListingEntry {
	return timescale.ListingEntry{
		Address:   addr,
		ListingID: id,
		Symbol:    symbol,
		PriceUSD:  price,
		PricedAt:  time.Now().Add(-5 * time.Minute),
		Source:    "coingecko",
	}
}

// rwaListingServer wires BOTH contract-side readers, with the curated
// directory deliberately naming nothing: anything served is arm 2's
// doing and could not have come from arm 1.
func rwaListingServer(
	t *testing.T,
	listings v1.RWAListingDirectoryReader,
	rows map[string]timescale.AssetRow,
	supplies map[string]string,
	decimals map[string]uint32,
	dir map[string]timescale.DirectoryEntry,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
		// Named by NOBODY in the curated directory — the live state for
		// every bound address on 2026-09-15.
		RWAContracts:          &stubRWAContractReader{accounts: 18000},
		RWAListings:           listings,
		ContractCatalogue:     &stubContractCatalogue{rows: rows},
		TokenSymbol:           &stubTokenSymbols{byID: map[string]string{}},
		TokenSupply:           &stubTokenSupplies{byID: supplies},
		TokenDecimals:         &stubTokenDecimalsRdr{byID: decimals},
		MinMarketCapVolumeUSD: 1000,
	})
}

// assetByContract finds a served row by contract address.
func assetByContract(v v1.RWAAssetsView, id string) (v1.RWAAsset, bool) {
	for _, a := range v.Assets {
		if a.ContractID == id {
			return a, true
		}
	}
	return v1.RWAAsset{}, false
}

// stageCount returns one funnel stage's count.
func stageCount(t *testing.T, v v1.RWAAssetsView, stage string) int {
	t.Helper()
	for _, s := range v.Funnel.Stages {
		if s.Stage == stage {
			return s.Count
		}
	}
	t.Fatalf("funnel has no stage %q", stage)
	return 0
}

// dropCount returns one drop's count from a named stage, or 0.
func dropCount(v v1.RWAAssetsView, stage, reason string) int {
	for _, s := range v.Funnel.Stages {
		if s.Stage != stage {
			continue
		}
		for _, d := range s.Dropped {
			if d.Reason == reason {
				return d.Count
			}
		}
	}
	return 0
}

// TestRWAListing_CorroboratedBindingIsAdmittedAndValued is the unlock,
// end to end. The curated directory names nothing; the listing names
// the address; an in-repo binding names the same address. The row is
// served, says which pair admitted it, and carries a reference
// valuation computed at the LISTING price.
func TestRWAListing_CorroboratedBindingIsAdmittedAndValued(t *testing.T) {
	view := getRWA(t, rwaListingServer(t,
		&stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		},
		// The real measured supply, at the real declared scale.
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		map[string]uint32{rwaListedBoundEUTBL: 5},
		map[string]timescale.DirectoryEntry{},
	))

	a, ok := assetByContract(view, rwaListedBoundEUTBL)
	if !ok {
		t.Fatalf("the corroborated binding was not served; refusals: %+v", view.Refused)
	}
	if a.Recognition != rwa.RecognitionListingCorroborated {
		t.Errorf("recognition = %q, want %q", a.Recognition, rwa.RecognitionListingCorroborated)
	}
	if a.Basis != rwa.BasisCuratedContract {
		t.Errorf("basis = %q, want %q", a.Basis, rwa.BasisCuratedContract)
	}
	if a.Decimals == nil || *a.Decimals != 5 {
		t.Fatalf("decimals = %s, want 5 — the exponent is the valuation", decimalsText(a.Decimals))
	}
	// The name comes from the in-repo curated binding, never from the
	// listing platform's display text — that source corroborates the
	// address, it does not attest to an identity.
	if a.Name != "Spiko EU T-Bills Money Market Fund (EUTBL)" {
		t.Errorf("name = %q, want the curated binding's instrument", a.Name)
	}
	if a.Reference == nil {
		t.Fatal("no reference on a listing-corroborated row")
	}
	if a.Reference.Provenance != v1.RWAReferenceListingPrice {
		t.Errorf("provenance = %q, want %q", a.Reference.Provenance, v1.RWAReferenceListingPrice)
	}
	if a.Reference.Feed != "eutbl" || a.Reference.Source != "coingecko" {
		t.Errorf("reference does not name its publisher and key: %+v", a.Reference)
	}
	// 28327867109034 / 10^5 * 1.22 = 345,599,978.73.
	if a.ReferenceValuation.ValueUSD == nil {
		t.Fatalf("no reference valuation: status %q", a.ReferenceValuation.Status)
	}
	if got := *a.ReferenceValuation.ValueUSD; got != "345599978.73" {
		t.Errorf("reference_valuation.value_usd = %q, want %q", got, "345599978.73")
	}
	// R-C on the new arm: a reference WITHOUT a premium, which is the
	// one place the two fields legitimately diverge.
	if a.Premium.Status != v1.RWAPremiumReferenceNotOracle {
		t.Errorf("premium.status = %q, want %q", a.Premium.Status, v1.RWAPremiumReferenceNotOracle)
	}
	if a.Premium.Pct != nil {
		t.Errorf("a premium was computed against a listing price: %q", *a.Premium.Pct)
	}
}

// TestRWAListing_DecimalsAreReadNotAssumed is the 10^n guard, stated as
// a test because a wrong exponent on this surface is not a display
// defect — it is the published money figure, wrong by two orders of
// magnitude.
//
// The same supply and the same price are valued at the contract's real
// scale of 5 and at the catalogue default of 7, and the two must differ
// by exactly 100x. If the valuation ever stops reading the contract's
// own decimals, both cases return the same number and this fails.
func TestRWAListing_DecimalsAreReadNotAssumed(t *testing.T) {
	value := func(dec uint32) string {
		t.Helper()
		view := getRWA(t, rwaListingServer(t,
			&stubRWAListings{rows: []timescale.ListingEntry{
				listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
			}},
			map[string]timescale.AssetRow{
				rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
			},
			map[string]string{rwaListedBoundEUTBL: "28327867109034"},
			map[string]uint32{rwaListedBoundEUTBL: dec},
			map[string]timescale.DirectoryEntry{},
		))
		a, ok := assetByContract(view, rwaListedBoundEUTBL)
		if !ok || a.ReferenceValuation.ValueUSD == nil {
			t.Fatalf("decimals %d: no valuation served", dec)
		}
		return *a.ReferenceValuation.ValueUSD
	}
	at5, at7 := value(5), value(7)
	if at5 == at7 {
		t.Fatalf("decimals are not being read: 5dp and 7dp both value at %s", at5)
	}
	if at5 != "345599978.73" || at7 != "3455999.79" {
		t.Errorf("valuation does not scale by 10^decimals: 5dp=%s 7dp=%s", at5, at7)
	}
}

// TestRWAListing_ListingAloneAdmitsNothing is the negative control the
// whole arm rests on, run against a REAL address the listing directory
// names and this repository holds no binding for.
//
// It must be absent from the set, and it must be VISIBLE — counted in
// the terminal census that makes "a listing admits nothing alone" an
// auditable claim rather than an assertion.
func TestRWAListing_ListingAloneAdmitsNothing(t *testing.T) {
	view := getRWA(t, rwaListingServer(t,
		&stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedNotBoundXLM, "stellar", "xlm", "0.175549"),
		}},
		map[string]timescale.AssetRow{
			rwaListedNotBoundXLM: rwaContractRow(rwaListedNotBoundXLM, sptr("4283.35")),
		},
		map[string]string{rwaListedNotBoundXLM: "1060884000000"},
		map[string]uint32{rwaListedNotBoundXLM: 8},
		map[string]timescale.DirectoryEntry{},
	))
	if _, ok := assetByContract(view, rwaListedNotBoundXLM); ok {
		t.Fatal("a listing entry alone admitted a contract — C2 has been replaced by `a price aggregator has heard of it`")
	}
	if n := stageCount(t, view, "listing_contracts_without_curated_binding"); n != 1 {
		t.Errorf("listed-but-unbound census = %d, want 1 — the refusal is invisible", n)
	}
}

// TestRWAListing_BoundButUnlistedStaysRefused pins the other control.
// A binding on identical evidence to the admitted one, which the
// listing directory does not name, is refused — and refused under a
// reason that names the missing half rather than a generic failure.
func TestRWAListing_BoundButUnlistedStaysRefused(t *testing.T) {
	view := getRWA(t, rwaListingServer(t,
		&stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL:    rwaContractRow(rwaListedBoundEUTBL, nil),
			rwaBoundNotListedUSTBL: rwaContractRow(rwaBoundNotListedUSTBL, nil),
		},
		map[string]string{
			rwaListedBoundEUTBL:    "28327867109034",
			rwaBoundNotListedUSTBL: "3296465979305",
		},
		map[string]uint32{rwaListedBoundEUTBL: 5, rwaBoundNotListedUSTBL: 5},
		map[string]timescale.DirectoryEntry{},
	))
	if _, ok := assetByContract(view, rwaBoundNotListedUSTBL); ok {
		t.Fatal("an in-repo binding nobody independent named was admitted — the index is vouching for itself")
	}
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractCuratedNotListed); n == 0 {
		t.Error("the unlisted binding is not reported in the funnel")
	}
	// And the one the listing DID name is in, so the refusal above is a
	// finding about that address rather than the arm failing wholesale.
	if _, ok := assetByContract(view, rwaListedBoundEUTBL); !ok {
		t.Error("the corroborated binding was refused alongside the uncorroborated one")
	}
}

// TestRWAListing_ScamFlagBeatsCorroboration is the precedence test at
// the response level. The listing carries no flags and never will, so
// if the scam check did not cover this arm an address the curated
// directory named ONLY to flag as malicious would be served with a
// nine-figure valuation.
func TestRWAListing_ScamFlagBeatsCorroboration(t *testing.T) {
	for _, tag := range timescale.DirectoryScamFlagTags {
		view := getRWA(t, rwaListingServer(t,
			&stubRWAListings{rows: []timescale.ListingEntry{
				listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
			}},
			map[string]timescale.AssetRow{
				rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
			},
			map[string]string{rwaListedBoundEUTBL: "28327867109034"},
			map[string]uint32{rwaListedBoundEUTBL: 5},
			map[string]timescale.DirectoryEntry{
				rwaListedBoundEUTBL: {
					Address: rwaListedBoundEUTBL, Tags: []string{"issuer", tag},
					Source: "stellar-expert",
				},
			},
		))
		if _, ok := assetByContract(view, rwaListedBoundEUTBL); ok {
			t.Fatalf("tag %q: a scam-flagged address was served through the listing arm", tag)
		}
		if n := dropCount(view, "listing_corroborated_contracts", rwa.RejectContractScam); n != 1 {
			t.Errorf("tag %q: the scam refusal is not reported on the listing arm", tag)
		}
	}
}

// TestRWAListing_UnavailableShrinksTheSetAndSaysSo is the fail-closed
// guarantee, and the reason it is a test rather than a comment: a set
// that silently shrinks is worse than one that says why.
//
// The listing reader is wired and FAILS. Nothing is admitted, every
// binding is refused under the outage reason rather than under a
// finding about the data, and the funnel basis states it in prose.
func TestRWAListing_UnavailableShrinksTheSetAndSaysSo(t *testing.T) {
	view := getRWA(t, rwaListingServer(t,
		&stubRWAListings{err: errors.New("listing read failed")},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		map[string]uint32{rwaListedBoundEUTBL: 5},
		map[string]timescale.DirectoryEntry{},
	))
	if _, ok := assetByContract(view, rwaListedBoundEUTBL); ok {
		t.Fatal("a row was admitted while the listing read was failing — recognition was carried forward")
	}
	bindings := stageCount(t, view, "curated_contract_bindings")
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractListingUnavailable); n != bindings {
		t.Errorf("outage drop = %d, want every one of the %d bindings", n, bindings)
	}
	// Reported as an OUTAGE, never as a finding that nobody names them.
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractCuratedNotListed); n != 0 {
		t.Errorf("a failed read reported %d addresses as unnamed — a read that did not answer made a finding", n)
	}
	if !strings.Contains(view.Funnel.Basis, "independent_listing_unavailable") {
		t.Error("the funnel basis does not say the set is smaller because the listing did not answer")
	}
}

// TestRWAListing_EntirelyStaleSnapshotIsAnOutageNotAFinding is the
// fail-closed guarantee in its subtlest form.
//
// `synced_at` is stamped once per sync transaction, so every row ages
// out together: a table that has gone past the recognition bound
// arrives from the reader as ZERO rows beside a non-zero stale count —
// shaped exactly like a listing that genuinely names nothing.
//
// Those are not the same statement. Treated as a populated snapshot,
// every binding would be refused under a finding about the world when
// the truth is that our copy expired.
func TestRWAListing_EntirelyStaleSnapshotIsAnOutageNotAFinding(t *testing.T) {
	view := getRWA(t, rwaListingServer(t,
		// The shape a fully-aged-out table produces: nothing fresh to
		// serve, and the count of what was dropped for being stale.
		&stubRWAListings{rows: nil, stale: 17},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		map[string]uint32{rwaListedBoundEUTBL: 5},
		map[string]timescale.DirectoryEntry{},
	))
	if _, ok := assetByContract(view, rwaListedBoundEUTBL); ok {
		t.Fatal("an aged-out snapshot still admitted a row")
	}
	bindings := stageCount(t, view, "curated_contract_bindings")
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractListingUnavailable); n != bindings {
		t.Errorf("stale-snapshot drop = %d, want every one of the %d bindings", n, bindings)
	}
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractCuratedNotListed); n != 0 {
		t.Errorf("an expired snapshot reported %d addresses as named by nobody — an outage published as a finding", n)
	}
}

// TestRWAListing_UnwiredIsNotMeasured draws the distinction this whole
// surface is built on. No reader wired means nobody looked, which is
// not the same finding as nobody agreeing — so the arm reports itself
// unmeasured and makes no refusals at all.
func TestRWAListing_UnwiredIsNotMeasured(t *testing.T) {
	view := getRWA(t, rwaListingServer(t,
		nil,
		map[string]timescale.AssetRow{},
		map[string]string{},
		map[string]uint32{},
		map[string]timescale.DirectoryEntry{},
	))
	for _, s := range view.Funnel.Stages {
		if s.Arm == "listing" {
			t.Fatalf("an unwired listing reader produced a funnel stage %q — a scan that never ran made a finding", s.Stage)
		}
	}
	if !strings.Contains(view.Funnel.Basis, "NOT MEASURED") {
		t.Error("the funnel basis does not say the listing arm was not measured")
	}
	for _, r := range view.Refused {
		if r.Reason == rwa.RejectContractCuratedNotListed {
			t.Error("an unwired reader reported bindings as unnamed by anybody")
		}
	}
}

// TestRWAListing_BasisDoesNotInheritTheOracleWording is the prose guard.
//
// The published basis string previously said the figure rests on an
// oracle's valuation of the instrument plus the issuer's declaration
// that one token is one unit. A listing price is neither of those
// things, and a total that quietly inherited that sentence would be
// making a claim about provenance that is simply false.
func TestRWAListing_BasisDoesNotInheritTheOracleWording(t *testing.T) {
	view := getRWA(t, rwaListingServer(t,
		&stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		map[string]uint32{rwaListedBoundEUTBL: 5},
		map[string]timescale.DirectoryEntry{},
	))
	sum := view.Summary.ReferenceValuation
	basis := sum.Basis

	// The total is entirely listing-priced, so it must NOT claim an
	// oracle valued the instrument or that an issuer declared a
	// one-for-one correspondence.
	for _, forbidden := range []string{
		"what an independent oracle says one unit",
		"one token is one unit of that instrument",
		"nobody was seen paying it",
	} {
		if strings.Contains(strings.ToLower(basis), strings.ToLower(forbidden)) {
			t.Errorf("an all-listing total inherited the oracle wording %q: %s", forbidden, basis)
		}
	}
	for _, want := range []string{
		"listing_platform_price",
		"asserts NOTHING about what stands behind it",
		"No premium",
	} {
		if !strings.Contains(basis, want) {
			t.Errorf("the basis does not state %q: %s", want, basis)
		}
	}
	// And the mixture is visible without walking every row.
	if len(sum.Provenances) != 1 || sum.Provenances[0] != v1.RWAReferenceListingPrice {
		t.Errorf("summary.provenances = %v, want just the listing price", sum.Provenances)
	}
	if len(sum.Sources) != 1 || sum.Sources[0] != "coingecko" {
		t.Errorf("summary.sources = %v, want the listing publisher", sum.Sources)
	}
}

// TestRWAListing_StalePriceIsNotServedAsFresh pins the price bound at
// the surface as well as in SQL. A listing row whose own published
// timestamp is older than the absolute outer bound carries no
// valuation, and says which rule refused it.
func TestRWAListing_StalePriceIsNotServedAsFresh(t *testing.T) {
	old := listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22")
	old.PricedAt = time.Now().Add(-30 * 24 * time.Hour)
	view := getRWA(t, rwaListingServer(t,
		&stubRWAListings{rows: []timescale.ListingEntry{old}},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		map[string]uint32{rwaListedBoundEUTBL: 5},
		map[string]timescale.DirectoryEntry{},
	))
	a, ok := assetByContract(view, rwaListedBoundEUTBL)
	if !ok {
		t.Fatal("a stale PRICE removed the row from the set; it should refuse the valuation, not the membership")
	}
	if a.ReferenceValuation.ValueUSD != nil {
		t.Fatalf("a month-old price was multiplied by a float: %s", *a.ReferenceValuation.ValueUSD)
	}
	if a.ReferenceValuation.Status != v1.RWAPremiumReferenceExpired {
		t.Errorf("status = %q, want %q", a.ReferenceValuation.Status, v1.RWAPremiumReferenceExpired)
	}
}

// TestRWAListing_FunnelClosesWithBothArms is the accounting guard. The
// two contract-side arms narrow different populations and each has to
// reconcile on its own; the served set is the sum of all three arms.
func TestRWAListing_FunnelClosesWithBothArms(t *testing.T) {
	view := getRWA(t, v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
		RWAContracts: &stubRWAContractReader{
			contracts: []timescale.DirectoryEntry{
				recognisedContract(rwaContractGood, "Example Treasury Fund"),
			},
			accounts: 18000,
		},
		RWAListings: &stubRWAListings{
			rows: []timescale.ListingEntry{
				listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
				listed(rwaListedNotBoundXLM, "stellar", "xlm", "0.175549"),
			},
			stale: 3,
		},
		ContractCatalogue: &stubContractCatalogue{rows: map[string]timescale.AssetRow{
			rwaContractGood:     rwaContractRow(rwaContractGood, sptr("1.07")),
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		}},
		TokenSymbol: &stubTokenSymbols{byID: map[string]string{rwaContractGood: "USTRY"}},
		TokenSupply: &stubTokenSupplies{byID: map[string]string{
			rwaContractGood:     "10000000000",
			rwaListedBoundEUTBL: "28327867109034",
		}},
		TokenDecimals: &stubTokenDecimalsRdr{byID: map[string]uint32{
			rwaContractGood: 7, rwaListedBoundEUTBL: 5,
		}},
		MinMarketCapVolumeUSD: 1000,
	}))

	checkFunnelArithmetic(t, view)
	if len(view.Assets) != 2 {
		t.Fatalf("served %d assets, want one from each contract-side arm", len(view.Assets))
	}
	if n := stageCount(t, view, "listing_contract_assets_served"); n != 1 {
		t.Errorf("listing arm served %d, want 1", n)
	}
	if n := stageCount(t, view, "contract_assets_served"); n != 1 {
		t.Errorf("directory arm served %d, want 1", n)
	}
	// Both arms are present, so their rows must be distinguishable by
	// the evidence that admitted them rather than only by address.
	seen := map[string]int{}
	for _, a := range view.Assets {
		seen[a.Recognition]++
	}
	if seen[rwa.RecognitionCuratedDirectory] != 1 || seen[rwa.RecognitionListingCorroborated] != 1 {
		t.Errorf("recognition sources on the served set = %v, want one of each", seen)
	}
}

// TestRWAListing_DirectoryArmWinsWhenBothNameIt pins the dedup. Four
// addresses really are in both sources, and evaluating one twice would
// serve it twice — which is how a double-counted valuation ships.
//
// The directory arm keeps it, because its recognition is the stronger
// of the two and admits on its own.
func TestRWAListing_DirectoryArmWinsWhenBothNameIt(t *testing.T) {
	view := getRWA(t, v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
		RWAContracts: &stubRWAContractReader{
			contracts: []timescale.DirectoryEntry{
				recognisedContract(rwaListedBoundEUTBL, "Spiko EU T-Bills"),
			},
			accounts: 18000,
		},
		RWAListings: &stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		ContractCatalogue: &stubContractCatalogue{rows: map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		}},
		TokenSymbol:   &stubTokenSymbols{byID: map[string]string{}},
		TokenSupply:   &stubTokenSupplies{byID: map[string]string{rwaListedBoundEUTBL: "28327867109034"}},
		TokenDecimals: &stubTokenDecimalsRdr{byID: map[string]uint32{rwaListedBoundEUTBL: 5}},

		MinMarketCapVolumeUSD: 1000,
	}))

	n := 0
	for _, a := range view.Assets {
		if a.ContractID == rwaListedBoundEUTBL {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("an address both sources name was served %d times", n)
	}
	a, _ := assetByContract(view, rwaListedBoundEUTBL)
	if a.Recognition != rwa.RecognitionCuratedDirectory {
		t.Errorf("recognition = %q, want the stronger arm %q", a.Recognition, rwa.RecognitionCuratedDirectory)
	}
	if n := dropCount(view, "curated_contract_bindings", "contract_already_evaluated_by_directory_arm"); n != 1 {
		t.Errorf("the dedup is not reported in the funnel (got %d)", n)
	}
	checkFunnelArithmetic(t, view)
}

// TestRWAListing_UnreadDecimalsPublishNoFigure is the 10^n guard in the
// form that would actually have shipped.
//
// The API binary dials ClickHouse TWICE — once for the supply reader and
// once for the explorer reader that resolves decimals — and each failure
// is a warning that leaves the other wired. "Supply up, decimals down"
// is therefore a reachable process state, not a hypothetical, and in it
// every contract row keeps the catalogue's hardcoded 7.
//
// For a 5-decimal fund that publishes ONE HUNDREDTH of its
// capitalisation: $3,455,999.79 against a real $345,599,978.73, under
// `status: published`, with `decimals: 7` served as though it were a
// reading and the funnel counting the row as successfully valued.
//
// The same silent default is reachable with every reader up: an instance
// missing from the lake, a METADATA map declaring no scale, or a
// contract declaring both spellings with different values. Refusing is
// the only honest answer — the supply is still served, because that is a
// chain fact and needs no scale to be true.
func TestRWAListing_UnreadDecimalsPublishNoFigure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decimals map[string]uint32
	}{
		// The reader answers for no address — an unwired or failing
		// explorer reader beside a working supply reader.
		{"decimals reader answers for nothing", map[string]uint32{}},
		// The reader is up and this contract's scale is simply not
		// derivable: no instance in the lake, no scale in METADATA, or
		// two contradictory declarations.
		{"this contract has no readable scale", map[string]uint32{"CCCCCC": 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := getRWA(t, rwaListingServer(t,
				&stubRWAListings{rows: []timescale.ListingEntry{
					listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
				}},
				map[string]timescale.AssetRow{
					rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, sptr("1.22")),
				},
				map[string]string{rwaListedBoundEUTBL: "28327867109034"},
				tc.decimals,
				map[string]timescale.DirectoryEntry{},
			))
			a, ok := assetByContract(view, rwaListedBoundEUTBL)
			if !ok {
				t.Fatal("an unread scale removed the row from the set; it refuses the VALUATION, not the membership")
			}
			if a.ReferenceValuation.ValueUSD != nil {
				t.Errorf("published %s against an exponent nobody read — the 5dp fund at the default 7 is 100x low",
					*a.ReferenceValuation.ValueUSD)
			}
			if a.ReferenceValuation.Status != v1.RWAReferenceValuationDecimalsUnknown {
				t.Errorf("reference_valuation.status = %q, want %q",
					a.ReferenceValuation.Status, v1.RWAReferenceValuationDecimalsUnknown)
			}
			if a.Valuation.MarketCapUSD != nil {
				t.Errorf("market_cap_usd %s was computed against an unread exponent", *a.Valuation.MarketCapUSD)
			}
			if a.Valuation.Status != v1.RWAValuationDecimalsUnknown {
				t.Errorf("valuation.status = %q, want %q", a.Valuation.Status, v1.RWAValuationDecimalsUnknown)
			}
			// The supply is a chain fact and needs no scale to be true,
			// so it is still served — withholding it would lose a
			// verifiable number to protect a derived one.
			if a.CirculatingSupply == nil || *a.CirculatingSupply != "28327867109034" {
				t.Errorf("circulating_supply = %v, want the raw lake figure", a.CirculatingSupply)
			}
			// ...but the default scale is not served beside it as though
			// it were a reading.
			if a.Decimals != nil {
				t.Errorf("decimals = %d served for a scale nobody read, want null", *a.Decimals)
			}
			// And the refusal is attributed in the funnel rather than
			// counted as a successful valuation.
			if n := dropCount(view, "assets_served_all_arms", v1.RWAReferenceValuationDecimalsUnknown); n != 1 {
				t.Errorf("the decimals refusal is not reported in the valuation arm (got %d)", n)
			}
		})
	}
}

// TestRWAListing_ReadDecimalsStillPublish is the other half: the guard
// must refuse an UNREAD scale, not every scale. Without this a fix that
// withheld every contract valuation would pass the test above.
func TestRWAListing_ReadDecimalsStillPublish(t *testing.T) {
	view := getRWA(t, rwaListingServer(t,
		&stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, sptr("1.22")),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		map[string]uint32{rwaListedBoundEUTBL: 5},
		map[string]timescale.DirectoryEntry{},
	))
	a, _ := assetByContract(view, rwaListedBoundEUTBL)
	if a.ReferenceValuation.ValueUSD == nil || *a.ReferenceValuation.ValueUSD != "345599978.73" {
		t.Fatalf("a READ scale of 5 must still publish: got %v", a.ReferenceValuation.ValueUSD)
	}
	if a.Valuation.MarketCapUSD == nil {
		t.Error("a read scale must still produce a market cap")
	}
}
