// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
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
