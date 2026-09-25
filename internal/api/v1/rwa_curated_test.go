// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The curated arm serves a named third party's list under its own
// basis and its own total. These tests pin the property the whole
// arrangement rests on — a curated row is NEVER merged into the verified
// figures — and the arithmetic of the comparison it exists to publish.

const (
	// A contract the verified arms know nothing about: VuMe Bond 2030.
	curatedOnlyVuMe = "CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4"
	// A contract the listing arm ADMITS (bound in-repo + listed): EUTBL.
	curatedAlsoVerifiedEUTBL = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
)

type stubCuratedReader struct {
	rows      map[string]timescale.CuratedRWAEntry
	census    *timescale.CuratedRWACensus
	err       error
	published *timescale.CuratedRWAPublished
	pubErr    error
}

func (s *stubCuratedReader) CuratedRWADirectoryByAddress(_ context.Context, _ string) (
	map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error,
) {
	if s.err != nil {
		return nil, timescale.CuratedRWACensus{}, s.err
	}
	if s.census != nil {
		return s.rows, *s.census, nil
	}
	return s.rows, timescale.CuratedRWACensus{
		Entries: len(s.rows), Contracts: len(s.rows), Priced: len(s.rows),
	}, nil
}

func (s *stubCuratedReader) LatestCuratedPublished(_ context.Context, _ string) (*timescale.CuratedRWAPublished, error) {
	if s.pubErr != nil {
		return nil, s.pubErr
	}
	return s.published, nil
}

// publishedFixtureExecutedAt is when the curator's query last ran, held
// relative to the test's own clock (not a fixed calendar date) so the
// fixture stays inside [v1.rwaCuratedPublishedStaleAfter] and
// [v1.rwaCuratedPublishedMaxAge] no matter when the suite runs.
var publishedFixtureExecutedAt = time.Now().UTC().Add(-6 * time.Hour)

// publishedFixture is what the curator's two public queries printed: a
// headline of $4,004,795,860 for August 2025, its split, and a
// three-month series.
func publishedFixture() *timescale.CuratedRWAPublished {
	return publishedFixtureAt(publishedFixtureExecutedAt)
}

// publishedFixtureAt is [publishedFixture] with the curator's own
// execution clock set explicitly, for pinning the staleness bound.
func publishedFixtureAt(executedAt time.Time) *timescale.CuratedRWAPublished {
	month := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	return &timescale.CuratedRWAPublished{
		MonthEnd:         month(2025, 8, 31),
		TotalUSD:         "4004795860",
		SourceQuery:      6961845,
		SplitSourceQuery: 6961847,
		ExecutedAt:       executedAt,
		SplitExecutedAt:  executedAt,
		ObservedAt:       time.Now().UTC(),
		BySubclass: []timescale.CuratedRWAPublishedSplit{
			{Subclass: "US Treasuries", ValueUSD: "3100000000"},
			{Subclass: "Private Credit", ValueUSD: "904795860.00"},
		},
		Series: []timescale.CuratedRWAPublishedPoint{
			{MonthEnd: month(2025, 6, 30), ValueUSD: "3800000000.5"},
			{MonthEnd: month(2025, 7, 31), ValueUSD: "3900000000.10"},
			{MonthEnd: month(2025, 8, 31), ValueUSD: "4004795860"},
		},
	}
}

func curatedEntry(addr, company, subclass, price string) timescale.CuratedRWAEntry {
	return timescale.CuratedRWAEntry{
		Address: addr, AssetCode: "X", Company: company, AssetSubclass: subclass,
		PriceUSD: price, PricedAt: time.Now().UTC().Add(-time.Hour), Source: "test",
	}
}

func rwaCuratedServer(t *testing.T, curated v1.RWACuratedDirectoryReader) *v1.Server {
	t.Helper()
	rows := map[string]timescale.AssetRow{
		curatedOnlyVuMe:          rwaContractRow(curatedOnlyVuMe, nil),
		curatedAlsoVerifiedEUTBL: rwaContractRow(curatedAlsoVerifiedEUTBL, nil),
	}
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
		RWAContracts: &stubRWAContractReader{accounts: 18000},
		// The listing names EUTBL, so the verified listing arm admits it
		// and prices it at 1.22; that row is the "also verified" control.
		RWAListings: &stubRWAListings{rows: []timescale.ListingEntry{
			listed(curatedAlsoVerifiedEUTBL, "eutbl", "eutbl", "1.22"),
		}},
		RWACurated:        curated,
		ContractCatalogue: &stubContractCatalogue{rows: rows},
		TokenSymbol:       &stubTokenSymbols{byID: map[string]string{}},
		TokenSupply: &stubTokenSupplies{byID: map[string]string{
			curatedOnlyVuMe:          "500000000000000000000000000", // 500,000,000 at 18dp
			curatedAlsoVerifiedEUTBL: "28327867109034",              // 283,278,671.09034 at 5dp
		}},
		TokenDecimals:         &stubTokenDecimalsRdr{byID: map[string]uint32{curatedOnlyVuMe: 18, curatedAlsoVerifiedEUTBL: 5}},
		MinMarketCapVolumeUSD: 1000,
	})
}

