package timescale

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// An issuer's wash trades between the SAC wrappers of its own credits are as unroutable as the same trades between the classic forms:
// the alias guard keeps markNoMarket off a wrapper, so only the issuer rule can tell this from a coverage gap.
func TestUsdPopulatedLabel_SameIssuerSACWrappersAreUnroutable(t *testing.T) {
	const (
		issuer      = "GBQR7IMFKZHNTQDRZZU3E3XHDX2IPS3MI3QZJ47I2HBMKEF5AOPEYMRI"
		bltaSAC     = "CCUYL75XNUTAHFE3RZAN7NEFQWZ7OIRJZNN4KUXXPA34RAHVUGLTBGT5"
		bltbSAC     = "CAANCS7TKC7IFWLRACKL4T4HMNMDUWSWNOL5EV244TVQCU3O7BM7N4VD"
		otherIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		otherSAC    = "CAAVZKRT2BJ5A5Y5SAGQNA3IC5EDAODEWBHO5CYR5ASLFDZYOQN6NEO3"
	)
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{
		bltaSAC: "BLTA:" + issuer, bltbSAC: "BLTB:" + issuer, otherSAC: "USTRY:" + otherIssuer,
	})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	soroban := func(id string) canonical.Asset { return canonical.Asset{Type: canonical.AssetSoroban, ContractID: id} }
	blta := canonical.Asset{Type: canonical.AssetClassic, Code: "BLTA", Issuer: issuer}
	unlisted := soroban("CB2J5FHCJJBQE5UPQ5FPZAWIEQD5KKP4GWLZZ3QT5EFVIT3FW5CI2OGK")
	tests := []struct {
		name string
		pair canonical.Pair
		want string
	}{
		{"both wrappers of one issuer", canonical.Pair{Base: soroban(bltaSAC), Quote: soroban(bltbSAC)}, "unroutable"},
		{"wrapper against its issuer's classic credit", canonical.Pair{Base: soroban(bltbSAC), Quote: blta}, "unroutable"},
		{"wrappers of two issuers", canonical.Pair{Base: soroban(bltaSAC), Quote: soroban(otherSAC)}, "no"},
		{"undeclared contract never matches", canonical.Pair{Base: unlisted, Quote: soroban(bltaSAC)}, "no"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := usdPopulatedLabel(tc.pair, false, false, false); got != tc.want {
				t.Errorf("usdPopulatedLabel = %q, want %q", got, tc.want)
			}
		})
	}
}
