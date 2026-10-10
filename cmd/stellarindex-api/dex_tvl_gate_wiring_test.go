// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"go/ast"
	"reflect"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBuildDEXTVLValueGate_ScreensNameOnlyTheWiredGuards pins the
// composition half of RV1 #3: /v1/protocols' `basis` string is a public
// claim about which trust screens ran on each TVL reserve leg, and the
// gate is the only thing that knows which of its sub-guards the
// operator left wired.
//
// buildSubstanceGate returns nil for [pricing_guard]
// disable_substance_gate = true, and NewScamGate returns nil for a nil
// directory reader — so all four combinations below are configurations
// this binary can actually be started in.
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
