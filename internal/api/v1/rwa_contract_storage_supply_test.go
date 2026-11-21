package v1

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// TestContractArmKeepsTheEventReadingWhenThereIsHistory is the control: a log whose net is at or above the storage level
// answers, and the two bases are never summed.
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
}

// TestContractArmServesTheStorageFloorWhenTheLogIsProvablyPartial: storage is a floor, so a level above the event net proves the
// log missed issuance. The row must take the storage reading (a lower-bound basis) and its cap must carry the bound.
func TestContractArmServesTheStorageFloorWhenTheLogIsProvablyPartial(t *testing.T) {
	const contractID = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
	net := big.NewInt(10_000_0000000)
	held := big.NewInt(1_010_000_0000000)
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: net, Mint: net, Burn: big.NewInt(0), FlowCount: 12},
		}}, ContractStorageSupply: &storageSupplyStub{byContract: map[string]clickhouse.ContractStorageSupply{
			contractID: {ContractID: contractID, Total: held, BalanceEntries: 40},
		}}},
		logger: discardLogger(),
	}
	price := "1.00"
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7, PriceUSD: &price}}

	s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}})

	if rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != held.String() {
		t.Fatalf("circulating_supply = %v, want the storage level %s, not the partial event net %s",
			rows[0].CirculatingSupply, held, net)
	}
	if rows[0].SupplyBasis == nil || *rows[0].SupplyBasis != supply.BasisContractStorageBalances.String() {
		t.Errorf("supply_basis = %v, want %q", rows[0].SupplyBasis, supply.BasisContractStorageBalances)
	}
	if rows[0].MarketCapUSD == nil || *rows[0].MarketCapUSD != "1010000.00" {
		t.Fatalf("market_cap_usd = %v, want 1010000.00", rows[0].MarketCapUSD)
	}
	if !rows[0].MarketCapLowerBound {
		t.Error("market_cap_lower_bound is false on a cap built from a floor supply")
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
		{"the reader refused a SAC", clickhouse.ContractStorageSupply{}, clickhouse.ErrStorageSupplyIsStellarAsset},
		{"the reader refused a holder set past its cap", clickhouse.ContractStorageSupply{}, clickhouse.ErrStorageSupplyTooManyEntries},
		{"the reader refused an undecodable entry", clickhouse.ContractStorageSupply{}, clickhouse.ErrStorageSupplyUndecodable},
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

// A failed storage read could have proved the event net partial, so the member is unread, not valued on the net.
func TestContractArmCountsAFailedStorageReadAsACutWhenTheLogHasFlows(t *testing.T) {
	const contractID = "CB53TITR65ZKTREWEWEEKMFAQZAE6IPJONH32JWOFOWJPHZL6ODP6R4F"
	net := big.NewInt(10_000_0000000)
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: net, Mint: net, Burn: big.NewInt(0), FlowCount: 12},
		}}, ContractStorageSupply: &storageSupplyStub{err: errors.New("clickhouse down")}},
		logger: discardLogger(),
	}
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

	if cut := s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}}); !cut {
		t.Error("cut = false after the storage read failed")
	}
	if rows[0].CirculatingSupply != nil {
		t.Errorf("supply = %v, want none: the net is unproven", *rows[0].CirculatingSupply)
	}
}

// A flowless log's only reading is storage, so a storage read that failed leaves the member unread, not "no figure".
func TestContractArmCountsAFailedStorageReadAsACutWhenTheLogIsFlowless(t *testing.T) {
	const contractID = "CB42I4CUI4VHF5JEOOAPGDS7WKHTZ3W6ODUKC2ARTMERGAE3KAIAWY4H"
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: big.NewInt(0), FlowCount: 0},
		}}, ContractStorageSupply: &storageSupplyStub{err: errors.New("clickhouse: memory limit exceeded")}},
		logger: discardLogger(),
	}
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

	if cut := s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}}); !cut {
		t.Error("cut = false after the only supply reading failed; the view would cache the row as unmeasured")
	}
	if _, ok, err := s.storageReadingSupersedes(context.Background(), contractID, clickhouse.TokenSupply{Total: big.NewInt(0)}); ok || err == nil {
		t.Errorf("storageReadingSupersedes(flowless, failed read) = ok %v err %v, want the error", ok, err)
	}
}

