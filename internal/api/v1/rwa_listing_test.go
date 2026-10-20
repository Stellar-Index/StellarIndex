package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/assets second arm: an independent listing directory corroborating an in-repo curated binding. It exists because
// the curated account directory names none of the contract addresses we hold verified bindings for. Two sources are required,
// neither alone suffices, and the surface says which pair admitted a row.

// Real addresses the two sources agree on, and real ones they don't; unlike invented C-strkeys elsewhere, arm 2 claims two
// independent parties produced the same 56 characters, which a made-up address can't express.
const (
	// Bound in-repo AND named by the listing directory.
	rwaListedBoundEUTBL = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	// Bound in-repo, NOT named by the listing directory. The control.
	rwaBoundNotListedUSTBL = "CARUUX2FZNPH6DGJOEUFSIUQWYHNL5AVDV7PMVSHWL7OBYIBFC76F4TO"
	// Named by the listing directory, bound by NOBODY: the native asset's own Stellar Asset
	// Contract, a real entry in the same map. A permanent negative control, since XLM can
	// never acquire a curated RWA binding, so no later admission can retire this test.
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
	// census, when set, is served verbatim instead of derived from rows. The derivation counts
	// every served row as a CONTRACT row, so it cannot express the healthy state that serves
	// none: a directory holding only classic rows.
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
		// every bound address.
		RWAContracts:          &stubRWAContractReader{accounts: 18000},
		RWAListings:           listings,
		ContractCatalogue:     &stubContractCatalogue{rows: rows},
		TokenSymbol:           &stubTokenSymbols{byID: map[string]string{}},
		TokenSupply:           &stubTokenSupplies{byID: supplies},
		TokenDecimals:         &stubTokenDecimalsRdr{byID: decimals},
		NonstandardDecimals:   confirmedNonstandardDecimals(t, decimals),
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

// TestRWAListing_CorroboratedBindingIsAdmittedAndValued is the end-to-end admission: curated directory names nothing, the
// listing and an in-repo binding name one address; the row is served, names the admitting pair, valued at the listing
// price.
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

// TestRWAListing_DecimalsAreReadNotAssumed is the 10^n guard: a wrong exponent changes the published money figure by
// orders
// of magnitude. The same supply and price at scale 5 vs the default 7 must differ by exactly 100x.
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

// TestRWAListing_ListingAloneAdmitsNothing is the negative control: a real address the listing names with no in-repo
// binding is absent from the set but counted in the terminal census, so the rule is auditable.
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

// TestRWAListing_BoundButUnlistedStaysRefused: a binding on identical evidence that the listing doesn't name is refused,
// under a reason naming the missing half rather than a generic failure.
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

// TestRWAListing_ScamFlagBeatsCorroboration is the precedence test at the response level. The
// listing carries no flags, so if the scam check did not cover this arm an address the
// curated directory named ONLY to flag as malicious would be served with a large valuation.
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

// TestRWAListing_UnavailableShrinksTheSetAndSaysSo is the fail-closed guarantee: with the listing reader failing, nothing
// is admitted, bindings are refused under the outage reason, and the funnel basis states it in prose.
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

// TestRWAListing_EntirelyStaleSnapshotIsAnOutageNotAFinding: `synced_at` is stamped once per sync, so all rows age out
// together, arriving as zero rows beside a non-zero stale count. That is an expired copy, not a listing naming nothing, so
// it must not be treated as a populated snapshot refusing every binding.
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

// TestRWAListing_UnwiredIsNotMeasured: no reader wired means nobody looked, not that nobody agreed, so the arm reports
// itself unmeasured and makes no refusals.
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

// TestRWAListing_BasisDoesNotInheritTheOracleWording: the basis string must not claim an oracle valuation plus the
// issuer's
// one-token-one-unit declaration; a listing price is neither, so that would be a false provenance claim.
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

// TestRWAListing_StalePriceIsNotServedAsFresh pins the price bound at the surface as well as in SQL: a listing row older
// than the absolute outer bound carries no valuation and names the rule that refused it.
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

// TestRWAListing_DirectoryArmWinsWhenBothNameIt pins the dedup: an address in both sources
// must not be evaluated twice, or a double-counted valuation ships. The directory arm keeps
// it, because its recognition is stronger and admits on its own.
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

// TestRWAListing_UnreadDecimalsPublishNoFigure is the 10^n guard: with supply up and decimals down (or an instance missing, no
// METADATA scale, or conflicting spellings), rows keep the catalogue's default 7, so a 5-decimal fund publishes 1/100 of its
// cap as `published`. Refuse the figure; supply is still served, as a chain fact needing no scale.
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

// TestRWAListing_UnreadDecimalsServeNullScale pins that a contract row with an unread scale serves null `decimals`, not
// the catalogue default 7, which would invite a supply/10^decimals division both valuation bases refuse.
func TestRWAListing_UnreadDecimalsServeNullScale(t *testing.T) {
	raw, ok := rwaWireDecimals(t, map[string]uint32{})
	if !ok {
		t.Fatal("decimals key absent; the schema requires it (null when unread)")
	}
	if string(raw) != "null" {
		t.Errorf("decimals = %s on a row whose scale was never read, want null", raw)
	}
}

// TestRWAListing_ReadDecimalsServeTheReading is the other half: a scale
// that WAS read is served as that number, not nulled with the rest.
func TestRWAListing_ReadDecimalsServeTheReading(t *testing.T) {
	raw, ok := rwaWireDecimals(t, map[string]uint32{rwaListedBoundEUTBL: 5})
	if !ok || string(raw) != "5" {
		t.Errorf("decimals = %s (present %v), want the contract's declared 5", raw, ok)
	}
}

// decimalsText renders a served scale for a failure message.
func decimalsText(d *int) string {
	if d == nil {
		return "null"
	}
	return strconv.Itoa(*d)
}

// rwaWireDecimals fetches /v1/rwa/assets and returns the raw `decimals`
// value served on the contract row, read from the bytes rather than a Go
// struct so the assertion is about the wire and not a decode default.
func rwaWireDecimals(t *testing.T, decimals map[string]uint32) (json.RawMessage, bool) {
	t.Helper()
	srv := rwaListingServer(t,
		&stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, sptr("1.22")),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		decimals,
		map[string]timescale.DirectoryEntry{},
	)
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/rwa/assets")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Assets []map[string]json.RawMessage `json:"assets"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, a := range env.Data.Assets {
		var id string
		if err := json.Unmarshal(a["contract_id"], &id); err != nil || id != rwaListedBoundEUTBL {
			continue
		}
		raw, ok := a["decimals"]
		return raw, ok
	}
	t.Fatalf("contract row %s not served", rwaListedBoundEUTBL)
	return nil, false
}

