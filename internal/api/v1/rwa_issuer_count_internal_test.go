package v1

import "testing"

// TestRwaAssetIssuerCount_ContractsAreNotOneSharedBlankBucket pins that
// rwaAssetIssuerCount (behind /v1/rwa/assets' summary.issuers) counts
// each admitted contract as its own issuer, rather than collapsing every
// contract row's blank Issuer string into a single shared map key.
func TestRwaAssetIssuerCount_ContractsAreNotOneSharedBlankBucket(t *testing.T) {
	assets := make([]RWAAsset, 0, 26)
	for i := 0; i < 21; i++ {
		assets = append(assets, RWAAsset{
			AssetID: "CODE-issuer",
			Issuer:  issuerAddr(i),
		})
	}
	for i := 0; i < 5; i++ {
		assets = append(assets, RWAAsset{
			AssetID:    "contract-asset",
			ContractID: contractAddr(i),
		})
	}

	got := rwaAssetIssuerCount(assets)
	const want = 26 // 21 distinct classic issuers + 5 distinct contracts
	if got != want {
		t.Fatalf("rwaAssetIssuerCount() = %d, want %d", got, want)
	}
}

// TestRwaIssuerCount_AgreesAcrossAssetsAndHistoryViews is the
// cross-endpoint regression: the SAME membership (21 classic issuers, 5
// contracts) must publish the SAME issuers count whether it is read
// through /v1/rwa/assets' rwaAssetIssuerCount or /v1/rwa/history and
// /v1/rwa/premium's rwaHistoryIssuerCount.
func TestRwaIssuerCount_AgreesAcrossAssetsAndHistoryViews(t *testing.T) {
	m := rwaMembership{}
	assets := make([]RWAAsset, 0, 26)
	for i := 0; i < 21; i++ {
		m.members = append(m.members, rwaMember{issuer: issuerAddr(i)})
		assets = append(assets, RWAAsset{Issuer: issuerAddr(i)})
	}
	for i := 0; i < 5; i++ {
		m.contracts = append(m.contracts, rwaContractMember{contractID: contractAddr(i)})
		assets = append(assets, RWAAsset{ContractID: contractAddr(i)})
	}

	fromAssets := rwaAssetIssuerCount(assets)
	fromHistory := rwaHistoryIssuerCount(m)
	if fromAssets != fromHistory {
		t.Fatalf("issuers disagree across endpoints for the same membership: assets=%d history/premium=%d", fromAssets, fromHistory)
	}
	if fromAssets != 26 {
		t.Fatalf("rwaAssetIssuerCount() = %d, want 26", fromAssets)
	}
}

func issuerAddr(i int) string {
	return "GISSUER" + string(rune('A'+i))
}

func contractAddr(i int) string {
	return "CCONTRACT" + string(rune('A'+i))
}
