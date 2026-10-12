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

// stubTVLGate withholds exactly the canonical assets it is told to, keyed on Asset.String() so tests can pin the identity asked.
// screens mirrors the production adapter unless set, because the Basis sentence is composed from it.
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

// TestTVLValuer_GateSeesTheCanonicalIdentity pins the SAC collapse: the scam directory is keyed by classic issuer G-address,
// so a configured classic/SAC wrapper must reach the gate as its classic twin, and native's SAC as `native`.
// Not parallel: the AliasRegistry is process-global.
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

// TestTVLValuer_GateBeatsTheDeclaredPeg pins ordering: the gate must run before the peg shortcut, or declared pegs go
// unscreened. Matches the asset detail path (suppressScamIssuerPricing after fillDeclaredPegPrice).
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

func TestTVLValuer_LegEdgeCases(t *testing.T) {
	v := newTVLValuer(stubTVLPricer{rates: map[string]string{"native": "2"}}, nil, nil, time.Now())
	ctx := context.Background()

	if usd, ok := v.legUSD(ctx, canonical.XLMSacContractID, big.NewInt(0)); !ok || usd.Sign() != 0 {
		t.Errorf("zero reserve should price as exactly 0 (ok=%v usd=%v)", ok, usd)
	}
	if _, ok := v.legUSD(ctx, canonical.XLMSacContractID, big.NewInt(-1)); ok {
		t.Error("negative reserve must be unpriceable")
	}
	if _, ok := v.legUSD(ctx, canonical.XLMSacContractID, nil); ok {
		t.Error("nil reserve must be unpriceable")
	}
	if _, ok := v.legUSD(ctx, "not-a-strkey", big.NewInt(1)); ok {
		t.Error("malformed token must be unpriceable")
	}
	// i128-scale reserve: 2^80 raw units at rate 2 — exact big math,
	// never a float or int64 (ADR-0003).
	huge := new(big.Int).Lsh(big.NewInt(1), 80)
	usd, ok := v.legUSD(ctx, canonical.XLMSacContractID, huge)
	if !ok {
		t.Fatal("huge reserve should price")
	}
	want := new(big.Rat).SetFrac(new(big.Int).Mul(huge, big.NewInt(2)), big.NewInt(10_000_000))
	if usd.Cmp(want) != 0 {
		t.Errorf("huge reserve usd = %s, want %s", usd.FloatString(4), want.FloatString(4))
	}
}
