package v1_test

import (
	"errors"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/assets — C2's second arm rests on TWO sources, and a
// refusal reason has to name the one that actually failed.
//
// The listing directory supplies the arm's recognition. The curated
// account directory supplies the scam vocabulary C3 reads, and C3
// refuses a flagged address whatever named it — so either read failing
// closes the arm. Both closed it under `independent_listing_unavailable`,
// which names the listing sync.
//
// The two failures are not correlated. The curated directory can be
// unreadable while the listing directory sits there perfectly fresh,
// and in that state the funnel raised an operator-addressed alarm
// against a sync that was running perfectly well — the strongest form
// of a misdirected instruction this surface can emit, because it is
// indistinguishable from the real thing and sends somebody to the wrong
// system to look for a fault that is not there.

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

// The defect, stated as a test: a CURATED read that fails, beside a
// listing directory that answered, must not be reported as a listing
// outage.
//
// The evidence block is asserted alongside the reason because together
// they are the contradiction. A response saying "the independent
// listing is unavailable" while publishing a healthy, freshly-read
// listing directory in the same object is not merely unhelpful — it
// disagrees with itself, and a reader who trusts the reason over the
// evidence goes to the wrong system.
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