// GET /v1/rwa/assets: C2's second arm rests on two sources (listing directory for recognition, curated directory for the
// scam vocabulary C3 reads), and either failing closes the arm. The failures are uncorrelated, so a refusal reason must
// name the source that actually failed or the operator is sent to the wrong system.

// rwaTwoSourceServer wires the listing arm with the curated directory
// reader under the test's control, so each source can be failed on its
// own.
func rwaTwoSourceServer(
	t *testing.T, listings v1.RWAListingDirectoryReader, dir *stubDirectoryReader,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{},
		Directory: dir,
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
		RWAContracts: &stubRWAContractReader{accounts: 18000},
		RWAListings:  listings,
		ContractCatalogue: &stubContractCatalogue{rows: map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		}},
		TokenSymbol:           &stubTokenSymbols{byID: map[string]string{}},
		TokenSupply:           &stubTokenSupplies{byID: map[string]string{rwaListedBoundEUTBL: "28327867109034"}},
		TokenDecimals:         &stubTokenDecimalsRdr{byID: map[string]uint32{rwaListedBoundEUTBL: 5}},
		MinMarketCapVolumeUSD: 1000,
	})
}

// A failed curated read beside an answering listing directory must not be reported as a listing outage; the evidence block
// is asserted too, since "listing unavailable" next to a healthy listing contradicts itself.
func TestRWAListingSources_CuratedTagFailureIsNotAListingOutage(t *testing.T) {
	view := getRWA(t, rwaTwoSourceServer(t,
		// The listing directory ANSWERS, freshly, naming the address.
		&stubRWAListings{rows: []timescale.ListingEntry{
			listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		// The curated directory does not.
		&stubDirectoryReader{err: errors.New("curated directory read failed")},
	))

	bindings := stageCount(t, view, "curated_contract_bindings")
	if bindings == 0 {
		t.Fatal("no bindings enumerated: the fixture cannot express the defect")
	}
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractListingUnavailable); n != 0 {
		t.Errorf("%d bindings dropped under %q while the listing directory answered — "+
			"the alarm names a source that is working and sends an operator to the wrong system",
			n, rwa.RejectContractListingUnavailable)
	}
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractCuratedTagsUnavailable); n != bindings {
		t.Errorf("drop under %q = %d, want every one of the %d bindings",
			rwa.RejectContractCuratedTagsUnavailable, n, bindings)
	}
	// The contradiction the reason would have produced, asserted from
	// the other side: the listing source demonstrably answered.
	ev := view.Funnel.ListingDirectory
	if ev == nil {
		t.Fatal("no evidence published for a read that answered")
	}
	if ev.Entries != 1 || ev.Contracts != 1 || ev.Stale != 0 {
		t.Errorf("evidence = %+v, want the healthy directory the read actually saw", ev)
	}
	// Nothing is admitted either way: C3 was not evaluated, so it may
	// not be assumed satisfied.
	if _, ok := assetByContract(view, rwaListedBoundEUTBL); ok {
		t.Error("a row was admitted with C3 unevaluated — a scam flag could not have refused it")
	}
	// And the tally the response publishes agrees with the funnel,
	// rather than the two disagreeing about which source failed.
	if n := refusalCount(view, rwa.RejectContractListingUnavailable); n != 0 {
		t.Errorf("refused[] still reports %d under the listing outage reason", n)
	}
	if n := refusalCount(view, rwa.RejectContractCuratedTagsUnavailable); n != bindings {
		t.Errorf("refused[] reports %d under the curated reason, want %d", n, bindings)
	}
}

