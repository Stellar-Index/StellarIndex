// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"strings"
	"testing"
)

// The standing and backing keys of a [[CURRENCIES]] entry must survive the
// parse, and an omitted boolean must stay distinguishable from a declared
// false: "not anchored" and "did not say" are different claims.
func TestParseCurrency_StandingAndBackingDeclarations(t *testing.T) {
	body := []byte(`
[[CURRENCIES]]
code="BOND"
issuer="GAAA"
status="live"
is_asset_anchored=true
anchor_asset_type="bond"
attestation_of_reserve="https://example.com/reserves.pdf"
redemption_instructions="Redeem through the issuer's portal."
regulated=true
approval_server="https://example.com/tx_approve"
approval_criteria="KYC required."

[[CURRENCIES]]
code="MEME"
issuer="GAAA"
is_asset_anchored=false
regulated=false

[[CURRENCIES]]
code="BARE"
issuer="GAAA"
`)
	sep, err := parseSEP1(body)
	if err != nil {
		t.Fatalf("parseSEP1: %v", err)
	}
	if len(sep.Currencies) != 3 {
		t.Fatalf("currencies = %d, want 3", len(sep.Currencies))
	}

	full := sep.Currencies[0]
	if full.Status != "live" {
		t.Errorf("Status = %q, want live", full.Status)
	}
	if full.IsAssetAnchored == nil || !*full.IsAssetAnchored {
		t.Errorf("IsAssetAnchored = %v, want true", full.IsAssetAnchored)
	}
	if full.AttestationOfReserve != "https://example.com/reserves.pdf" {
		t.Errorf("AttestationOfReserve = %q", full.AttestationOfReserve)
	}
	if full.RedemptionInstructions != "Redeem through the issuer's portal." {
		t.Errorf("RedemptionInstructions = %q", full.RedemptionInstructions)
	}
	if full.Regulated == nil || !*full.Regulated {
		t.Errorf("Regulated = %v, want true", full.Regulated)
	}
	if full.ApprovalServer != "https://example.com/tx_approve" {
		t.Errorf("ApprovalServer = %q", full.ApprovalServer)
	}
	if full.ApprovalCriteria != "KYC required." {
		t.Errorf("ApprovalCriteria = %q", full.ApprovalCriteria)
	}

	declaredFalse := sep.Currencies[1]
	if declaredFalse.IsAssetAnchored == nil || *declaredFalse.IsAssetAnchored {
		t.Errorf("declared is_asset_anchored=false read as %v, want a non-nil false", declaredFalse.IsAssetAnchored)
	}
	if declaredFalse.Regulated == nil || *declaredFalse.Regulated {
		t.Errorf("declared regulated=false read as %v, want a non-nil false", declaredFalse.Regulated)
	}

	bare := sep.Currencies[2]
	if bare.IsAssetAnchored != nil || bare.Regulated != nil {
		t.Errorf("omitted booleans read as declared: is_asset_anchored=%v regulated=%v", bare.IsAssetAnchored, bare.Regulated)
	}
}

// Free-text keys an issuer controls are capped like every other field.
func TestParseCurrency_CapsRedemptionInstructions(t *testing.T) {
	long := strings.Repeat("x", maxLongFieldRunes+100)
	sep, err := parseSEP1([]byte("[[CURRENCIES]]\ncode=\"A\"\nredemption_instructions=\"" + long + "\"\n"))
	if err != nil {
		t.Fatalf("parseSEP1: %v", err)
	}
	if got := len(sep.Currencies[0].RedemptionInstructions); got != maxLongFieldRunes {
		t.Errorf("redemption_instructions length = %d, want capped at %d", got, maxLongFieldRunes)
	}
}
