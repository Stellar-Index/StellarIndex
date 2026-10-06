package v1

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type stubIssuerFlags map[string]clickhouse.AccountAuthFlags

func (m stubIssuerFlags) BulkAccountAuthFlags(context.Context, []string) (map[string]clickhouse.AccountAuthFlags, error) {
	return m, nil
}

type stubTokenSupply struct {
	byContract map[string]clickhouse.TokenSupply
	err        error
}

func (s *stubTokenSupply) TokenSupply(_ context.Context, contractID string) (clickhouse.TokenSupply, error) {
	return s.byContract[contractID], s.err
}

func (s *stubTokenSupply) NativeTotalCoins(context.Context) (int64, uint32, error) { return 0, 0, nil }

const issuerTestIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func TestIssuerBehaviour_FlagsAndFlowTotals(t *testing.T) {
	contractID, ok := classicSACContractID(globalTestUSDC)
	if !ok {
		t.Fatal("no SAC id for the test asset")
	}
	s := &Server{
		Options: Options{IssuerAuthFlags: stubIssuerFlags{issuerTestIssuer: {
			Revocable: true, Clawback: true, Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 61_000_000,
		}}, TokenSupply: &stubTokenSupply{byContract: map[string]clickhouse.TokenSupply{contractID: {
			ContractID: contractID, Mint: new(big.Int).Lsh(big.NewInt(1), 100), Burn: big.NewInt(7),
			Clawback: big.NewInt(3), Total: big.NewInt(0), FlowCount: 12,
		}}}},
	}
	d := AssetDetail{AssetID: globalTestUSDC}
	s.applyIssuerBehaviour(context.Background(), &d)
	b := d.IssuerBehaviour
	if b == nil {
		t.Fatal("issuer_behaviour absent")
	}
	if *b.AuthRequired || !*b.AuthRevocable || !*b.AuthClawbackEnabled || *b.AuthImmutable || *b.FlagsAsOfLedger != 61_000_000 {
		t.Errorf("flags = req %v rev %v claw %v imm %v as_of %v", *b.AuthRequired, *b.AuthRevocable,
			*b.AuthClawbackEnabled, *b.AuthImmutable, *b.FlagsAsOfLedger)
	}
	// Σmint above 2^64 must survive verbatim (ADR-0003).
	if *b.MintTotal != "1267650600228229401496703205376" || *b.BurnTotal != "7" || *b.ClawbackTotal != "3" || *b.SupplyFlowCount != 12 {
		t.Errorf("totals = mint %s burn %s clawback %s flows %d", *b.MintTotal, *b.BurnTotal, *b.ClawbackTotal, *b.SupplyFlowCount)
	}
}

// An unresolved part is omitted, never served as false or zero.
func TestIssuerBehaviour_OmitsWhatDoesNotResolve(t *testing.T) {
	contractID, _ := classicSACContractID(globalTestUSDC)
	incomplete := &stubTokenSupply{byContract: map[string]clickhouse.TokenSupply{contractID: {
		Mint: big.NewInt(1), Burn: big.NewInt(5), Clawback: big.NewInt(0), FlowCount: 2, Incomplete: true,
	}}}
	removed := stubIssuerFlags{issuerTestIssuer: {Clawback: true, Source: clickhouse.AuthFlagsSourceLastKnownBeforeRemoval}}
	for name, s := range map[string]*Server{
		"nothing wired":     {},
		"read error":        {Options: Options{IssuerAuthFlags: stubIssuerFlags{}, TokenSupply: &stubTokenSupply{err: errors.New("boom")}}},
		"incomplete flows":  {Options: Options{IssuerAuthFlags: stubIssuerFlags{}, TokenSupply: incomplete}},
		"not a live record": {Options: Options{IssuerAuthFlags: removed}},
	} {
		d := AssetDetail{AssetID: globalTestUSDC}
		s.applyIssuerBehaviour(context.Background(), &d)
		if d.IssuerBehaviour != nil {
			t.Errorf("%s: issuer_behaviour = %+v, want omitted", name, d.IssuerBehaviour)
		}
	}
	d := AssetDetail{AssetID: "native"}
	(&Server{Options: Options{IssuerAuthFlags: removed}}).applyIssuerBehaviour(context.Background(), &d)
	if d.IssuerBehaviour != nil {
		t.Error("native asset got issuer_behaviour")
	}
}
