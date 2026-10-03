// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func currencyAuthorityFixture(t *testing.T, cur timescale.IssuerSep1Currency) string {
	t.Helper()
	issuer := testUSDCIssuer
	domain := "issuer.example.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID: "USDC-" + testUSDCIssuer, Type: "classic",
				Code: "USDC", Issuer: &issuer, HomeDomain: &domain, Decimals: 7,
			},
		},
	}
	cur.Code, cur.Issuer = "USDC", testUSDCIssuer
	sep1 := &stubSep1Cache{byIssuer: map[string]*timescale.IssuerSep1Cached{
		testUSDCIssuer: {Currencies: []timescale.IssuerSep1Currency{cur}},
	}}
	return httpTestServer(t, v1.New(v1.Options{Assets: reader, Sep1Cache: sep1})).URL
}

// The issuer's standing and backing declarations reach both the asset
// detail and the metadata-only surface, with a declared false served as
// false rather than dropped.
func TestAssetGet_Sep1OverlayCurrencyAuthority(t *testing.T) {
	yes, no := true, false
	base := currencyAuthorityFixture(t, timescale.IssuerSep1Currency{
		Status:                 "live",
		IsAssetAnchored:        &yes,
		AttestationOfReserve:   "https://issuer.example.com/reserves.pdf",
		RedemptionInstructions: "Redeem through the issuer's portal.",
		Regulated:              &no,
		ApprovalServer:         "https://issuer.example.com/tx_approve",
		ApprovalCriteria:       "KYC required.",
	})

	for _, path := range []string{"/v1/assets/USDC-" + testUSDCIssuer, "/v1/assets/USDC-" + testUSDCIssuer + "/metadata"} {
		resp := mustGet(t, base+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", path, resp.StatusCode)
		}
		var env struct {
			Data v1.Sep1CurrencyAuthority `json:"data"`
		}
		mustDecode(t, resp, &env)
		d := env.Data
		if d.CurrencyStatus == nil || *d.CurrencyStatus != "live" {
			t.Errorf("%s: currency_status = %v, want live", path, d.CurrencyStatus)
		}
		if d.IsAssetAnchored == nil || !*d.IsAssetAnchored {
			t.Errorf("%s: is_asset_anchored = %v, want true", path, d.IsAssetAnchored)
		}
		if d.Regulated == nil || *d.Regulated {
			t.Errorf("%s: regulated = %v, want a declared false", path, d.Regulated)
		}
		if d.AttestationOfReserve == nil || *d.AttestationOfReserve != "https://issuer.example.com/reserves.pdf" {
			t.Errorf("%s: attestation_of_reserve = %v", path, d.AttestationOfReserve)
		}
		if d.RedemptionInstructions == nil || *d.RedemptionInstructions != "Redeem through the issuer's portal." {
			t.Errorf("%s: redemption_instructions = %v", path, d.RedemptionInstructions)
		}
		if d.ApprovalServer == nil || *d.ApprovalServer != "https://issuer.example.com/tx_approve" {
			t.Errorf("%s: approval_server = %v", path, d.ApprovalServer)
		}
		if d.ApprovalCriteria == nil || *d.ApprovalCriteria != "KYC required." {
			t.Errorf("%s: approval_criteria = %v", path, d.ApprovalCriteria)
		}
	}
}

// Clients render the two URL fields as links, so a non-http(s) value from
// the issuer's document is dropped while the rest of the entry still serves.
func TestAssetGet_Sep1OverlayDropsUnsafeAuthorityURLs(t *testing.T) {
	base := currencyAuthorityFixture(t, timescale.IssuerSep1Currency{
		Status:               "live",
		AttestationOfReserve: "javascript:alert(1)",
		ApprovalServer:       "https://x.example.com/\"><script>",
	})
	resp := mustGet(t, base+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.AttestationOfReserve != nil {
		t.Errorf("attestation_of_reserve = %q, want dropped", *env.Data.AttestationOfReserve)
	}
	if env.Data.ApprovalServer != nil {
		t.Errorf("approval_server = %q, want dropped", *env.Data.ApprovalServer)
	}
	if env.Data.CurrencyStatus == nil {
		t.Error("currency_status dropped alongside the unsafe URLs; the guard should be URL-only")
	}
	if env.Data.IsAssetAnchored != nil || env.Data.Regulated != nil {
		t.Errorf("undeclared booleans served as declared: is_asset_anchored=%v regulated=%v",
			env.Data.IsAssetAnchored, env.Data.Regulated)
	}
}