func TestRWACurated_ServesTheCuratorsRowsApart(t *testing.T) {
	curated := &stubCuratedReader{rows: map[string]timescale.CuratedRWAEntry{
		curatedOnlyVuMe:          curatedEntry(curatedOnlyVuMe, "Realiz", "Corporate Credit", "1.1174"),
		curatedAlsoVerifiedEUTBL: curatedEntry(curatedAlsoVerifiedEUTBL, "Spiko", "Non-US Government Debt", "1.22"),
	}}
	with := getRWA(t, rwaCuratedServer(t, curated))
	without := getRWA(t, rwaCuratedServer(t, nil))

	// NEVER MERGED: the verified figures are identical with and without
	// the curated reader wired. Everything else here is secondary to
	// this line.
	if with.Summary.Assets != without.Summary.Assets ||
		derefStr(with.Summary.ReferenceValuation.ValueUSD) != derefStr(without.Summary.ReferenceValuation.ValueUSD) {
		t.Fatalf("the curated arm changed the verified summary: with=%d/%s without=%d/%s",
			with.Summary.Assets, derefStr(with.Summary.ReferenceValuation.ValueUSD),
			without.Summary.Assets, derefStr(without.Summary.ReferenceValuation.ValueUSD))
	}
	if _, ok := assetByContract(with, curatedOnlyVuMe); ok {
		t.Fatal("a curated-only contract appeared in the VERIFIED assets array")
	}

	if with.Curated == nil || with.Curated.Status != "served" {
		t.Fatalf("curated block = %+v, want status served", with.Curated)
	}
	if with.Curated.Assets != 2 || with.Curated.AlsoVerified != 1 {
		t.Errorf("curated assets=%d also_verified=%d, want 2 and 1", with.Curated.Assets, with.Curated.AlsoVerified)
	}

	var vume, eutbl *v1.RWAAsset
	for i := range with.CuratedAssets {
		switch with.CuratedAssets[i].ContractID {
		case curatedOnlyVuMe:
			vume = &with.CuratedAssets[i]
		case curatedAlsoVerifiedEUTBL:
			eutbl = &with.CuratedAssets[i]
		}
	}
	if vume == nil || eutbl == nil {
		t.Fatalf("curated rows missing: vume=%v eutbl=%v", vume != nil, eutbl != nil)
	}
	if vume.Basis != rwa.BasisThirdPartyCurated || vume.Recognition != rwa.RecognitionThirdPartyCurator {
		t.Errorf("vume basis/recognition = %q/%q", vume.Basis, vume.Recognition)
	}
	if vume.Curator == nil || vume.Curator.Company != "Realiz" || vume.Curator.Subclass != "Corporate Credit" || vume.Curator.AlsoVerified {
		t.Errorf("vume curator block = %+v", vume.Curator)
	}
	if vume.Reference == nil || vume.Reference.Provenance != v1.RWAReferenceCuratorPrice {
		t.Fatalf("vume reference = %+v, want provenance %s", vume.Reference, v1.RWAReferenceCuratorPrice)
	}
	// 500,000,000 tokens × 1.1174 — the curator's figure, reproduced
	// from the lake's supply and the curator's price.
	if got := derefStr(vume.ReferenceValuation.ValueUSD); got != "558700000.00" {
		t.Errorf("vume valuation = %s, want 558700000.00", got)
	}
	if vume.Premium.Status == "" || vume.Premium.Status == "published" {
		t.Errorf("a premium was published against a curator price: %q", vume.Premium.Status)
	}
	if eutbl.Curator == nil || !eutbl.Curator.AlsoVerified {
		t.Errorf("EUTBL is in the verified set and must be marked also_verified: %+v", eutbl.Curator)
	}

	// The additional total counts ONLY the curated-only row; the
	// also-verified row is already in the verified total and adding it
	// again would count it twice in the combined figure.
	if got := derefStr(with.Curated.AdditionalValueUSD); got != "558700000.00" {
		t.Errorf("additional_value_usd = %s, want the VuMe row alone", got)
	}
	if with.Curated.CombinedValueUSD == nil || with.Curated.VerifiedValueUSD == nil {
		t.Fatalf("combined/verified missing: %+v", with.Curated)
	}
	if derefStr(with.Curated.VerifiedValueUSD) != derefStr(with.Summary.ReferenceValuation.ValueUSD) {
		t.Errorf("verified_value_usd repeats the wrong figure")
	}
}

