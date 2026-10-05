package v1_test

import (
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	// Named by centrifuge.io's own SEP-1 [[CURRENCIES]] contract field.
	rwaDeJTRSY = "CBI7UCH5KGSVQRO5H4SUCZUTZABCITZLRHQQZTWL2TK4RZ72TAR6IHRV"
	rwaDeJAAA  = "CC64WBDGS6QQP22QTTIACYIXT3WF7BBQEYOQPLTP7GTKYY7PZ74QYGSL"
	// A real contract that is not deJTRSY, made to call itself deJTRSY
	// on chain and in the listing: the code-only impersonation the
	// contract key exists to refuse.
	rwaDeJTRSYImpostor = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	// deJTRSY's measured supply at 18 decimals: 8,763,619.97 tokens.
	rwaDeJTRSYSupply = "8763619974700234898508352"
)

// TestRWADeRWA_AdmittedByContractNeverByCode: the deRWA
// tokens reach /v1/rwa/assets keyed on their exact contract address, and
// a different contract wearing the same code never does.
func TestRWADeRWA_AdmittedByContractNeverByCode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		listing    []timescale.ListingEntry
		wantServed []string
		wantAbsent []string
	}{
		{
			name:       "listed deJTRSY is admitted on its contract id",
			listing:    []timescale.ListingEntry{listed(rwaDeJTRSY, "dejtrsy", "dejtrsy", "1.02")},
			wantServed: []string{rwaDeJTRSY},
			wantAbsent: []string{rwaDeJAAA, rwaDeJTRSYImpostor},
		},
		{
			name:       "same code from another contract admits nothing",
			listing:    []timescale.ListingEntry{listed(rwaDeJTRSYImpostor, "dejtrsy", "dejtrsy", "1.02")},
			wantAbsent: []string{rwaDeJTRSYImpostor, rwaDeJTRSY, rwaDeJAAA},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := []string{rwaDeJTRSY, rwaDeJAAA, rwaDeJTRSYImpostor}
			rows := map[string]timescale.AssetRow{}
			supplies := map[string]string{}
			decimals := map[string]uint32{}
			for _, id := range ids {
				rows[id] = rwaContractRow(id, nil)
				supplies[id] = rwaDeJTRSYSupply
				decimals[id] = 18
			}
			srv := v1.New(v1.Options{
				Sep1Cache: &stubSep1BoundReader{},
				Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{}},
				AssetsReader: &rwaListStub{
					stubAssetsReaderExt: &stubAssetsReaderExt{},
					byIssuer:            map[string][]timescale.AssetRow{},
				},
				RWAContracts:      &stubRWAContractReader{accounts: 18000},
				RWAListings:       &stubRWAListings{rows: tc.listing},
				ContractCatalogue: &stubContractCatalogue{rows: rows},
				// Every candidate's on-chain symbol says deJTRSY; only the
				// address may decide.
				TokenSymbol: &stubTokenSymbols{byID: map[string]string{
					rwaDeJTRSY: "deJTRSY", rwaDeJAAA: "deJAAA", rwaDeJTRSYImpostor: "deJTRSY",
				}},
				TokenSupply:           &stubTokenSupplies{byID: supplies},
				TokenDecimals:         &stubTokenDecimalsRdr{byID: decimals},
				NonstandardDecimals:   confirmedNonstandardDecimals(t, decimals),
				MinMarketCapVolumeUSD: 1000,
			})
			view := getRWA(t, srv)

			for _, id := range tc.wantServed {
				a, ok := assetByContract(view, id)
				if !ok {
					t.Fatalf("%s not served; refusals: %+v", id, view.Refused)
				}
				if a.Basis != rwa.BasisCuratedContract || a.Recognition != rwa.RecognitionListingCorroborated {
					t.Errorf("basis/recognition = %q/%q", a.Basis, a.Recognition)
				}
				if a.AnchorClass != "bond" {
					t.Errorf("anchor_class = %q, want bond", a.AnchorClass)
				}
				if a.Code != "" || a.Issuer != "" {
					t.Errorf("contract row carries a classic identity: code %q issuer %q", a.Code, a.Issuer)
				}
				if a.Name != "Janus Henderson Anemoy Treasury Fund, Centrifuge deRWA token (deJTRSY)" {
					t.Errorf("name = %q, want the curated binding's instrument", a.Name)
				}
				if a.Decimals == nil || *a.Decimals != 18 {
					t.Fatalf("decimals = %s, want 18", decimalsText(a.Decimals))
				}
				if a.CirculatingSupply == nil || *a.CirculatingSupply != rwaDeJTRSYSupply {
					t.Errorf("circulating_supply = %v, want the raw SEP-41 total %s", a.CirculatingSupply, rwaDeJTRSYSupply)
				}
				// 8,763,619.974700234898508352 * 1.02, as a decimal string.
				if a.ReferenceValuation.ValueUSD == nil || *a.ReferenceValuation.ValueUSD != "8938892.37" {
					t.Errorf("reference_valuation.value_usd = %v, want 8938892.37", a.ReferenceValuation.ValueUSD)
				}
			}
			for _, id := range tc.wantAbsent {
				if _, ok := assetByContract(view, id); ok {
					t.Errorf("%s was served", id)
				}
			}
			// Every binding the listing does not name is refused and counted,
			// so an unlisted deJAAA is visible rather than silent.
			want := len(rwa.ContractInstrumentBindings()) - len(tc.wantServed)
			if n := dropCount(view, "curated_contract_bindings", rwa.RejectContractCuratedNotListed); n != want {
				t.Errorf("bound-but-unlisted refusals = %d, want %d", n, want)
			}
		})
	}
}
