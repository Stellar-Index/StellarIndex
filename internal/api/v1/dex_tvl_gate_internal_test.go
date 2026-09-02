// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubTVLGate withholds exactly the canonical assets it is told to.
// Keyed on Asset.String() so a test can pin WHICH identity the valuer
// asked about — the SAC-collapse half of the fix is invisible otherwise.
type stubTVLGate struct {
	withhold map[string]bool
	asked    []string
}

func (g *stubTVLGate) ValueWithheld(_ context.Context, asset canonical.Asset) bool {
	g.asked = append(g.asked, asset.String())
	return g.withhold[asset.String()]
}

// TestDEXTVLCache_GatedTokenContributesNoValue is the #338 regression.
//
// Before the gate, tvlValuer.rateFor consulted only the USD resolver,
// whose sole floor is $0.01 of quote notional — so a directory-flagged
// or substance-less token with one self-traded minute was valued into
// the pool at its own VWAP and summed into the protocol headline. The
// pool holding it counted as fully PRICED, so even the "≥" lower-bound
// hatching told the reader nothing was missing.
//
// The fixture is deliberately the SAME pool shape the pre-existing
// TestDEXTVLCache_RefreshComputesProtocolTVL asserts on ($10 XLM +
// $10.50 pegged USDC = $20.50, 1/1/0), with the USDC leg withheld. Both
// halves of the contract are pinned: the money (the gated leg's $10.50
// leaves tvl_usd) AND the honesty (the pool moves from priced to
// unpriced, so the surface renders a lower bound).
func TestDEXTVLCache_GatedTokenContributesNoValue(t *testing.T) {
	gate := &stubTVLGate{withhold: map[string]bool{
		// The pool's USDC-SAC leg: a token the serving guards refuse.
		"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": true,
	}}
	src := tvlTestSources()
	src.Gate = gate

	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()

	phx, ok := snap["phoenix"]
	if !ok {
		t.Fatal("phoenix missing from snapshot")
	}
	// $10 from the XLM leg only — the withheld USDC leg's $10.50 is
	// gone, and it must NOT have been re-derived through the declared
	// USD peg either (the peg shortcut is downstream of the gate).
	if phx.TVLUSD != "10.00" {
		t.Errorf("phoenix TVLUSD = %q, want 10.00 (was 20.50 with the gated leg valued)", phx.TVLUSD)
	}
	// The undecodable pool still counts 1 total + 1 unpriced; the gated
	// pool moves from priced → unpriced. Losing value silently while
	// still claiming pools_priced=1 would be the worse bug.
	if phx.PoolsTotal != 2 || phx.PoolsPriced != 0 || phx.UnpricedPools != 2 {
		t.Errorf("phoenix pools = total %d priced %d unpriced %d, want 2/0/2",
			phx.PoolsTotal, phx.PoolsPriced, phx.UnpricedPools)
	}
	if !strings.Contains(phx.Basis, "serving trust gates withhold") {
		t.Errorf("basis = %q, want it to state that gated legs count unpriced", phx.Basis)
	}

	// Comet holds the same two tokens: $5 XLM + $2.10 gated USDC.
	cm, ok := snap["comet"]
	if !ok {
		t.Fatal("comet missing from snapshot")
	}
	if cm.TVLUSD != "5.00" {
		t.Errorf("comet TVLUSD = %q, want 5.00 (was 7.10)", cm.TVLUSD)
	}
	if cm.PoolsTotal != 1 || cm.PoolsPriced != 0 || cm.UnpricedPools != 1 {
		t.Errorf("comet pools = total %d priced %d unpriced %d, want 1/0/1",
			cm.PoolsTotal, cm.PoolsPriced, cm.UnpricedPools)
	}

	// Soroswap: pair A loses its $10.50 USDC leg (priced → unpriced),
	// pair B was already unpriced. $10 + $5 = $15.
	ss := snap["soroswap"]
	if ss.TVLUSD != "15.00" {
		t.Errorf("soroswap TVLUSD = %q, want 15.00 (was 25.50)", ss.TVLUSD)
	}
	if ss.PoolsPriced != 0 || ss.UnpricedPools != 2 {
		t.Errorf("soroswap priced %d unpriced %d, want 0/2", ss.PoolsPriced, ss.UnpricedPools)
	}
}

// TestDEXTVLCache_NoGateKeepsTodaysFigures pins the nil direction: a
// deployment with [pricing_guard] disabled wires no gate and must value
// exactly what it valued before — and its Basis must NOT claim a screen
// that did not run.
func TestDEXTVLCache_NoGateKeepsTodaysFigures(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()
	if got := snap["phoenix"].TVLUSD; got != "20.50" {
		t.Errorf("ungated phoenix TVLUSD = %q, want the unchanged 20.50", got)
	}
	if strings.Contains(snap["phoenix"].Basis, "serving trust gates withhold") {
		t.Errorf("basis = %q, must not claim a gate that is not wired", snap["phoenix"].Basis)
	}
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
	reg, err := canonical.NewAliasRegistry(map[string]string{sac: scamCode + ":" + scamIssuer})
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
// issuer. Same ordering the asset detail path settled on 2026-08-25:
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

// TestDEXTVLCache_GatedAquariusLegCountsUnpriced covers the fourth
// protocol, whose leg loop is shaped differently (address-less legs are
// skipped before valuation) — the gate must apply on the arm that DOES
// resolve a token.
func TestDEXTVLCache_GatedAquariusLegCountsUnpriced(t *testing.T) {
	gate := &stubTVLGate{withhold: map[string]bool{tvlTestUSDCSAC: true}}
	src := tvlTestSources()
	src.Gate = gate
	// One pool: 1 XLM-SAC ($0.50 at rate 0.5) + 10.5 gated USDC-SAC
	// (which the declared peg would otherwise value at $10.50).
	src.AquariusReserves = &stubAquariusReserveReader{pools: []timescale.AquariusPoolReserve{{
		ContractID: tvlTestAqPool,
		ObservedAt: time.Now(),
		Legs: []timescale.AquariusReserveLeg{
			{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(10_000_000))},
			{TokenIndex: 1, Token: tvlTestUSDCSAC, Reserve: canonical.NewAmount(big.NewInt(105_000_000))},
		},
	}}}
	// Drop the other protocols so the assertion is about aquarius only.
	src.SoroswapReserves = stubTVLReserveReader{states: map[string]clickhouse.SoroswapPairState{}}
	src.PhoenixReserves = stubPhoenixReserveReader{states: map[string]clickhouse.PhoenixPoolState{}}
	src.CometReserves = stubCometReserveReader{states: map[string]clickhouse.CometPoolState{}}

	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()
	aq := snap["aquarius"]
	if aq.TVLUSD != "0.50" {
		t.Errorf("aquarius TVLUSD = %q, want 0.50 — the XLM leg only, with the gated"+
			" USDC leg's $10.50 excluded", aq.TVLUSD)
	}
	if aq.PoolsTotal != 1 || aq.PoolsPriced != 0 || aq.UnpricedPools != 1 {
		t.Errorf("aquarius pools = total %d priced %d unpriced %d, want 1/0/1",
			aq.PoolsTotal, aq.PoolsPriced, aq.UnpricedPools)
	}
}
