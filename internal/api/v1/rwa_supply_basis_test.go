package v1

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Every reference_valuation is circulating_supply times an oracle price, so a row must carry the supply basis and a
// floor marker, not just the supply. Tests drive the production fills and projection to pin that the arm name reaches
// the wire.

// TestRWARowDeclaresTheTrustlineFallbackAsAFloor: when the lake cannot answer (cold cache) the figure is the trustline
// sum, blind to claimable balances, LP reserves and SAC supply. Asserts the declaration, not the number: serving it as a
// floor is defensible, under a four-domain total's shape is not.
func TestRWARowDeclaresTheTrustlineFallbackAsAFloor(t *testing.T) {
	s, _ := lakeSupplyServer(t, "") // no lake reading for CETES at all
	rows := cetesRow()
	rows[0].PriceUSD = nil // no price → the market-cap fill returns before it looks supply up

	s.rwaFillMissingSupply(context.Background(), rows)

	if rows[0].CirculatingSupply == nil {
		t.Fatalf("no circulating supply served at all; row=%+v", rows[0])
	}
	if got := *rows[0].CirculatingSupply; got != cetesTrustlineOnly {
		t.Fatalf("circulating_supply = %s, want the trustline sum %s", got, cetesTrustlineOnly)
	}

	a := rwaRowFor(t, s, rows[0])
	if a.SupplyBasis != supply.BasisClassicTrustlineSum.String() {
		t.Errorf("supply_basis = %q, want %q — a consumer cannot tell a floor from a total "+
			"when the wire names neither", a.SupplyBasis, supply.BasisClassicTrustlineSum)
	}
	if !a.CirculatingSupplyLowerBound {
		t.Error("circulating_supply_lower_bound is false on a trustline-only figure; the " +
			"trustline sum cannot see claimable balances, LP reserves or SAC-held supply, " +
			"and publishing it as if it were complete understated PYUSD by 73% and XRF by 82%")
	}
}

// TestRWARowDoesNotMarkAFourDomainTotalAsAFloor is the control. A lower-bound marker on every row
// carries as much information as one on no row, so the lake arm, which cannot miss a holding
// domain because a flow does not know where the token came to rest, must come back unmarked.
func TestRWARowDoesNotMarkAFourDomainTotalAsAFloor(t *testing.T) {
	s, _ := lakeSupplyServer(t, cetesTrueSupply)
	rows := cetesRow()
	rows[0].PriceUSD = nil

	s.rwaFillMissingSupply(context.Background(), rows)

	if rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != cetesTrueSupply {
		t.Fatalf("circulating_supply = %v, want the lake total %s", rows[0].CirculatingSupply, cetesTrueSupply)
	}
	a := rwaRowFor(t, s, rows[0])
	if a.SupplyBasis != supply.BasisClassicLakeFlows.String() {
		t.Errorf("supply_basis = %q, want %q", a.SupplyBasis, supply.BasisClassicLakeFlows)
	}
	if a.CirculatingSupplyLowerBound {
		t.Error("circulating_supply_lower_bound set on the lake-flows total, which covers " +
			"trustlines, claimable balances, LP reserves and SAC-held supply alike")
	}
}

// TestRWARowWithNoBasisNamesNoArm: a supply attached by a branch that carries no basis (the
// dust-suppressed and ticker-collision paths) reaches the wire with no provenance, and the
// projection must not invent one; naming an arm that did not answer is worse than naming none.
func TestRWARowWithNoBasisNamesNoArm(t *testing.T) {
	circ := "5000000000"
	s, _ := lakeSupplyServer(t, "")
	d := cetesRow()[0]
	d.CirculatingSupply = &circ
	d.SupplyBasis = nil

	a := rwaRowFor(t, s, d)

	if a.CirculatingSupply == nil || *a.CirculatingSupply != circ {
		t.Fatalf("circulating_supply = %v, want %s — the fact is still served", a.CirculatingSupply, circ)
	}
	if a.SupplyBasis != "" {
		t.Errorf("supply_basis = %q on a row nothing stamped a basis onto", a.SupplyBasis)
	}
	if a.CirculatingSupplyLowerBound {
		t.Error("circulating_supply_lower_bound asserted about a reading whose arm is unknown")
	}
}

// TestRWAContractRowNamesTheFlowSum covers the contract arm end to end: its supply comes from a different reader and
// population, so it must carry a basis too or contract and classic rows look alike on the wire.
func TestRWAContractRowNamesTheFlowSum(t *testing.T) {
	const contractID = "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA"
	total, _ := new(big.Int).SetString("123450000000", 10)
	s := &Server{
		Options: Options{TokenSupply: &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{
			contractID: {ContractID: contractID, Total: total, Mint: total, Burn: big.NewInt(0), Clawback: big.NewInt(0), FlowCount: 7},
		}}},
		logger: slog.Default(),
	}
	rows := []AssetDetail{{AssetID: contractID, Kind: "soroban_token", Decimals: 7}}

	s.fillContractMarketCaps(context.Background(), rows, nil, map[string]struct{}{contractID: {}})

	out := rwaContractAssetRows(
		[]rwaContractMember{{contractID: contractID, symbol: "XYZ"}},
		map[string]AssetDetail{contractID: rows[0]},
	)
	if len(out) != 1 {
		t.Fatalf("rows = %d, want 1", len(out))
	}
	if out[0].CirculatingSupply == nil || *out[0].CirculatingSupply != total.String() {
		t.Fatalf("circulating_supply = %v, want %s", out[0].CirculatingSupply, total)
	}
	if out[0].SupplyBasis != supply.BasisSEP41LakeFlows.String() {
		t.Errorf("supply_basis = %q, want %q", out[0].SupplyBasis, supply.BasisSEP41LakeFlows)
	}
	if out[0].CirculatingSupplyLowerBound {
		t.Error("circulating_supply_lower_bound set on an event-flow total, which covers " +
			"every holder at once because a mint does not know where the token came to rest")
	}
}

// TestRWARowOmitsSupplyProvenanceWhenThereIsNone guards serialisation: both fields are omitempty, so a row with no
// supply must not grow `supply_basis: ""` or `circulating_supply_lower_bound: false`, which a consumer would read as a
// statement.
func TestRWARowOmitsSupplyProvenanceWhenThereIsNone(t *testing.T) {
	b, err := json.Marshal(RWAAsset{AssetID: "FOO-GAFOO"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"supply_basis", "circulating_supply_lower_bound"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s present on a row with no circulating supply: %s", k, b)
		}
	}
}

// rwaRowFor runs the production projection the handler calls over one filled listing row, so tests cannot pass
// against a projection that drops fields; a hand-built RWAAsset would only assert the struct has them.
func rwaRowFor(t *testing.T, s *Server, d AssetDetail) RWAAsset {
	t.Helper()
	mem := rwaMember{code: "CETES", issuer: cetesIssuer, name: "Cetes", basis: "sep1_declaration", anchorClass: "bond"}
	out, notObserved := s.rwaAssetRows(rwaMembership{members: []rwaMember{mem}},
		map[string]AssetDetail{rwaKey(mem.code, mem.issuer): d}, time.Now())
	if notObserved != 0 || len(out) != 1 {
		t.Fatalf("projection returned %d rows (%d not observed), want 1", len(out), notObserved)
	}
	return out[0]
}