// The converse, so a fix that merely renamed the reason cannot pass. A
// genuine LISTING outage must still be reported as one.
func TestRWAListingSources_ListingOutageStillNamesTheListing(t *testing.T) {
	view := getRWA(t, rwaTwoSourceServer(t,
		&stubRWAListings{err: errors.New("listing read failed")},
		&stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{}},
	))

	bindings := stageCount(t, view, "curated_contract_bindings")
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractListingUnavailable); n != bindings {
		t.Errorf("listing outage drop = %d, want every one of the %d bindings", n, bindings)
	}
	if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractCuratedTagsUnavailable); n != 0 {
		t.Errorf("%d bindings blamed on the curated read while IT answered", n)
	}
}

// refusalCount reads one reason out of the response's single `refused[]`
// tally — the view a consumer gets that knows nothing about arms.
func refusalCount(v v1.RWAAssetsView, reason string) int {
	for _, r := range v.Refused {
		if r.Reason == reason {
			return r.Assets
		}
	}
	return 0
}

// GET /v1/rwa/assets: the EVIDENCE behind C2's second arm, as opposed to its verdict.
//
// The response must publish the evidence behind a closed arm, not only the verdict, because
// three unrelated states produce the same "nothing corroborated" verdict:
//
//   - a directory nobody has ever synced,
//   - a sync that stopped days ago and left every row to age out,
//   - a healthy directory this set simply predates.
//
// They call for different responses (run the sync, fix the sync, do nothing), so they must be
// told apart on the wire. The tests are one per state plus the two states that publish no
// evidence at all.

// rwaEvidenceServer wires the listing arm with nothing else able to
// admit a row, so the funnel under test is arm 2's alone.
func rwaEvidenceServer(t *testing.T, listings v1.RWAListingDirectoryReader) *v1.Server {
	t.Helper()
	return rwaListingServer(t, listings,
		map[string]timescale.AssetRow{
			rwaListedBoundEUTBL: rwaContractRow(rwaListedBoundEUTBL, nil),
		},
		map[string]string{rwaListedBoundEUTBL: "28327867109034"},
		map[string]uint32{rwaListedBoundEUTBL: 5},
		map[string]timescale.DirectoryEntry{},
	)
}