// An entry the reader cannot decode is a fixed lake property: it declines the storage reading, and a log with flows is valued on its net.
func TestContractArmValuesTheNetWhenStorageHoldsAnUndecodableEntry(t *testing.T) {
	const contractID = "CB53TITR65ZKTREWEWEEKMFAQZAE6IPJONH32JWOFOWJPHZL6ODP6R4F"
	net := big.NewInt(10_000_0000000)
	undecodable := fmt.Errorf("%w: %s: balance entry carries no decodable amount", clickhouse.ErrStorageSupplyUndecodable, contractID)
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: net, Mint: net, Burn: big.NewInt(0), FlowCount: 12},
		}}, ContractStorageSupply: &storageSupplyStub{err: undecodable}},
		logger: discardLogger(),
	}
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

	if cut := s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}}); cut {
		t.Error("cut = true for a deterministic decline; the view would stay degraded on every rebuild")
	}
	if rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != net.String() {
		t.Fatalf("circulating_supply = %v, want the event net %s", rows[0].CirculatingSupply, net)
	}
	if rows[0].SupplyBasis == nil || *rows[0].SupplyBasis != supply.BasisSEP41LakeFlows.String() {
		t.Errorf("supply_basis = %v, want %q", rows[0].SupplyBasis, supply.BasisSEP41LakeFlows)
	}
}

