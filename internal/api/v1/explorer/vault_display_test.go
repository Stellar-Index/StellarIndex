package explorer

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// greepVault is the mainnet DeFindex vault whose 58-holder cohort total (4405979358 base units = 440.5979358 GRPVLT) was served unscaled.
const greepVault = "CD4B5WJDJQ6G5K6MVC3VHTBI2PNLWJBWLXHV75S245Q3PIQWC262UZ4C"

type vaultMetaReader struct {
	capReader
	err error
}

func (r *vaultMetaReader) TokenDisplays(_ context.Context, tokens []string) (map[string]clickhouse.TokenDisplayMeta, error) {
	if r.err != nil {
		return nil, r.err
	}
	out := map[string]clickhouse.TokenDisplayMeta{}
	for _, t := range tokens {
		out[t] = clickhouse.TokenDisplayMeta{}
		if t == greepVault {
			out[t] = clickhouse.TokenDisplayMeta{Name: "DeFindex-Vault-Greep Vault", Symbol: "GRPVLT", Decimals: 7, HasMeta: true}
		}
	}
	return out, nil
}

type defindexRowsReader struct {
	*slowPositionsReader
	rows []timescale.DefindexVaultFold
}

func (r *defindexRowsReader) DefindexVaultSharesByUser(context.Context, string) ([]timescale.DefindexVaultFold, error) {
	return r.rows, nil
}

func TestCohortPositionsView_NamesAndScalesVaultShares(t *testing.T) {
	h := newProbeHandler(&vaultMetaReader{capReader: capReader{probe: &deadlineProbe{}}}, nil)
	got := h.cohortPositionsView(context.Background(), []clickhouse.AccountCohortPosition{
		{Protocol: "defindex", PositionKind: PositionKindVaultShares, Venue: greepVault, Holders: 58, Amount: big.NewInt(4405979358)},
		{Protocol: "defindex", PositionKind: PositionKindVaultShares, Venue: "CUNREAD", Holders: 1, Amount: big.NewInt(5)},
		{Protocol: "blend", PositionKind: PositionKindBackstopShares, Venue: greepVault, Holders: 1, Amount: big.NewInt(9)},
	})
	v := got[0]
	if v.VenueLabel != "Greep Vault (GRPVLT)" || v.Decimals == nil || *v.Decimals != 7 || v.Amount != "4405979358" {
		t.Fatalf("vault row = %+v (decimals %v), want label %q, decimals 7, base-unit amount", v, v.Decimals, "Greep Vault (GRPVLT)")
	}
	if got[1].Decimals != nil || got[1].VenueLabel != "" {
		t.Errorf("unreadable vault = %+v: must stay unscaled and unnamed, never a guessed 7", got[1])
	}
	if got[2].Decimals != nil || got[2].VenueLabel != "" {
		t.Errorf("non-vault kind = %+v: vault metadata must not leak onto other kinds", got[2])
	}
}

func TestCohortPositionsView_FailedMetadataReadLeavesSharesUnscaled(t *testing.T) {
	h := newProbeHandler(&vaultMetaReader{capReader: capReader{probe: &deadlineProbe{}}, err: errors.New("lake down")}, nil)
	got := h.cohortPositionsView(context.Background(), []clickhouse.AccountCohortPosition{
		{Protocol: "defindex", PositionKind: PositionKindVaultShares, Venue: greepVault, Holders: 58, Amount: big.NewInt(4405979358)},
	})
	if got[0].Decimals != nil {
		t.Errorf("decimals = %d after a failed read, want absent", *got[0].Decimals)
	}
}

func TestBuildDefindexPositions_NamesAndScalesFromTheVaultToken(t *testing.T) {
	positions := &defindexRowsReader{slowPositionsReader: &slowPositionsReader{}, rows: []timescale.DefindexVaultFold{
		{ContractID: greepVault, SharesNet: "4405979358"},
	}}
	h := newProbeHandler(&vaultMetaReader{capReader: capReader{probe: &deadlineProbe{}}}, positions)
	got := h.buildDefindexPositions(context.Background(), validTestAccount, &positionsCoverage{})
	if len(got) != 1 || got[0].VenueLabel != "Greep Vault (GRPVLT)" || got[0].Decimals == nil || *got[0].Decimals != 7 {
		t.Fatalf("got %+v, want the vault named and scaled at 7", got)
	}
}

func TestVaultLabel_RefusesImplausibleText(t *testing.T) {
	for _, c := range []struct{ name, symbol, want string }{
		{"DeFindex-Vault-Greep Vault", "GRPVLT", "Greep Vault (GRPVLT)"},
		{"Plain", "", "Plain"},
		{"bad\nname", "SYM", "SYM vault"},
		{"", "TWO WORDS", ""},
		{string(make([]rune, vaultNameMaxRunes+1)), "", ""},
	} {
		if got := vaultLabel(c.name, c.symbol); got != c.want {
			t.Errorf("vaultLabel(%q, %q) = %q, want %q", c.name, c.symbol, got, c.want)
		}
	}
}