// The curator's free-text company label is the curator's word about a
// contract row, not an independent label on an issuer G-address (the R3
// evidence `issuer_directory_name` names) nor a SEP-1 `name`. It is
// served on `curator.company` and nowhere else.
func TestRWACurated_CompanyIsNotServedAsDirectoryOrSEP1Name(t *testing.T) {
	curated := &stubCuratedReader{rows: map[string]timescale.CuratedRWAEntry{
		curatedOnlyVuMe:          curatedEntry(curatedOnlyVuMe, "Realiz", "Corporate Credit", "1.1174"),
		curatedAlsoVerifiedEUTBL: curatedEntry(curatedAlsoVerifiedEUTBL, "Spiko", "Non-US Government Debt", "1.22"),
	}}
	got := getRWA(t, rwaCuratedServer(t, curated))
	if len(got.CuratedAssets) != 2 {
		t.Fatalf("served %d curated rows, want 2", len(got.CuratedAssets))
	}
	company := map[string]string{curatedOnlyVuMe: "Realiz", curatedAlsoVerifiedEUTBL: "Spiko"}
	for _, a := range got.CuratedAssets {
		want := company[a.ContractID]
		if a.IssuerDirectoryName != "" {
			t.Errorf("%s: issuer_directory_name = %q, want empty: a curated row has no directory-labelled issuer", a.ContractID, a.IssuerDirectoryName)
		}
		if a.Name == want {
			t.Errorf("%s: name = %q is the curator's company label, not a SEP-1 or bound-instrument name", a.ContractID, a.Name)
		}
		if a.Curator == nil || a.Curator.Company != want {
			t.Errorf("%s: curator.company = %+v, want %q", a.ContractID, a.Curator, want)
		}
	}
	if vume, ok := curatedByContract(got, curatedOnlyVuMe); !ok || vume.Name != "" {
		t.Errorf("unbound curated-only row name = %q, want empty (no SEP-1 entry, no in-repo binding)", vume.Name)
	}
}

func curatedByContract(v v1.RWAAssetsView, contractID string) (v1.RWAAsset, bool) {
	for _, a := range v.CuratedAssets {
		if a.ContractID == contractID {
			return a, true
		}
	}
	return v1.RWAAsset{}, false
}

// A classic `CODE-GISSUER` row has no contract path to be served through,
// so the arm leaves it out; the census must say how many it left out
// rather than let them vanish from the response.
func TestRWACurated_ClassicRowsAreCountedNotSilentlyDropped(t *testing.T) {
	classic := "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	curated := &stubCuratedReader{
		rows: map[string]timescale.CuratedRWAEntry{
			curatedOnlyVuMe: curatedEntry(curatedOnlyVuMe, "Realiz", "Corporate Credit", "1.1174"),
			classic:         curatedEntry(classic, "Etherfuse", "Non-US Government Debt", "0.057"),
		},
		census: &timescale.CuratedRWACensus{
			Entries: 2, Contracts: 1, Classic: 1, Priced: 1, PricedClassic: 1,
		},
	}
	got := getRWA(t, rwaCuratedServer(t, curated))
	if got.Curated == nil || got.Curated.Status != "served" {
		t.Fatalf("curated block = %+v, want status served", got.Curated)
	}
	if got.Curated.Assets != 1 || len(got.CuratedAssets) != 1 || got.CuratedAssets[0].ContractID != curatedOnlyVuMe {
		t.Fatalf("served %d rows, want the contract row alone", len(got.CuratedAssets))
	}
	c := got.Curated.Census
	if c.Entries != 2 || c.Contracts != 1 || c.Classic != 1 || c.Priced != 1 || c.PricedClassic != 1 {
		t.Errorf("census = %+v, want entries 2 = contracts 1 + classic 1, priced 1, priced_classic 1", c)
	}
	if c.Contracts+c.Classic != c.Entries {
		t.Errorf("census does not close: %d + %d != %d", c.Contracts, c.Classic, c.Entries)
	}
}