// The generic listing fill runs first and may stamp an observer supply on a contract row. When the contract reading refuses
// or is cut, that other basis must not be served, or it would feed the cap and the reference valuation beside a no_supply status.
func TestContractArmClearsTheGenericSupplyWhenItHasNoReading(t *testing.T) {
	const a = "CB53TITR65ZKTREWEWEEKMFAQZAE6IPJONH32JWOFOWJPHZL6ODP6R4F"
	const b = "CB42I4CUI4VHF5JEOOAPGDS7WKHTZ3W6ODUKC2ARTMERGAE3KAIAWY4H"
	net := big.NewInt(10_000_0000000)
	stamped := func(id string) AssetDetail {
		circ, basis, mc := "5000000000000", supply.BasisSEP41TotalOnly.String(), "500000.00"
		return AssetDetail{
			AssetID: id, Kind: "soroban_token", Decimals: 7,
			CirculatingSupply: &circ, SupplyBasis: &basis, MarketCapUSD: &mc, MarketCapLowLiquidity: true,
		}
	}
	for _, tt := range []struct {
		name    string
		supply  map[string]clickhouse.TokenSupply
		storage *storageSupplyStub
		cancel  bool
	}{
		{"incomplete flow net", map[string]clickhouse.TokenSupply{
			a: {Total: big.NewInt(-5), Incomplete: true, FlowCount: 3}, b: {Total: big.NewInt(-5), Incomplete: true, FlowCount: 3},
		}, nil, false},
		{"failed storage read on a log with flows", map[string]clickhouse.TokenSupply{
			a: {Total: net, Mint: net, FlowCount: 12}, b: {Total: net, Mint: net, FlowCount: 12},
		}, &storageSupplyStub{err: errors.New("clickhouse down")}, false},
		{"budget spent on the first row", map[string]clickhouse.TokenSupply{
			a: {Total: net, Mint: net, FlowCount: 12}, b: {Total: net, Mint: net, FlowCount: 12},
		}, &storageSupplyStub{err: context.Canceled}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := Options{TokenSupply: &lakeSupplyStub{byContract: tt.supply}}
			if tt.storage != nil {
				if tt.cancel {
					tt.storage.onAsk = cancel
				}
				opts.ContractStorageSupply = tt.storage
			}
			s := &Server{Options: opts, logger: discardLogger()}
			rows := []AssetDetail{stamped(a), stamped(b)}

			s.fillContractMarketCaps(ctx, rows, nil, map[string]struct{}{a: {}, b: {}})

			for _, r := range rows {
				if r.CirculatingSupply != nil || r.SupplyBasis != nil || r.MarketCapUSD != nil || r.MarketCapLowLiquidity {
					t.Errorf("%s: supply = %v basis = %v cap = %v low_liquidity = %v, want none of the generic fill's figures",
						r.AssetID, r.CirculatingSupply, r.SupplyBasis, r.MarketCapUSD, r.MarketCapLowLiquidity)
				}
			}
		})
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

type failingTokenDecimals struct{}

func (failingTokenDecimals) TokenDecimals(context.Context, string) (uint32, bool, error) {
	return 0, false, errors.New("lake read failed")
}

// TestFillContractDecimals_AReadingOverridesTheProvisionalVerdict: the generic listing fill marks the scale unresolved
// when its own read fails; the contract arm's successful reading owns the scale and clears that verdict, so a row never
// publishes a cap beside `decimals: null`. A read that errors is unread, not a quiet refusal.
func TestFillContractDecimals_AReadingOverridesTheProvisionalVerdict(t *testing.T) {
	const id = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	src := map[string]timescale.AssetRow{id: {}}

	s := &Server{TokenDecimals: fixedTokenDecimals(7)}
	rows := []AssetDetail{{AssetID: id, DecimalsUnresolved: true}}
	resolved, cut := s.fillContractDecimals(context.Background(), rows, src)
	if _, ok := resolved[id]; !ok || cut {
		t.Fatalf("resolved = %v, cut = %v, want the reading resolved", resolved, cut)
	}
	if rows[0].DecimalsUnresolved || rwaDecimalsOf(rows[0]) == nil {
		t.Errorf("DecimalsUnresolved = %v after a successful reading, decimals = %v", rows[0].DecimalsUnresolved, rwaDecimalsOf(rows[0]))
	}

	s = &Server{TokenDecimals: failingTokenDecimals{}, logger: discardLogger()}
	rows = []AssetDetail{{AssetID: id}}
	if _, cut := s.fillContractDecimals(context.Background(), rows, src); !cut {
		t.Error("a failed decimals read was not reported as unread")
	}
}

type undeclaredTokenDecimals struct{}

func (undeclaredTokenDecimals) TokenDecimals(context.Context, string) (uint32, bool, error) {
	return 0, false, nil
}

// TestFillContractDecimals_UnknownScaleWithoutSupplyIsNotServedAsSeven: a contract with no declared or readable
// scale and no supply reading still has the stamped 7 on its row; it must reach the wire as absent, not 7.
func TestFillContractDecimals_UnknownScaleWithoutSupplyIsNotServedAsSeven(t *testing.T) {
	const id = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	src := map[string]timescale.AssetRow{id: {}}
	for name, reader := range map[string]TokenDecimalsReader{
		"undeclared":  undeclaredTokenDecimals{},
		"read failed": failingTokenDecimals{},
	} {
		s := &Server{TokenDecimals: reader, logger: discardLogger()}
		rows := []AssetDetail{{AssetID: id, Type: "soroban", Decimals: 7}}
		scaled, _ := s.fillContractDecimals(context.Background(), rows, src)
		refuseUnscaledContractCaps(rows, scaled)
		if got := rwaDecimalsOf(rows[0]); got != nil {
			t.Errorf("%s: decimals = %d, want absent", name, *got)
		}
	}
}

// TestRWAContractListingRows_ValuesAnAdmittedContractDiscoveryNeverSighted: a token emitting no SEP-41 events has no
// catalogue row, and membership already proved its address, so the catalogue may not gate its valuation, whether it
// holds no row or is not wired: the storage supply fallback built for exactly these tokens must reach it.
func TestRWAContractListingRows_ValuesAnAdmittedContractDiscoveryNeverSighted(t *testing.T) {
	const contractID = "CAOCXWNXCG63U43DCJSS52FDAFT2ZXY2T2UZF3ZYVZ4LLLFVFRF2GM6Z"
	held, _ := new(big.Int).SetString("344995973100000", 10)
	for name, catalogue := range map[string]ContractCatalogueReader{
		"no catalogue row":  capDecimalsContractCatalogue{rows: map[string]timescale.AssetRow{}},
		"catalogue unwired": nil,
	} {
		s := &Server{
			Options: Options{
				ContractCatalogue: catalogue,
				TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
					contractID: {ContractID: contractID, Total: big.NewInt(0), FlowCount: 0},
				}},
				ContractStorageSupply: &storageSupplyStub{byContract: map[string]clickhouse.ContractStorageSupply{
					contractID: {ContractID: contractID, Total: held, BalanceEntries: 6},
				}},
				TokenDecimals: fixedTokenDecimals(7),
			},
			logger: discardLogger(),
		}
		out, _, err := s.rwaContractListingRows(context.Background(), []rwaContractMember{{contractID: contractID}})
		if err != nil {
			t.Fatal(err)
		}
		row, ok := out[contractID]
		if !ok {
			t.Errorf("%s: no row for the admitted contract: its whole value is in no total", name)
			continue
		}
		if row.CirculatingSupply == nil || *row.CirculatingSupply != held.String() {
			t.Errorf("%s: circulating_supply = %v, want the storage sum %s", name, row.CirculatingSupply, held)
		}
	}
}
