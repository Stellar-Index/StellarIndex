// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const (
	aliasUSDCIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aliasUSDCClassic = "USDC-" + aliasUSDCIssuer
	aliasUSDCSAC     = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

// TestAssetDetail_SACFormResolvesCanonical is Part A's detail-page half:
// GET /v1/assets/{USDC-SAC} must serve the CANONICAL classic USDC detail
// (same asset_id as /v1/assets/{USDC-classic}), resolved from the config
// AliasRegistry with NO lake round-trip (no explorer wired here). This is
// what makes a SAC-form link land on the asset's real page instead of a
// second, thin "SAC" identity.
//
// RED without the handleAssetGet canonicalization: with no explorer to run
// resolveSACToClassic, the SAC C-address echoes back as its own asset_id.
func TestAssetDetail_SACFormResolvesCanonical(t *testing.T) {
	// Process-global registry — must not run parallel; reset on cleanup.
	reg, err := canonical.NewAliasRegistry(map[string]string{aliasUSDCSAC: "USDC:" + aliasUSDCIssuer})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	srv := v1.New(v1.Options{})
	base := httpTestServer(t, srv).URL

	detail, code := getAssetDetail(t, base, aliasUSDCSAC)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if detail.AssetID != aliasUSDCClassic {
		t.Errorf("SAC-form detail asset_id = %q, want the canonical classic %q — a SAC link must resolve to the real asset page",
			detail.AssetID, aliasUSDCClassic)
	}
}