func TestCuratedRWACensus_Check(t *testing.T) {
	for name, tc := range map[string]struct {
		c    timescale.CuratedRWACensus
		want bool
	}{
		"balanced":              {timescale.CuratedRWACensus{Entries: 3, Contracts: 2, Classic: 1, Priced: 2, PricedClassic: 1}, true},
		"forms miss a row":      {timescale.CuratedRWACensus{Entries: 3, Contracts: 2}, false},
		"priced over contracts": {timescale.CuratedRWACensus{Entries: 1, Contracts: 1, Priced: 2}, false},
		"priced over classic":   {timescale.CuratedRWACensus{Entries: 1, Classic: 1, PricedClassic: 2}, false},
	} {
		if got := tc.c.Check() == ""; got != tc.want {
			t.Errorf("%s: Check() = %q, want balanced=%v", name, tc.c.Check(), tc.want)
		}
	}
}

func TestRWACurated_UnavailableAndUnwiredSaySo(t *testing.T) {
	failing := getRWA(t, rwaCuratedServer(t, &stubCuratedReader{err: errors.New("boom")}))
	if failing.Curated == nil || failing.Curated.Status != "unavailable" || len(failing.CuratedAssets) != 0 {
		t.Errorf("failed read: %+v rows=%d, want status unavailable and no rows", failing.Curated, len(failing.CuratedAssets))
	}
	unwired := getRWA(t, rwaCuratedServer(t, nil))
	if unwired.Curated == nil || unwired.Curated.Status != "unwired" {
		t.Errorf("no reader: %+v, want status unwired", unwired.Curated)
	}
}

// The curator's per-asset list is private, so in production the arm's
// rows are empty and what answers is the curator's PUBLISHED total.
// That state is its own status, carries the published block whole, and
// the gap to the verified figure is one signed number.
func TestRWACurated_PublishedTotalsWhenNoRowIsReadable(t *testing.T) {
	v := getRWA(t, rwaCuratedServer(t, &stubCuratedReader{published: publishedFixture()}))
	if v.Curated == nil || v.Curated.Status != "published_totals" {
		t.Fatalf("curated = %+v, want status published_totals", v.Curated)
	}
	if len(v.CuratedAssets) != 0 || v.Curated.Assets != 0 {
		t.Errorf("published totals served rows: %d / %d", len(v.CuratedAssets), v.Curated.Assets)
	}
	p := v.Curated.Published
	if p == nil {
		t.Fatal("published block missing")
	}
	if p.TotalUSD != "4004795860.00" || p.AsOf != "2025-08-31" {
		t.Errorf("headline = %s as of %s, want 4004795860.00 as of 2025-08-31", p.TotalUSD, p.AsOf)
	}
	if got := time.Time(p.ExecutedAt); !got.Equal(publishedFixtureExecutedAt) {
		t.Errorf("executed_at = %v, want the curator's execution time", got)
	}
	if p.Stale {
		t.Errorf("a 6-hour-old published total must not be labelled stale")
	}
	if p.Source != "dune query 6961845 / 6961847" {
		t.Errorf("source = %q", p.Source)
	}
	if len(p.BySubclass) != 2 || p.BySubclass[0].Subclass != "US Treasuries" || p.BySubclass[0].ValueUSD != "3100000000.00" ||
		p.BySubclass[1].ValueUSD != "904795860.00" {
		t.Errorf("by_subclass = %+v", p.BySubclass)
	}
	if len(p.Series) != 3 || p.Series[0].MonthEnd != "2025-06-30" || p.Series[0].ValueUSD != "3800000000.50" ||
		p.Series[2].MonthEnd != "2025-08-31" || p.Series[2].ValueUSD != "4004795860.00" {
		t.Errorf("series = %+v, want three points oldest first at 2dp", p.Series)
	}
	// The gap is published − verified, signed: the verified reference
	// total here is the EUTBL listing row alone.
	verified := ratFromStr(t, derefStr(v.Summary.ReferenceValuation.ValueUSD))
	wantGap := new(big.Rat).Sub(ratFromStr(t, "4004795860"), verified).FloatString(2)
	if derefStr(p.GapVsVerifiedUSD) != wantGap {
		t.Errorf("gap_vs_verified_usd = %s, want %s (published %s − verified %s)",
			derefStr(p.GapVsVerifiedUSD), wantGap, p.TotalUSD, verified.FloatString(2))
	}
	if strings.HasPrefix(derefStr(p.GapVsVerifiedUSD), "-") || verified.Sign() <= 0 {
		t.Errorf("gap %s should be positive here (the curator counts far more than the %s this index verifies)",
			derefStr(p.GapVsVerifiedUSD), verified.FloatString(2))
	}
	if derefStr(v.Curated.VerifiedValueUSD) != derefStr(v.Summary.ReferenceValuation.ValueUSD) {
		t.Errorf("verified_value_usd repeats the wrong figure")
	}
	if !strings.Contains(v.Curated.Basis, "PRIVATE") || !strings.Contains(v.Curated.Basis, "PUBLISHES") {
		t.Errorf("basis prose must say the list is private and only published totals are read: %q", v.Curated.Basis)
	}

	// A wired reader whose published read fails, with no rows either,
	// is unavailable — never a served figure from a failed read.
	failing := getRWA(t, rwaCuratedServer(t, &stubCuratedReader{pubErr: errors.New("boom")}))
	if failing.Curated == nil || failing.Curated.Status != "unavailable" || failing.Curated.Published != nil {
		t.Errorf("failed published read: %+v, want unavailable and no block", failing.Curated)
	}

	// Rows AND a published block: status stays served, the block rides
	// along, and the verified headline is untouched by either.
	both := getRWA(t, rwaCuratedServer(t, &stubCuratedReader{
		rows:      map[string]timescale.CuratedRWAEntry{curatedOnlyVuMe: curatedEntry(curatedOnlyVuMe, "Realiz", "Corporate Credit", "1.1174")},
		published: publishedFixture(),
	}))
	if both.Curated == nil || both.Curated.Status != "served" || both.Curated.Published == nil || both.Curated.Assets != 1 {
		t.Errorf("rows + published: %+v", both.Curated)
	}
	if derefStr(both.Summary.ReferenceValuation.ValueUSD) != derefStr(v.Summary.ReferenceValuation.ValueUSD) {
		t.Error("the published block changed the verified summary")
	}
}

