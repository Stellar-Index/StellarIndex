package xdrjson

import "testing"

const pubnet = "Public Global Stellar Network ; September 2015"

func TestSACContractID(t *testing.T) {
	cases := []struct {
		asset string
		want  string
	}{
		// Native XLM's SAC on pubnet (well-known).
		{"native", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"},
		// USDC (Circle issuer) SAC — appears as a Blend reserve.
		{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"},
	}
	for _, c := range cases {
		got, ok := SACContractID(c.asset, pubnet)
		if !ok {
			t.Errorf("SACContractID(%q) ok=false", c.asset)
			continue
		}
		if got != c.want {
			t.Errorf("SACContractID(%q) = %s, want %s", c.asset, got, c.want)
		}
	}
	// Non-SAC assets → ok=false.
	for _, a := range []string{"fiat:USD", "crypto:BTC", "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"} {
		if _, ok := SACContractID(a, pubnet); ok {
			t.Errorf("SACContractID(%q) ok=true, want false", a)
		}
	}
}
