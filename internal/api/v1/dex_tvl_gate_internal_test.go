// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// stubTVLGate withholds exactly the canonical assets it is told to.
// Keyed on Asset.String() so a test can pin WHICH identity the valuer
// asked about — the SAC-collapse half of the fix is invisible otherwise.
//
// screens mirrors what the production adapter reports (both guards
// wired, in that order) unless a test sets it: the Basis sentence is
// composed from it, so a gate that claims fewer screens must describe
// fewer screens.
type stubTVLGate struct {
	withhold map[string]bool
	asked    []string
	screens  []string
}

func (g *stubTVLGate) ValueWithheld(_ context.Context, asset canonical.Asset) bool {
	g.asked = append(g.asked, asset.String())
	return g.withhold[asset.String()]
}

func (g *stubTVLGate) Screens() []string {
	if g.screens == nil {
		return []string{TVLScreenScamDirectory, TVLScreenSubstanceFloor}
	}
	return g.screens
}

// TestTVLValuer_GateSeesTheCanonicalIdentity pins the SAC-collapse half.
// A pool leg is a C-strkey by construction, but the scam directory is
// keyed by the issuer G-address only a CLASSIC asset carries — so a
// configured classic↔SAC wrapper must reach the gate as its classic
// twin, or the gate is asked about an identity it can never speak
// about. Native's SAC must arrive as `native` for the same reason.
//
// NOT parallel: the AliasRegistry is process-global (same convention as
// installFoldRegistry in assets_fold_alias_test.go).
func TestTVLValuer_GateSeesTheCanonicalIdentity(t *testing.T) {
	const (
		scamCode   = "SCAM"
		scamIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	)
	classic, err := canonical.NewClassicAsset(scamCode, scamIssuer)
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	sac, err := classic.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{sac: scamCode + ":" + scamIssuer})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	gate := &stubTVLGate{withhold: map[string]bool{classic.String(): true}}
	v := newTVLValuer(
		stubTVLPricer{rates: map[string]string{sac: "3", "native": "2"}},
		nil, gate, time.Now())

	if _, ok := v.legUSD(context.Background(), sac, big.NewInt(10_000_000)); ok {
		t.Error("a SAC-wrapped flagged classic must be withheld — the scam directory " +
			"is keyed by issuer, so the gate has to see the classic identity")
	}
	if _, ok := v.legUSD(context.Background(), canonical.XLMSacContractID, big.NewInt(10_000_000)); !ok {
		t.Error("native XLM's SAC must still price")
	}
	want := []string{classic.String(), "native"}
	if len(gate.asked) != len(want) {
		t.Fatalf("gate was asked about %v, want the canonical identities %v", gate.asked, want)
	}
	for i, w := range want {
		if gate.asked[i] != w {
			t.Errorf("gate.asked[%d] = %q, want %q", i, gate.asked[i], w)
		}
	}
}

// TestTVLValuer_GateBeatsTheDeclaredPeg pins the ORDERING. The peg
// shortcut sits between the token→asset resolution and the resolver, so
// a gate consulted after it would leave every operator-declared peg
// unscreened — and an operator's 1:1-USD declaration is an older,
// broader statement than a curated directory's later scam flag on that
// issuer. Same ordering as the asset detail path:
// suppressScamIssuerPricing runs after fillDeclaredPegPrice.
func TestTVLValuer_GateBeatsTheDeclaredPeg(t *testing.T) {
	const pegged = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	gate := &stubTVLGate{withhold: map[string]bool{}}
	v := newTVLValuer(nil, stubTVLPegInfo{pegged: map[string]int{pegged: 7}}, gate, time.Now())

	usd, ok := v.legUSD(context.Background(), pegged, big.NewInt(10_000_000))
	if !ok || usd.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("ungated declared peg = %v ok=%v, want exactly $1", usd, ok)
	}

	gate2 := &stubTVLGate{withhold: map[string]bool{pegged: true}}
	v2 := newTVLValuer(nil, stubTVLPegInfo{pegged: map[string]int{pegged: 7}}, gate2, time.Now())
	if _, ok := v2.legUSD(context.Background(), pegged, big.NewInt(10_000_000)); ok {
		t.Error("a withheld asset must not be re-valued through the declared USD peg")
	}
}

// TestTVLValuer_GateAskedOncePerToken pins the memo: the refresh walks
// hundreds of pool legs and the gates each do a cached DB lookup, so a
// per-LEG call would multiply the directory/substance read fan-out.
func TestTVLValuer_GateAskedOncePerToken(t *testing.T) {
	gate := &stubTVLGate{withhold: map[string]bool{}}
	v := newTVLValuer(stubTVLPricer{rates: map[string]string{"native": "1"}}, nil, gate, time.Now())
	for range 5 {
		if _, ok := v.legUSD(context.Background(), canonical.XLMSacContractID, big.NewInt(1)); !ok {
			t.Fatal("leg should price")
		}
	}
	if len(gate.asked) != 1 {
		t.Errorf("gate consulted %d times for one token, want 1 (%v)", len(gate.asked), gate.asked)
	}
}
