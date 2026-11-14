package v1_test

import (
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRWA_ClassicMembersSACIsNotASecondAsset: the directory naming an admitted classic member's
// SAC must not add a contract row, or the one supply is valued twice in every total.
func TestRWA_ClassicMembersSACIsNotASecondAsset(t *testing.T) {
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
	}))

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
