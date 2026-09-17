// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
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
	rows map[string]timescale.CuratedRWAEntry
	err  error
}

func (s *stubCuratedReader) CuratedRWADirectoryByAddress(_ context.Context, _ string) (
	map[string]timescale.CuratedRWAEntry, timescale.CuratedRWACensus, error,
) {
	if s.err != nil {
		return nil, timescale.CuratedRWACensus{}, s.err
	}
	return s.rows, timescale.CuratedRWACensus{Entries: len(s.rows), Priced: len(s.rows)}, nil
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

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
