// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// What the refresh cron writes must read back through the API's decoder
// field for field, with an undeclared boolean still undeclared.
func TestPayloadRoundTripsCurrencyAuthority(t *testing.T) {
	yes, no := true, false
	sep := &metadata.SEP1{
		FetchedAt: time.Now().UTC(),
		Currencies: []metadata.Currency{
			{
				Code: "BOND", Issuer: "GAAA", Status: "live",
				IsAssetAnchored:        &yes,
				AttestationOfReserve:   "https://example.com/reserves.pdf",
				RedemptionInstructions: "Redeem through the portal.",
				Regulated:              &no,
				ApprovalServer:         "https://example.com/tx_approve",
				ApprovalCriteria:       "KYC required.",
			},
			{Code: "BARE", Issuer: "GAAA"},
		},
	}
	b, err := marshalSep1Payload(sep, true)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got timescale.IssuerSep1Cached
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Currencies) != 2 {
		t.Fatalf("currencies = %d, want 2", len(got.Currencies))
	}

	c := got.Currencies[0]
	if c.Status != "live" ||
		c.AttestationOfReserve != "https://example.com/reserves.pdf" ||
		c.RedemptionInstructions != "Redeem through the portal." ||
		c.ApprovalServer != "https://example.com/tx_approve" ||
		c.ApprovalCriteria != "KYC required." {
		t.Errorf("string declarations lost in the payload: %+v", c)
	}
	if c.IsAssetAnchored == nil || !*c.IsAssetAnchored {
		t.Errorf("IsAssetAnchored = %v, want true", c.IsAssetAnchored)
	}
	if c.Regulated == nil || *c.Regulated {
		t.Errorf("Regulated = %v, want a declared false", c.Regulated)
	}

	bare := got.Currencies[1]
	if bare.IsAssetAnchored != nil || bare.Regulated != nil {
		t.Errorf("undeclared booleans came back declared: is_asset_anchored=%v regulated=%v", bare.IsAssetAnchored, bare.Regulated)
	}
}
