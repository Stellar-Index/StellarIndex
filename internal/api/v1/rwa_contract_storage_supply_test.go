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
	onAsk      func()
}

func (s *storageSupplyStub) ContractStorageSupply(
	_ context.Context, contractID string,
) (clickhouse.ContractStorageSupply, error) {
	s.asked = append(s.asked, contractID)
	if s.onAsk != nil {
		s.onAsk()
	}
	if s.err != nil {
		return clickhouse.ContractStorageSupply{}, s.err
	}
	return s.byContract[contractID], nil
}

// TestContractArmReadsStorageWhenTheEventLogIsEmpty: a token emitting no SEP-41 events is absent from the event reader and
// would sum to a confident zero, so the contract arm must read storage instead.
// Red without the storage read: the row publishes "0" on the sep41_lake_flows basis.
func TestContractArmReadsStorageWhenTheEventLogIsEmpty(t *testing.T) {
	const contractID = "CAOCXWNXCG63U43DCJSS52FDAFT2ZXY2T2UZF3ZYVZ4LLLFVFRF2GM6Z"
	// The six non-zero Balance entries measured for this exact contract.
	held, _ := new(big.Int).SetString("344995973100000", 10)
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			// What the event reader returns for a contract with no events:
			// a zero total over zero flows. Not an error, not incomplete.
			contractID: {ContractID: contractID, Total: big.NewInt(0), FlowCount: 0},
		}}, ContractStorageSupply: &storageSupplyStub{byContract: map[string]clickhouse.ContractStorageSupply{
			contractID: {ContractID: contractID, Total: held, BalanceEntries: 6},
		}}},
		logger: discardLogger(),
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

// TestContractArmKeepsTheEventReadingWhenThereIsHistory is the control: a token whose log has flows is answered by the
// log and storage is never consulted, so the two bases are never summed or swapped.
func TestContractArmKeepsTheEventReadingWhenThereIsHistory(t *testing.T) {
	const contractID = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	total, _ := new(big.Int).SetString("28327867109034", 10)
	storage := &storageSupplyStub{byContract: map[string]clickhouse.ContractStorageSupply{
		contractID: {ContractID: contractID, Total: big.NewInt(999), BalanceEntries: 1},
	}}
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: total, Mint: total, Burn: big.NewInt(0), FlowCount: 1828},
		}}, ContractStorageSupply: storage},
		logger: discardLogger(),
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

// TestContractArmRefusesAnEmptyStorageRead: no balance entries is an absent reading, not a zero supply, and a flowless
// event log is not one either. The row carries no supply at all.
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
				Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
					contractID: {ContractID: contractID, Total: big.NewInt(0), FlowCount: 0},
				}}, ContractStorageSupply: &storageSupplyStub{
					byContract: map[string]clickhouse.ContractStorageSupply{contractID: tt.st},
					err:        tt.err,
				}},
				logger: discardLogger(),
			}
			rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

			s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}})

			if rows[0].CirculatingSupply != nil || rows[0].SupplyBasis != nil || rows[0].MarketCapUSD != nil {
				t.Errorf("supply = %v basis = %v cap = %v, want an unmeasured row",
					rows[0].CirculatingSupply, rows[0].SupplyBasis, rows[0].MarketCapUSD)
			}
		})
	}
}

// TestContractArmWithNoStorageReaderWiredPublishesNoZero: the reader is optional wiring. Without it a flowless contract is
// unmeasured, not fully burned, and a contract with flows is read as before.
func TestContractArmWithNoStorageReaderWiredPublishesNoZero(t *testing.T) {
	const flowless = "CB42I4CUI4VHF5JEOOAPGDS7WKHTZ3W6ODUKC2ARTMERGAE3KAIAWY4H"
	const flowing = "CB53TITR65ZKTREWEWEEKMFAQZAE6IPJONH32JWOFOWJPHZL6ODP6R4F"
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			flowless: {ContractID: flowless, Total: big.NewInt(0), FlowCount: 0},
			flowing:  {ContractID: flowing, Total: big.NewInt(0), FlowCount: 4},
		}}},
		logger: discardLogger(),
	}
	rows := []AssetDetail{
		{AssetID: flowless, Kind: "soroban_token", Decimals: 7},
		{AssetID: flowing, Kind: "soroban_token", Decimals: 7},
	}

	cut := s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{flowless: {}, flowing: {}})

	if cut {
		t.Error("cut = true on a live context")
	}
	if rows[0].CirculatingSupply != nil || rows[0].SupplyBasis != nil {
		t.Errorf("flowless: supply = %v basis = %v, want none", rows[0].CirculatingSupply, rows[0].SupplyBasis)
	}
	if rows[1].CirculatingSupply == nil || *rows[1].CirculatingSupply != "0" {
		t.Errorf("minted then fully burned: supply = %v, want 0 from the event log", rows[1].CirculatingSupply)
	}
}

// TestContractArmCountsAStorageDeadlineAsACut: a storage read that ran out of time is unread, so the walk reports a cut and
// the view degrades instead of caching the member as absent.
func TestContractArmCountsAStorageDeadlineAsACut(t *testing.T) {
	const contractID = "CB42I4CUI4VHF5JEOOAPGDS7WKHTZ3W6ODUKC2ARTMERGAE3KAIAWY4H"
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: big.NewInt(0), FlowCount: 0},
		}}, ContractStorageSupply: &storageSupplyStub{err: context.Canceled, onAsk: cancel}},
		logger: discardLogger(),
	}
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

	if cut := s.fillContractMarketCaps(ctx, rows, nil, map[string]struct{}{contractID: {}}); !cut {
		t.Error("cut = false after the storage read ended the context")
	}
	if rows[0].CirculatingSupply != nil {
		t.Errorf("supply = %v, want none", rows[0].CirculatingSupply)
	}
}
