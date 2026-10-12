// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"go/ast"
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBuildDEXTVLValueGate_ScreensNameOnlyTheWiredGuards: /v1/protocols' `basis` string publicly claims which trust
// screens ran on each TVL reserve leg, so it must name only wired guards; buildSubstanceGate and NewScamGate can each
// return nil, giving four combinations.
func TestBuildDEXTVLValueGate_ScreensNameOnlyTheWiredGuards(t *testing.T) {
	substance := pricingguard.NewSubstanceGate(nil, pricingguard.SubstanceGateOptions{})
	scam := pricingguard.NewScamGate(stubScamDirectory{}, pricingguard.ScamGateOptions{})
	if substance == nil || scam == nil {
		t.Fatalf("test setup: gates must be non-nil (substance=%v scam=%v)", substance, scam)
	}

	for _, tc := range []struct {
		name        string
		substance   *pricingguard.SubstanceGate
		scam        *pricingguard.ScamGate
		wantNil     bool
		wantScreens []string
	}{
		// The r1 default. Both screens run, so Basis may name both.
		{
			name: "both guards wired", substance: substance, scam: scam,
			wantScreens: []string{v1.TVLScreenScamDirectory, v1.TVLScreenSubstanceFloor},
		},
		// disable_substance_gate = true. The substance floor did NOT
		// run; claiming it did is the defect this test exists for.
		{
			name: "substance gate disabled", substance: nil, scam: scam,
			wantScreens: []string{v1.TVLScreenScamDirectory},
		},
		{
			name: "scam directory unavailable", substance: substance, scam: nil,
			wantScreens: []string{v1.TVLScreenSubstanceFloor},
		},
		// Neither guard: a nil INTERFACE, not an interface holding an
		// empty struct — v1's "no gate wired" degradation is reached by
		// comparing the interface against nil.
		{name: "no guard wired", substance: nil, scam: nil, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := buildDEXTVLValueGate(tc.substance, tc.scam, nil)
			if tc.wantNil {
				if gate != nil {
					t.Fatalf("gate = %#v, want a nil interface so DEXTVLCache.basisTail claims no screen", gate)
				}
				return
			}
			if gate == nil {
				t.Fatal("gate is nil, want a wired gate")
			}
			if got := gate.Screens(); !reflect.DeepEqual(got, tc.wantScreens) {
				t.Errorf("Screens() = %q, want %q", got, tc.wantScreens)
			}
		})
	}
}

// exprText renders the callee for the failure message.
func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x)
	}
	return "an expression"
}

// stubScamDirectory is a non-nil ScamDirectoryReader: NewScamGate
// returns nil for a nil reader, and these tests need the gate POINTER,
// never a directory verdict.
type stubScamDirectory struct{}

func (stubScamDirectory) DirectoryEntryByAddress(_ context.Context, _ string) (timescale.DirectoryEntry, bool, error) {
	return timescale.DirectoryEntry{}, false, nil
}

// flaggedScamDirectory flags every issuer it is asked about.
type flaggedScamDirectory struct{}

func (flaggedScamDirectory) DirectoryEntryByAddress(_ context.Context, _ string) (timescale.DirectoryEntry, bool, error) {
	return timescale.DirectoryEntry{Tags: []string{"scam"}}, true, nil
}

// TestDEXTVLValueGate_CountsAFlaggedTokenOnce: the TVL refresh asks once per reserve token whether its value may be
// published, not once per backing quote, or one flagged issuer reads as 2 + len(usd pegs) withheld serves per refresh.
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