// The curator's own execution clock (executed_at) is a SEPARATE fact
// from our sync's own freshness (observed_at): a sync that keeps
// succeeding against an unmoving curator-side result must not be read
// as a fresh headline figure forever.
func TestRWACurated_PublishedTotalAgesOutStaleThenGone(t *testing.T) {
	stale := getRWA(t, rwaCuratedServer(t, &stubCuratedReader{
		published: publishedFixtureAt(time.Now().UTC().Add(-60 * time.Hour)), // > 48h, < 7d
	}))
	if stale.Curated == nil || stale.Curated.Published == nil {
		t.Fatalf("60h-old published total: %+v, want it still served", stale.Curated)
	}
	if !stale.Curated.Published.Stale {
		t.Error("a 60-hour-old published total must be labelled stale: true")
	}

	gone := getRWA(t, rwaCuratedServer(t, &stubCuratedReader{
		published: publishedFixtureAt(time.Now().UTC().Add(-8 * 24 * time.Hour)), // > 7d
	}))
	if gone.Curated == nil || gone.Curated.Published != nil || gone.Curated.Status != "unavailable" {
		t.Errorf("8-day-old published total: %+v, want no published block and status unavailable", gone.Curated)
	}
}

// TestRWACurated_PublishedSplitCarriesItsOwnExecution: the total and the
// split are two separate query executions. The split is served with its
// own executed_at, and a split whose query has not run inside the 7-day
// cutoff is withheld rather than served beside a fresh total as if the
// two were one execution.
func TestRWACurated_PublishedSplitCarriesItsOwnExecution(t *testing.T) {
	now := time.Now().UTC()
	older := publishedFixtureAt(now.Add(-6 * time.Hour))
	older.SplitExecutedAt = now.Add(-40 * time.Hour)
	v := getRWA(t, rwaCuratedServer(t, &stubCuratedReader{published: older}))
	p := v.Curated.Published
	if p == nil || len(p.BySubclass) != 2 {
		t.Fatalf("published = %+v, want the block with a two-line split", p)
	}
	if p.BySubclassExecutedAt == nil || !time.Time(*p.BySubclassExecutedAt).Equal(older.SplitExecutedAt) {
		t.Errorf("by_subclass_executed_at = %v, want the split's own execution %v (not the total's %v)",
			p.BySubclassExecutedAt, older.SplitExecutedAt, older.ExecutedAt)
	}

	frozen := publishedFixtureAt(now.Add(-6 * time.Hour))
	frozen.SplitExecutedAt = now.Add(-8 * 24 * time.Hour)
	v = getRWA(t, rwaCuratedServer(t, &stubCuratedReader{published: frozen}))
	p = v.Curated.Published
	if p == nil {
		t.Fatal("a fresh total must still be served when only its split is frozen")
	}
	if len(p.BySubclass) != 0 || p.BySubclassExecutedAt != nil {
		t.Errorf("8-day-old split served: %+v at %v, want it withheld", p.BySubclass, p.BySubclassExecutedAt)
	}
}

func ratFromStr(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a decimal: %q", s)
	}
	return r
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
