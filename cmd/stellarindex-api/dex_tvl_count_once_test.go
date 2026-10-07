// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// flaggedScamDirectory flags every issuer it is asked about.
type flaggedScamDirectory struct{}

func (flaggedScamDirectory) DirectoryEntryByAddress(_ context.Context, _ string) (timescale.DirectoryEntry, bool, error) {
	return timescale.DirectoryEntry{Tags: []string{"scam"}}, true, nil
}

// TestDEXTVLValueGate_CountsAFlaggedTokenOnce : the TVL refresh
// asks one question per reserve token — may its value be published —
// and must not ask it once per backing quote, or a flagged issuer reads as
// 2 + len(usd pegs) withheld serves per refresh for one issuer verdict.
func TestDEXTVLValueGate_CountsAFlaggedTokenOnce(t *testing.T) {
	token, err := canonical.NewClassicAsset("FLAG", "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	if err != nil {
		t.Fatal(err)
	}
	peg, err := canonical.NewClassicAsset("USDX", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	scam := pricingguard.NewScamGate(flaggedScamDirectory{}, pricingguard.ScamGateOptions{})
	gate := buildDEXTVLValueGate(nil, scam, []canonical.Asset{peg})
	counter := obs.PriceServeScamWithheldTotal.WithLabelValues(dexTVLGateSurface)

	before := testutil.ToFloat64(counter)
	if !gate.ValueWithheld(context.Background(), token) {
		t.Fatal("a flagged issuer's token was valued")
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("one flagged token counted %v times, want 1 (three quotes back it)", got)
	}
}
