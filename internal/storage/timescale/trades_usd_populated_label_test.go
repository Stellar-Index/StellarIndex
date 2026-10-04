package timescale

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestUsdPopulatedLabel(t *testing.T) {
	const (
		issuerA = "GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUSCQNBA3CULWJQJHOS6VKUMH"
		issuerB = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		sac     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	)
	classic := func(code, issuer string) canonical.Asset {
		return canonical.Asset{Type: canonical.AssetClassic, Code: code, Issuer: issuer}
	}
	tests := []struct {
		name      string
		pair      canonical.Pair
		populated bool
		thin      bool
		want      string
	}{
		{"priced same-issuer pair stays yes", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerA)}, true, false, "yes"},
		{"priced cross-issuer pair", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("USDC", issuerB)}, true, false, "yes"},
		{"unpriced same-issuer pair is unroutable", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerA)}, false, false, "unroutable"},
		{"unpriced cross-issuer pair is a gap", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, false, false, "no"},
		{"unpriced native leg is a gap", canonical.Pair{Base: classic("AAA", issuerA), Quote: canonical.NativeAsset()}, false, false, "no"},
		{"unpriced soroban legs are a gap", canonical.Pair{
			Base:  canonical.Asset{Type: canonical.AssetSoroban, ContractID: sac},
			Quote: canonical.Asset{Type: canonical.AssetSoroban, ContractID: sac},
		}, false, false, "no"},
		{"thin refusal on a cross-issuer pair", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, false, true, "thin"},
		{"unroutable wins over thin", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerA)}, false, true, "unroutable"},
		{"populated wins over thin", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, true, true, "yes"},
		{"empty issuers never match", canonical.Pair{Base: classic("AAA", ""), Quote: classic("BBB", "")}, false, false, "no"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := usdPopulatedLabel(tc.pair, tc.populated, tc.thin); got != tc.want {
				t.Errorf("usdPopulatedLabel = %q, want %q", got, tc.want)
			}
		})
	}
}
