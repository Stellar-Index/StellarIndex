package v1

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// storageSupplyStub answers the per-contract storage read.
type storageSupplyStub struct {
	byContract map[string]clickhouse.ContractStorageSupply
	err        error
	asked      []string
}

func (s *storageSupplyStub) ContractStorageSupply(
	_ context.Context, contractID string,
) (clickhouse.ContractStorageSupply, error) {
	s.asked = append(s.asked, contractID)
	if s.err != nil {
		return clickhouse.ContractStorageSupply{}, s.err
	}
	return s.byContract[contractID], nil
}

// TestContractArmReadsStorageWhenTheEventLogIsEmpty is the regression this
// path exists for. A token that emits no SEP-41 events is not UNDERCOUNTED by
// the event reader, it is ABSENT from it — and an absent contract sums to a
// confident zero, served as certainly as a measurement. Twenty-four
// private-credit deal tokens on pubnet were in exactly that state, reporting a
// total supply of 0 while their storage held 548,113,042.88 tokens.
//
// Red before the fix: the row published "0" on the sep41_lake_flows basis.
func TestContractArmReadsStorageWhenTheEventLogIsEmpty(t *testing.T) {
	const contractID = "CAOCXWNXCG63U43DCJSS52FDAFT2ZXY2T2UZF3ZYVZ4LLLFVFRF2GM6Z"
	// The six non-zero Balance entries measured for this exact contract.
	held, _ := new(big.Int).SetString("344995973100000", 10)
	s := &Server{
		logger: discardLogger(),
		tokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			// What the event reader returns for a contract with no events:
			// a zero total over zero flows. Not an error, not incomplete.
			contractID: {ContractID: contractID, Total: big.NewInt(0), FlowCount: 0},
		}},
		storageSupply: &storageSupplyStub{byContract: map[string]clickhouse.ContractStorageSupply{
			contractID: {ContractID: contractID, Total: held, BalanceEntries: 6},
		}},
	}
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

	s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}})

	if rows[0].CirculatingSupply == nil {
		t.Fatal("no supply served at all")
	}
	if got := *rows[0].CirculatingSupply; got != held.String() {
		t.Errorf("circulating_supply = %s, want the storage sum %s — a confident zero "+
			"reads as a fully-burned token and understates it by the whole float", got, held)
	}
	if rows[0].SupplyBasis == nil || *rows[0].SupplyBasis != supply.BasisContractStorageBalances.String() {
		t.Errorf("supply_basis = %v, want %q — the two are different bases, not two "+
			"readings of one, and the wire has to say which was taken",
			rows[0].SupplyBasis, supply.BasisContractStorageBalances)
	}
}

// TestContractArmKeepsTheEventReadingWhenThereIsHistory is the control, and it
// is the half that keeps the two bases from being summed or swapped. A token
// whose log HAS flows is answered by the log; storage is never consulted, so
// the same tokens can never be counted twice.
func TestContractArmKeepsTheEventReadingWhenThereIsHistory(t *testing.T) {
	const contractID = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	total, _ := new(big.Int).SetString("28327867109034", 10)
	storage := &storageSupplyStub{byContract: map[string]clickhouse.ContractStorageSupply{
		contractID: {ContractID: contractID, Total: big.NewInt(999), BalanceEntries: 1},
	}}
	s := &Server{
		logger: discardLogger(),
		tokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: total, Mint: total, Burn: big.NewInt(0), FlowCount: 1828},
		}},
		storageSupply: storage,
	}
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 5}}

	s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}})

	if rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != total.String() {
		t.Fatalf("circulating_supply = %v, want the event total %s", rows[0].CirculatingSupply, total)
	}
	if rows[0].SupplyBasis == nil || *rows[0].SupplyBasis != supply.BasisSEP41LakeFlows.String() {
		t.Errorf("supply_basis = %v, want %q", rows[0].SupplyBasis, supply.BasisSEP41LakeFlows)
	}
	if len(storage.asked) != 0 {
		t.Errorf("storage was read for a token whose event log has %d flows; the two bases "+
			"hold the SAME tokens, so consulting both is how they get summed", 1828)
	}
}

// TestContractArmRefusesAnEmptyStorageRead — no balance entries is not a
// supply of zero, it is the absence of a reading. Publishing it would replace
// one unfounded zero with another, which is the whole defect this path removes.
// The row keeps whatever the event reader said, unchanged.
func TestContractArmRefusesAnEmptyStorageRead(t *testing.T) {
	const contractID = "CB53TITR65ZKTREWEWEEKMFAQZAE6IPJONH32JWOFOWJPHZL6ODP6R4F"
	for _, tt := range []struct {
		name string
		st   clickhouse.ContractStorageSupply
		err  error
	}{
		{"no entries", clickhouse.ContractStorageSupply{Total: big.NewInt(0), BalanceEntries: 0}, nil},
		{"entries but a nil total", clickhouse.ContractStorageSupply{BalanceEntries: 3}, nil},
		{"entries summing to zero", clickhouse.ContractStorageSupply{Total: big.NewInt(0), BalanceEntries: 3}, nil},
		{"the reader refused (a SAC, or a holder set past its cap)", clickhouse.ContractStorageSupply{}, errors.New("declined")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				logger: discardLogger(),
				tokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
					contractID: {ContractID: contractID, Total: big.NewInt(0), FlowCount: 0},
				}},
				storageSupply: &storageSupplyStub{
					byContract: map[string]clickhouse.ContractStorageSupply{contractID: tt.st},
					err:        tt.err,
				},
			}
			rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

			s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}})

			if rows[0].SupplyBasis == nil || *rows[0].SupplyBasis != supply.BasisSEP41LakeFlows.String() {
				t.Errorf("supply_basis = %v, want the event basis left in place", rows[0].SupplyBasis)
			}
		})
	}
}

// TestContractArmWithNoStorageReaderWiredIsUnchanged — the reader is optional
// wiring, and a deployment without it must behave exactly as it did before
// this path existed rather than panicking on a nil seam.
func TestContractArmWithNoStorageReaderWiredIsUnchanged(t *testing.T) {
	const contractID = "CB42I4CUI4VHF5JEOOAPGDS7WKHTZ3W6ODUKC2ARTMERGAE3KAIAWY4H"
	s := &Server{
		logger: discardLogger(),
		tokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: big.NewInt(0), FlowCount: 0},
		}},
	}
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

	s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}})

	if rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != "0" {
		t.Errorf("circulating_supply = %v, want the unchanged pre-fallback reading", rows[0].CirculatingSupply)
	}
}
