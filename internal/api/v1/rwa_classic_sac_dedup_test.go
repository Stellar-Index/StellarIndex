package v1_test

import (
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRWA_ClassicMembersSACIsNotASecondAsset: the directory naming an admitted classic member's
// SAC must not add a contract row, or the one supply is valued twice in every total. A mis-cased or padded declaration
// still reaches its row through the catalogue join, so it must reach that row's SAC too, under the row's real (mixed-case) code.
func TestRWA_ClassicMembersSACIsNotASecondAsset(t *testing.T) {
	for _, tc := range []struct{ declared, real string }{
		{"USTRY", "USTRY"}, {"ustry", "USTRY"}, {" USTRY", "USTRY"}, {"XAUM", "XAUm"}, {"IBENJI", "iBENJI"},
	} {
		t.Run(tc.declared+"->"+tc.real, func(t *testing.T) { classicMemberSACIsNotASecondAsset(t, tc.declared, tc.real) })
	}
}

func classicMemberSACIsNotASecondAsset(t *testing.T, declared, real string) {
	bound, dir, rows := oneBoundMember()
	bound[0].Code = declared
	rows[rwaGoodIssuer][0] = rwaRow(real, rwaGoodIssuer, sptr("1.0412"), 346312)
	sac, err := canonical.Asset{Type: canonical.AssetClassic, Code: real, Issuer: rwaGoodIssuer}.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	view := getRWA(t, rwaClassicSACServer(bound, dir, rows, sac))

	contracts := 0
	for _, a := range view.Assets {
		if a.ContractID == sac {
			t.Errorf("the classic member's SAC is served as its own row: %+v", a)
		}
		if a.ContractID != "" {
			contracts++
		}
	}
	if len(view.Assets) != 2 || contracts != 1 {
		t.Fatalf("assets = %d (contracts %d), want the classic row and the unrelated contract", len(view.Assets), contracts)
	}
	if !view.Funnel.Balanced {
		t.Fatalf("funnel does not balance:\n%s", funnelDump(view))
	}
	assertContractDrop(t, view, "contract_candidates_evaluated", "contract_is_classic_member_sac", 1)
	found := false
	for _, r := range view.Refused {
		found = found || (r.Reason == "contract_is_classic_member_sac" && r.Assets == 1)
	}
	if !found {
		t.Errorf("refused = %+v, want contract_is_classic_member_sac: 1", view.Refused)
	}
}

// TestRWA_DistinctCaseSiblingSACIsKept: one issuer's `ustry` beside the admitted `USTRY` is another asset (codes are
// case-sensitive), so its SAC is not the member's and must be served, not refused as a duplicate.
func TestRWA_DistinctCaseSiblingSACIsKept(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	rows[rwaGoodIssuer] = append(rows[rwaGoodIssuer], rwaRow("ustry", rwaGoodIssuer, sptr("1.0412"), 346312))
	sibling, err := canonical.Asset{Type: canonical.AssetClassic, Code: "ustry", Issuer: rwaGoodIssuer}.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	view := getRWA(t, rwaClassicSACServer(bound, dir, rows, sibling))
	served := false
	for _, a := range view.Assets {
		served = served || a.ContractID == sibling
	}
	if !served {
		t.Fatalf("assets = %+v, refused = %+v: the distinct ustry SAC was dropped as the USTRY member's", view.Assets, view.Refused)
	}
	for _, r := range view.Refused {
		if r.Reason == "contract_is_classic_member_sac" {
			t.Errorf("refused = %+v, want no contract_is_classic_member_sac", view.Refused)
		}
	}
}

// rwaClassicSACServer serves one bound classic member beside a directory naming sac and an unrelated contract.
func rwaClassicSACServer(bound []timescale.Sep1BoundCurrency, dir map[string]timescale.DirectoryEntry, rows map[string][]timescale.AssetRow, sac string) *v1.Server {
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
		RWAContracts: &stubRWAContractReader{contracts: []timescale.DirectoryEntry{
			recognisedContract(sac, "Etherfuse"),
			recognisedContract(rwaContractGood, "Example Treasury Fund"),
		}},
		ContractCatalogue: &stubContractCatalogue{rows: map[string]timescale.AssetRow{
			sac:             rwaContractRow(sac, sptr("1.0412")),
			rwaContractGood: rwaContractRow(rwaContractGood, sptr("1.0740000000")),
		}},
		TokenSymbol:           &stubTokenSymbols{byID: map[string]string{sac: "USTRY", rwaContractGood: "USTRY"}},
		TokenSupply:           &stubTokenSupplies{byID: map[string]string{sac: "3463120000000", rwaContractGood: "10000000000000"}},
		TokenDecimals:         &stubTokenDecimalsRdr{byID: map[string]uint32{sac: 7, rwaContractGood: 7}},
		MinMarketCapVolumeUSD: 1000,
	})
}

// TestRWACurated_ClassicMembersSACIsAlsoVerified: a curated row for a verified classic member's
// SAC is the same asset, so it adds nothing to the curator's additional or combined value.
func TestRWACurated_ClassicMembersSACIsAlsoVerified(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac, err := canonical.Asset{Type: canonical.AssetClassic, Code: "USTRY", Issuer: rwaGoodIssuer}.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	view := getRWA(t, v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
		RWAContracts: &stubRWAContractReader{accounts: 18000},
		RWACurated: &stubCuratedReader{
			rows:   map[string]timescale.CuratedRWAEntry{sac: curatedEntry(sac, "Etherfuse", "US Government Debt", "1.0412")},
			census: &timescale.CuratedRWACensus{Entries: 1, Contracts: 1, Priced: 1},
		},
		ContractCatalogue:     &stubContractCatalogue{rows: map[string]timescale.AssetRow{sac: rwaContractRow(sac, nil)}},
		TokenSymbol:           &stubTokenSymbols{byID: map[string]string{sac: "USTRY"}},
		TokenSupply:           &stubTokenSupplies{byID: map[string]string{sac: "3463120000000"}},
		TokenDecimals:         &stubTokenDecimalsRdr{byID: map[string]uint32{sac: 7}},
		MinMarketCapVolumeUSD: 1000,
	}))
	if view.Curated == nil || view.Curated.AlsoVerified != 1 {
		t.Fatalf("curated = %+v, want the SAC row marked also verified", view.Curated)
	}
	if got := derefStr(view.Curated.AdditionalValueUSD); got != "" && got != "0.00" {
		t.Errorf("additional_value_usd = %q: the classic member's supply is counted a second time", got)
	}
}