// A set built from a read taken before the sync landed must say on the wire when it looked and what it saw; without the
// timestamp, re-reading later shows a healthy directory while the response still reports a closed arm.
func TestRWAListingEvidence_DatesTheReadThatClosedTheArm(t *testing.T) {
	// The directory as it stood before the first sync ever completed.
	before := time.Now().UTC().Add(-time.Second)
	view := getRWA(t, rwaEvidenceServer(t, &stubRWAListings{}))
	after := time.Now().UTC().Add(time.Second)

	ev := view.Funnel.ListingDirectory
	if ev == nil {
		t.Fatal("the arm closed and published no evidence for it: a reader " +
			"holding only the verdict cannot tell a stopped sync from a set " +
			"that predates a working one")
	}
	at := ev.ObservedAt.Time()
	if at.Before(before) || at.After(after) {
		t.Errorf("observed_at = %s, want the instant of the read (between %s and %s)",
			at.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
	if at.Location() != time.UTC {
		t.Errorf("observed_at carries a %v offset; the wire contract is UTC", at.Location())
	}
	// And it is the read's instant, not the response's: a timestamp
	// stamped at render time would date every response to now and
	// answer the opposite of the question asked.
	if ev.Entries != 0 || ev.Stale != 0 {
		t.Errorf("evidence = %+v, want the empty directory the read actually saw", ev)
	}
	if n := stageCount(t, view, "listing_corroborated_contracts"); n != 0 {
		t.Fatalf("corroborated = %d — the fixture must close the arm for this to mean anything", n)
	}
}

// One case per state that closes the arm; the verdict is identical, only the evidence separates them. `entries` counts
// fresh rows only (Entries = Contracts + Classic, Stale beside), so the two zero-entry states differ by `stale` alone.
func TestRWAListingEvidence_SeparatesTheThreeClosedArmStates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listings  *stubRWAListings
		entries   int
		contracts int
		stale     int
		reading   string
	}{
		{
			name:     "never synced",
			listings: &stubRWAListings{},
			reading:  "the table is empty and nothing has ever written to it: run the sync",
		},
		{
			// Every row ages out together, because synced_at is stamped
			// once per sync transaction. The recognised set empties
			// while every read keeps succeeding.
			name:     "the sync stopped",
			listings: &stubRWAListings{stale: 17},
			stale:    17,
			reading:  "rows are present and every one is past the bound: go and fix the sync",
		},
		{
			// A directory that lists Stellar assets only by their
			// classic CODE-GISSUER ids names no contract address at
			// all. The arm has nothing to consult and nothing is wrong.
			name: "healthy, but it names no contract address",
			listings: &stubRWAListings{census: &timescale.ListingDirectoryCensus{
				Entries: 33, Classic: 33,
			}},
			entries: 33,
			reading: "fresh rows, none of them contracts: nothing to fix",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := getRWA(t, rwaEvidenceServer(t, tc.listings))
			if n := stageCount(t, view, "listing_corroborated_contracts"); n != 0 {
				t.Fatalf("corroborated = %d, want 0 — every case here closes the arm", n)
			}
			ev := view.Funnel.ListingDirectory
			if ev == nil {
				t.Fatalf("no evidence published; a reader cannot reach %q from the verdict alone", tc.reading)
			}
			if ev.Entries != tc.entries || ev.Contracts != tc.contracts || ev.Stale != tc.stale {
				t.Errorf("evidence = {entries %d, contracts %d, stale %d}, want {%d, %d, %d} — %s",
					ev.Entries, ev.Contracts, ev.Stale, tc.entries, tc.contracts, tc.stale, tc.reading)
			}
			if ev.ObservedAt.IsZero() {
				t.Error("evidence carries counts with no instant: the counts can be re-derived " +
					"from the table afterwards, the moment this set looked cannot")
			}
		})
	}
}

// A HEALTHY arm publishes its evidence too. Otherwise a change emitting the block only on the
// closed path would pass everything above, and the comparison of this set's read against the
// sync's clock would be unavailable exactly when confirming a recovery.
func TestRWAListingEvidence_PublishedOnTheHealthyPathToo(t *testing.T) {
	view := getRWA(t, rwaEvidenceServer(t, &stubRWAListings{
		rows: []timescale.ListingEntry{listed(rwaListedBoundEUTBL, "eutbl", "eutbl", "1.22")},
	}))
	if n := stageCount(t, view, "listing_corroborated_contracts"); n != 1 {
		t.Fatalf("corroborated = %d, want 1 — the fixture must OPEN the arm here", n)
	}
	ev := view.Funnel.ListingDirectory
	if ev == nil {
		t.Fatal("an open arm published no evidence: the block is not a failure report")
	}
	if ev.Entries != 1 || ev.Contracts != 1 || ev.Stale != 0 {
		t.Errorf("evidence = %+v, want one fresh contract row", ev)
	}
}

// A read that did not answer observed nothing and must publish nothing, not a census of zeros: zeros from a failed query
// look like an empty table but are a finding about our connection. An absent block means the source couldn't be read.
func TestRWAListingEvidence_AFailedReadPublishesNone(t *testing.T) {
	view := getRWA(t, rwaEvidenceServer(t, &stubRWAListings{err: errRWAListingRead}))
	if ev := view.Funnel.ListingDirectory; ev != nil {
		t.Fatalf("a failed read published evidence %+v — zeros from a query that "+
			"never completed read as a directory holding no rows, which is a "+
			"finding nobody made", ev)
	}
	// The outage is still reported; it is the EVIDENCE that is absent,
	// not the verdict.
	bindings := stageCount(t, view, "curated_contract_bindings")
	if n := dropCount(view, "curated_contract_bindings", rwaRejectListingUnavailable); n != bindings {
		t.Errorf("outage drop = %d, want every one of the %d bindings", n, bindings)
	}
}

// No reader wired is the other no-evidence state, and for the stronger
// reason: nobody looked at all.
func TestRWAListingEvidence_UnwiredReaderPublishesNone(t *testing.T) {
	view := getRWA(t, rwaEvidenceServer(t, nil))
	if ev := view.Funnel.ListingDirectory; ev != nil {
		t.Fatalf("an unwired reader published evidence %+v — nothing was observed", ev)
	}
}

// Fixture constants kept beside the tests that read them.
var errRWAListingRead = errors.New("listing read failed")

// rwaRejectListingUnavailable is the reason an outage is published
// under, spelled once here so a rename in internal/rwa reaches these
// tests through the compiler.
const rwaRejectListingUnavailable = rwa.RejectContractListingUnavailable
