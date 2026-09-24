// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import "testing"

// Golden values verified against the live chain: the USDC SAC + the
// native (XLM) SAC are the two most-held wrapped assets on pubnet.
func TestSacContractID_Golden(t *testing.T) {
	usdc, err := NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	got, err := usdc.SacContractID()
	if err != nil {
		t.Fatalf("usdc: %v", err)
	}
	// The observed on-chain USDC SAC (internal/supply sac_wrappers,
	// lake-verified): a derivation mismatch means the passphrase or
	// XDR encoding is wrong.
	if want := "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"; got != want {
		t.Errorf("USDC SAC = %s, want %s", got, want)
	}

	xlm, err := NativeAsset().SacContractID()
	if err != nil {
		t.Fatalf("native: %v", err)
	}
	if want := "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"; xlm != want {
		t.Errorf("native SAC = %s, want %s", xlm, want)
	}
}

func TestSEP11SACAsset(t *testing.T) {
	InstallNetworkPassphrase("")
	const (
		usdcName = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcSAC  = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		xlmSAC   = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	)
	for _, tc := range []struct {
		name, contract, want string
	}{
		{"native", xlmSAC, "native"},
		{usdcName, usdcSAC, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		{usdcName, xlmSAC, ""},
		{"native", usdcSAC, ""},
		{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", usdcSAC, ""},
		{"not an asset", usdcSAC, ""},
	} {
		got, ok := SEP11SACAsset(tc.name, tc.contract)
		if tc.want == "" {
			if ok {
				t.Errorf("(%q, %s) trusted as %s; want untrusted", tc.name, tc.contract, got)
			}
			continue
		}
		if !ok || got.String() != tc.want {
			t.Errorf("(%q, %s) = (%s, %v), want %s", tc.name, tc.contract, got, ok, tc.want)
		}
	}
}

func TestSacContractID_SorobanHasNone(t *testing.T) {
	sor, err := NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sor.SacContractID(); err == nil {
		t.Error("soroban asset returned a SAC id; want error")
	}
}
