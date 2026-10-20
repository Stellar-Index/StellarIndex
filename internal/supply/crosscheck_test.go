package supply_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

const (
	usdcClassicKey = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdcSACKey     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUOZWS4HG3B5UPHHC2QQA"
)

// supplyWithTotal is a small helper to build a Supply with just the
// fields CrossCheck reads — keeps tests focused on the comparison.
func supplyWithTotal(key string, total int64) supply.Supply {
	return supply.Supply{
		AssetKey:    key,
		TotalSupply: big.NewInt(total),
	}
}

// TestCrossCheck_ExactMatch — equal totals report DivergenceStroops=0
// and WithinTolerance=true.
func TestCrossCheck_ExactMatch(t *testing.T) {
	classic := supplyWithTotal(usdcClassicKey, 1_000_000_000)
	sac := supplyWithTotal(usdcSACKey, 1_000_000_000)

	got, err := supply.CrossCheck(classic, sac)
	if err != nil {
		t.Fatalf("CrossCheck: %v", err)
	}
	if got.DivergenceStroops.Sign() != 0 {
		t.Errorf("DivergenceStroops = %s, want 0", got.DivergenceStroops)
	}
	if !got.WithinTolerance {
		t.Errorf("WithinTolerance = false on exact match, want true")
	}
}

// TestCrossCheck_OneStroopDriftIsTolerated — exactly 1 stroop is
// the documented tolerance; alert MUST NOT fire.
func TestCrossCheck_OneStroopDriftIsTolerated(t *testing.T) {
	classic := supplyWithTotal(usdcClassicKey, 1_000_000_001)
	sac := supplyWithTotal(usdcSACKey, 1_000_000_000)

	got, _ := supply.CrossCheck(classic, sac)
	if got.DivergenceStroops.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("DivergenceStroops = %s, want 1", got.DivergenceStroops)
	}
	if !got.WithinTolerance {
		t.Errorf("1-stroop drift must be tolerated; got WithinTolerance=false")
	}
}

// TestCrossCheck_TwoStroopDriftFires — > tolerance triggers an
// out-of-tolerance result; this is what feeds the alert.
func TestCrossCheck_TwoStroopDriftFires(t *testing.T) {
	classic := supplyWithTotal(usdcClassicKey, 1_000_000_002)
	sac := supplyWithTotal(usdcSACKey, 1_000_000_000)

	got, _ := supply.CrossCheck(classic, sac)
	if got.DivergenceStroops.Cmp(big.NewInt(2)) != 0 {
		t.Errorf("DivergenceStroops = %s, want 2", got.DivergenceStroops)
	}
	if got.WithinTolerance {
		t.Errorf("2-stroop drift must trigger alert; got WithinTolerance=true")
	}
}

// TestCrossCheck_AsymmetricSign — divergence is absolute, regardless
// of which side is larger.
func TestCrossCheck_AsymmetricSign(t *testing.T) {
	// SAC reports a higher total than classic — could happen if
	// the SAC has been minted-into via cross-contract calls the
	// classic indexer hasn't observed yet.
	classic := supplyWithTotal(usdcClassicKey, 1_000_000_000)
	sac := supplyWithTotal(usdcSACKey, 1_000_000_005)

	got, _ := supply.CrossCheck(classic, sac)
	if got.DivergenceStroops.Cmp(big.NewInt(5)) != 0 {
		t.Errorf("DivergenceStroops = %s, want 5", got.DivergenceStroops)
	}
	if got.DivergenceStroops.Sign() < 0 {
		t.Errorf("DivergenceStroops = %s, must be non-negative", got.DivergenceStroops)
	}
	if got.WithinTolerance {
		t.Error("5-stroop drift must trigger alert")
	}
}

// ClassicTotal / SACTotal on the result are independent copies of the inputs, so logs and dashboards can quote them and
// later mutation does not corrupt the originals.
func TestCrossCheck_PreservesInputs(t *testing.T) {
	classic := supplyWithTotal(usdcClassicKey, 100)
	sac := supplyWithTotal(usdcSACKey, 100)

	got, _ := supply.CrossCheck(classic, sac)
	if got.ClassicTotal.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("ClassicTotal = %s, want 100", got.ClassicTotal)
	}
	if got.SACTotal.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("SACTotal = %s, want 100", got.SACTotal)
	}
	// Mutate the result-side copies; original Supply must remain.
	got.ClassicTotal.SetInt64(0)
	got.SACTotal.SetInt64(0)
	if classic.TotalSupply.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("classic.TotalSupply mutated by result mutation")
	}
	if sac.TotalSupply.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("sac.TotalSupply mutated by result mutation")
	}
}

// TestCrossCheck_RejectsNilTotalSupply — defensive: a caller passing
// a zero-value Supply (no TotalSupply) must trip a typed error rather
// than nil-pointer the Sub call.
func TestCrossCheck_RejectsNilTotalSupply(t *testing.T) {
	classic := supply.Supply{AssetKey: usdcClassicKey} // TotalSupply nil
	sac := supplyWithTotal(usdcSACKey, 100)
	_, err := supply.CrossCheck(classic, sac)
	if !errors.Is(err, supply.ErrCrossCheckNilSupply) {
		t.Errorf("err = %v, want ErrCrossCheckNilSupply", err)
	}
	// Also when the SAC side is nil.
	_, err = supply.CrossCheck(supplyWithTotal(usdcClassicKey, 100), supply.Supply{AssetKey: usdcSACKey})
	if !errors.Is(err, supply.ErrCrossCheckNilSupply) {
		t.Errorf("err = %v, want ErrCrossCheckNilSupply (sac-side)", err)
	}
}

// TestCrossCheck_KeysCarriedThroughResult — the result's
// ClassicKey + SACKey echo the inputs so log lines + alert labels
// can identify the asset without separate plumbing.
func TestCrossCheck_KeysCarriedThroughResult(t *testing.T) {
	got, _ := supply.CrossCheck(
		supplyWithTotal(usdcClassicKey, 100),
		supplyWithTotal(usdcSACKey, 100),
	)
	if got.ClassicKey != usdcClassicKey {
		t.Errorf("ClassicKey = %q", got.ClassicKey)
	}
	if got.SACKey != usdcSACKey {
		t.Errorf("SACKey = %q", got.SACKey)
	}
}

// TestCrossCheckTolerance_Value — the documented tolerance is
// exactly 1 stroop per ADR-0011. Pinning the constant guards against
// an accidental nudge.
func TestCrossCheckTolerance_Value(t *testing.T) {
	if supply.CrossCheckTolerance.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("CrossCheckTolerance = %s, want 1 (per ADR-0011)",
			supply.CrossCheckTolerance)
	}
}

// TestCrossCheck_ResultIsWrapClassFull — [CrossCheck] always tags its
// result WrapClassFull, since it runs the strict equality invariant.
func TestCrossCheck_ResultIsWrapClassFull(t *testing.T) {
	got, err := supply.CrossCheck(
		supplyWithTotal(usdcClassicKey, 100),
		supplyWithTotal(usdcSACKey, 100),
	)
	if err != nil {
		t.Fatalf("CrossCheck: %v", err)
	}
	if got.WrapClass != supply.WrapClassFull {
		t.Errorf("WrapClass = %q, want %q", got.WrapClass, supply.WrapClassFull)
	}
}

// ─── CrossCheckSubsetBound ───────────────────────────────────────────
//
// Equality of Algorithm 2's classic total and Algorithm 3's SAC-wrapped total only holds for a
// fully SAC-represented asset, and would false-positive
// `stellarindex_supply_cross_check_divergence` otherwise. The real invariant is a subset bound:
// sac_total never exceeds classic_total (SACWrapped is a non-negative addend of Algorithm 2), so
// only sac_total > classic_total is a violation; classic_total > sac_total is the normal state
// for a partially wrapped asset.

// The AQUA case from the crosscheck.go package doc (classic about 86.4B, SAC about 0): an equality compare would false-
// positive, so the subset bound must report zero divergence.
func TestCrossCheckSubsetBound_ClassicExceedsSacIsBenign(t *testing.T) {
	classic := supplyWithTotal(usdcClassicKey, 86_400_000_000_0000000)
	sac := supplyWithTotal(usdcSACKey, 0)

	got, err := supply.CrossCheckSubsetBound(classic, sac)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.DivergenceStroops.Sign() != 0 {
		t.Errorf("DivergenceStroops = %s, want 0 (classic > sac is benign for a partially-wrapped asset)", got.DivergenceStroops)
	}
	if !got.WithinTolerance {
		t.Errorf("WithinTolerance = false, want true — classic > sac must not alert")
	}
	if got.WrapClass != supply.WrapClassPartial {
		t.Errorf("WrapClass = %q, want %q", got.WrapClass, supply.WrapClassPartial)
	}
}

// A fully-wrapped asset whose totals match exactly is still a benign zero divergence under the subset bound (a special
// case of <=).
func TestCrossCheckSubsetBound_ExactMatchIsWithin(t *testing.T) {
	got, err := supply.CrossCheckSubsetBound(
		supplyWithTotal(usdcClassicKey, 1_000_000_000),
		supplyWithTotal(usdcSACKey, 1_000_000_000),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.DivergenceStroops.Sign() != 0 || !got.WithinTolerance {
		t.Errorf("exact match: got divergence=%s withinTolerance=%v, want 0/true",
			got.DivergenceStroops, got.WithinTolerance)
	}
}

// Leg 1's premise (cumulative SAC net mint <= classic outstanding) fails for BLND (retired classically after SAC
// minting) and PHO (all minted via SAC, classic issuer-excluded). The excess is reported as OverMintStroops but must not
// feed DivergenceStroops or alert.
func TestCrossCheckSubsetBound_OverMintIsDiagnosticOnly(t *testing.T) {
	got, err := supply.CrossCheckSubsetBound(
		supplyWithTotal(usdcClassicKey, 1_000_000_000),
		supplyWithTotal(usdcSACKey, 1_000_000_005),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.OverMintStroops.Cmp(big.NewInt(5)) != 0 {
		t.Errorf("OverMintStroops = %s, want 5 — the gap must stay visible", got.OverMintStroops)
	}
	if got.DivergenceStroops.Sign() != 0 {
		t.Errorf("DivergenceStroops = %s, want 0 — over-mint is diagnostic, not paging", got.DivergenceStroops)
	}
	if !got.WithinTolerance {
		t.Error("a pure over-mint gap must not alert (BLND/PHO false-positive class)")
	}
}

// TestCrossCheckSubsetBound_RejectsNilTotalSupply — same defensive
// contract as CrossCheck.
func TestCrossCheckSubsetBound_RejectsNilTotalSupply(t *testing.T) {
	_, err := supply.CrossCheckSubsetBound(
		supply.Supply{AssetKey: usdcClassicKey},
		supplyWithTotal(usdcSACKey, 100),
	)
	if !errors.Is(err, supply.ErrCrossCheckNilSupply) {
		t.Errorf("err = %v, want ErrCrossCheckNilSupply", err)
	}
}

// TestCrossCheckSubsetBound_DivergenceNeverNegative — even a large
// classic-exceeds-sac gap must clamp to zero, never go negative
// (a negative gauge reading would be nonsensical).
func TestCrossCheckSubsetBound_DivergenceNeverNegative(t *testing.T) {
	got, err := supply.CrossCheckSubsetBound(
		supplyWithTotal(usdcClassicKey, 1_000_000_000_000),
		supplyWithTotal(usdcSACKey, 1),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.DivergenceStroops.Sign() < 0 {
		t.Errorf("DivergenceStroops = %s, must be non-negative", got.DivergenceStroops)
	}
	if got.DivergenceStroops.Sign() != 0 {
		t.Errorf("DivergenceStroops = %s, want 0", got.DivergenceStroops)
	}
}

// ─── CrossCheckForClass dispatch ───────────────────────────────────

// Table over every WrapClass (including the zero value and an unrecognized string) on a pair with classic > sac by 2
// stroops: a violation under WrapClassFull (equality), benign under WrapClassPartial (subset bound); the class decides
// whether identical inputs alert.
func TestCrossCheckForClass_Dispatch(t *testing.T) {
	classic := supplyWithTotal(usdcClassicKey, 1_000_000_002)
	sac := supplyWithTotal(usdcSACKey, 1_000_000_000)

	cases := []struct {
		name           string
		class          supply.WrapClass
		wantWithin     bool
		wantWrapClass  supply.WrapClass
		wantDivergence int64
	}{
		{"full: 2-stroop mismatch fires", supply.WrapClassFull, false, supply.WrapClassFull, 2},
		{"partial: classic exceeds sac is benign", supply.WrapClassPartial, true, supply.WrapClassPartial, 0},
		{"zero value normalizes to partial", "", true, supply.WrapClassPartial, 0},
		{"unrecognized string normalizes to partial", supply.WrapClass("bogus"), true, supply.WrapClassPartial, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := supply.CrossCheckForClass(classic, sac, tc.class)
			if err != nil {
				t.Fatalf("CrossCheckForClass: %v", err)
			}
			if got.WithinTolerance != tc.wantWithin {
				t.Errorf("WithinTolerance = %v, want %v", got.WithinTolerance, tc.wantWithin)
			}
			if got.WrapClass != tc.wantWrapClass {
				t.Errorf("WrapClass = %q, want %q", got.WrapClass, tc.wantWrapClass)
			}
			if got.DivergenceStroops.Cmp(big.NewInt(tc.wantDivergence)) != 0 {
				t.Errorf("DivergenceStroops = %s, want %d", got.DivergenceStroops, tc.wantDivergence)
			}
		})
	}
}

// Leg 2, the escrow bound: classic.SACWrappedStroops <= sac.TotalSupply. SACWrapped (ledger-entry sum) and the SAC total
// (event-flow mint-burn-clawback) measure one quantity by independent paths, so escrow exceeding net mint is impossible;
// without the persisted component it hides in the benign classic > sac gap.

// supplyWithSACWrapped builds a classic-side Supply carrying Algorithm
// 2's SACWrapped component alongside the folded total — the shape
// ClassicComputer.Compute returns and LatestSupply reads back.
func supplyWithSACWrapped(key string, total, sacWrapped int64) supply.Supply {
	s := supplyWithTotal(key, total)
	s.SACWrappedStroops = big.NewInt(sacWrapped)
	return s
}

// Core check for leg 2: classic reports 1_000_000 stroops inside the SAC but the SAC's event-derived total is 999_000,
// so 1000 were escrowed unminted. Leg 1 sees the normal partial-wrap shape (sac 999_000 < classic 5_000_000), so a
// leg-1-only check would report green.
func TestCrossCheckSubsetBound_EscrowExceedsMintedFires(t *testing.T) {
	classic := supplyWithSACWrapped(usdcClassicKey, 5_000_000, 1_000_000)
	sac := supplyWithTotal(usdcSACKey, 999_000)

	got, err := supply.CrossCheckSubsetBound(classic, sac)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if !got.SubsetBoundChecked {
		t.Fatal("SubsetBoundChecked = false; want true — the classic snapshot carried a SACWrapped component, so the escrow leg must be evaluated")
	}
	if got.EscrowExcessStroops == nil || got.EscrowExcessStroops.Cmp(big.NewInt(1000)) != 0 {
		t.Errorf("EscrowExcessStroops = %v, want 1000 (1_000_000 escrowed − 999_000 net-minted)", got.EscrowExcessStroops)
	}
	if got.OverMintStroops == nil || got.OverMintStroops.Sign() != 0 {
		t.Errorf("OverMintStroops = %v, want 0 — leg 1 is satisfied here; only leg 2 is breached", got.OverMintStroops)
	}
	if got.DivergenceStroops.Cmp(big.NewInt(1000)) != 0 {
		t.Errorf("DivergenceStroops = %s, want 1000 — the gauge must carry the worse of the two legs", got.DivergenceStroops)
	}
	if got.WithinTolerance {
		t.Error("WithinTolerance = true on a 1000-stroop escrow-exceeds-minted breach; this is the one-sided blind spot E4/N-F3(b) closes")
	}
}

// Leg 2 reuses CrossCheckTolerance unchanged, so its boundary matches leg 1: 1 stroop of honest NUMERIC/i128 rounding
// does not page.
func TestCrossCheckSubsetBound_EscrowLegOneStroopTolerated(t *testing.T) {
	got, err := supply.CrossCheckSubsetBound(
		supplyWithSACWrapped(usdcClassicKey, 5_000_000, 1_000_001),
		supplyWithTotal(usdcSACKey, 1_000_000),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.EscrowExcessStroops == nil || got.EscrowExcessStroops.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("EscrowExcessStroops = %v, want 1", got.EscrowExcessStroops)
	}
	if !got.WithinTolerance {
		t.Error("1-stroop escrow overshoot must be tolerated, matching leg 1's boundary")
	}
}

// The BLND/EURC/KALE/PHO shape: SACWrapped is under-counted (dormant pool balances not yet seeded) and sits below
// sac_total. That is benign, cured by `supply seed-sac-balances -full-history`; leg 2 must stay silent to avoid that
// false-positive class.
func TestCrossCheckSubsetBound_EscrowBelowMintedIsBenign(t *testing.T) {
	got, err := supply.CrossCheckSubsetBound(
		supplyWithSACWrapped(usdcClassicKey, 5_000_000, 200_000),
		supplyWithTotal(usdcSACKey, 1_236_670),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if !got.SubsetBoundChecked {
		t.Error("SubsetBoundChecked = false, want true")
	}
	if got.EscrowExcessStroops == nil || got.EscrowExcessStroops.Sign() != 0 {
		t.Errorf("EscrowExcessStroops = %v, want 0 — an under-counted SACWrapped is benign here", got.EscrowExcessStroops)
	}
	if got.DivergenceStroops.Sign() != 0 || !got.WithinTolerance {
		t.Errorf("got divergence=%s within=%v, want 0/true", got.DivergenceStroops, got.WithinTolerance)
	}
}

// A classic snapshot without a SACWrapped component reports the escrow leg as unevaluated, never defaulted to zero,
// which would vacuously satisfy 0 <= sac_total and publish a "checked" green verdict that verified nothing.
func TestCrossCheckSubsetBound_UncheckedWhenNoComponent(t *testing.T) {
	got, err := supply.CrossCheckSubsetBound(
		supplyWithTotal(usdcClassicKey, 5_000_000), // nil SACWrappedStroops = pre-0117 row
		supplyWithTotal(usdcSACKey, 999_000),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.SubsetBoundChecked {
		t.Error("SubsetBoundChecked = true with no SACWrapped component on the classic snapshot; a green result here would be read as two-sided assurance it cannot support")
	}
	if got.EscrowExcessStroops != nil {
		t.Errorf("EscrowExcessStroops = %v, want nil when the leg was not evaluated", got.EscrowExcessStroops)
	}
	if got.SACWrapped != nil {
		t.Errorf("SACWrapped = %v, want nil", got.SACWrapped)
	}
	// Leg 1 still runs and still reports its own verdict unchanged.
	if got.OverMintStroops == nil || got.OverMintStroops.Sign() != 0 {
		t.Errorf("OverMintStroops = %v, want 0", got.OverMintStroops)
	}
	if !got.WithinTolerance {
		t.Error("WithinTolerance = false; leg 1 is satisfied and leg 2 was not evaluated, so nothing may page")
	}
}

// When both bounds are breached the single divergence gauge must report the LARGER excess, so the existing 1-stroop
// alert threshold fires on either leg without a per-leg rule.
func TestCrossCheckSubsetBound_GaugeCarriesWorseLeg(t *testing.T) {
	// sac_total 1_000_050 exceeds classic total 1_000_000 by 50 (leg 1),
	// and SACWrapped 1_000_500 exceeds sac_total by 450 (leg 2).
	got, err := supply.CrossCheckSubsetBound(
		supplyWithSACWrapped(usdcClassicKey, 1_000_000, 1_000_500),
		supplyWithTotal(usdcSACKey, 1_000_050),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.OverMintStroops == nil || got.OverMintStroops.Cmp(big.NewInt(50)) != 0 {
		t.Errorf("OverMintStroops = %v, want 50", got.OverMintStroops)
	}
	if got.EscrowExcessStroops == nil || got.EscrowExcessStroops.Cmp(big.NewInt(450)) != 0 {
		t.Errorf("EscrowExcessStroops = %v, want 450", got.EscrowExcessStroops)
	}
	if got.DivergenceStroops.Cmp(big.NewInt(450)) != 0 {
		t.Errorf("DivergenceStroops = %s, want 450 (max of the two legs)", got.DivergenceStroops)
	}
	if got.WithinTolerance {
		t.Error("WithinTolerance = true with both bounds breached")
	}
}

// The result's *big.Int fields must be defensive copies: the refresher hands them to a metric emitter and a log line,
// and aliasing the caller's snapshot would let downstream mutation rewrite the recorded comparison.
func TestCrossCheckSubsetBound_DoesNotAliasInput(t *testing.T) {
	classic := supplyWithSACWrapped(usdcClassicKey, 5_000_000, 1_000_000)
	got, err := supply.CrossCheckSubsetBound(classic, supplyWithTotal(usdcSACKey, 2_000_000))
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	classic.SACWrappedStroops.SetInt64(42)
	if got.SACWrapped == nil || got.SACWrapped.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Errorf("SACWrapped = %v after mutating the input, want 1000000 — result must not alias the input", got.SACWrapped)
	}
}

// TestCrossCheckForClass_FullWrapLeavesEscrowLegUnchecked: under WrapClassFull the equality
// compare already subsumes the escrow bound, so leg 2 is deliberately not evaluated. Pinned so
// SubsetBoundChecked is never true on a path that did not run the check.
func TestCrossCheckForClass_FullWrapLeavesEscrowLegUnchecked(t *testing.T) {
	got, err := supply.CrossCheckForClass(
		supplyWithSACWrapped(usdcClassicKey, 1_000_000, 1_000_000),
		supplyWithTotal(usdcSACKey, 1_000_000),
		supply.WrapClassFull,
	)
	if err != nil {
		t.Fatalf("CrossCheckForClass: %v", err)
	}
	if got.SubsetBoundChecked {
		t.Error("SubsetBoundChecked = true under WrapClassFull; the equality compare subsumes the escrow bound and does not evaluate it separately")
	}
	if !got.WithinTolerance {
		t.Errorf("WithinTolerance = false on an exact full-wrap match (divergence=%s)", got.DivergenceStroops)
	}
}

// TestCrossCheckForClass_PartialRoutesEscrowLeg: the dispatch entry point the
// CrossCheckRefresher calls must carry the escrow leg through, or production would silently
// regress to the one-sided check while every direct CrossCheckSubsetBound test still passed.
func TestCrossCheckForClass_PartialRoutesEscrowLeg(t *testing.T) {
	for _, class := range []supply.WrapClass{supply.WrapClassPartial, "", supply.WrapClass("bogus")} {
		got, err := supply.CrossCheckForClass(
			supplyWithSACWrapped(usdcClassicKey, 5_000_000, 1_000_000),
			supplyWithTotal(usdcSACKey, 999_000),
			class,
		)
		if err != nil {
			t.Fatalf("CrossCheckForClass(%q): %v", class, err)
		}
		if !got.SubsetBoundChecked {
			t.Errorf("class %q: SubsetBoundChecked = false, want true", class)
		}
		if got.WithinTolerance {
			t.Errorf("class %q: WithinTolerance = true on a 1000-stroop escrow breach", class)
		}
		if got.DivergenceStroops.Cmp(big.NewInt(1000)) != 0 {
			t.Errorf("class %q: DivergenceStroops = %s, want 1000", class, got.DivergenceStroops)
		}
	}
}

// TestCrossCheckForClass_RefusesMisalignedLedgers pins the misaligned-ledger guard
// at the shared entry point, so no caller (refresher or CLI) can compare
// two snapshots computed more than CrossCheckLedgerTolerance apart.
func TestCrossCheckForClass_RefusesMisalignedLedgers(t *testing.T) {
	t.Parallel()
	classic := supply.Supply{
		AssetKey: usdcClassicKey, TotalSupply: big.NewInt(1_000),
		SACWrappedStroops: big.NewInt(400), LedgerSequence: 60_000_000,
	}
	sac := supply.Supply{AssetKey: usdcSACKey, TotalSupply: big.NewInt(1_000), LedgerSequence: 60_000_000}

	for _, class := range []supply.WrapClass{supply.WrapClassPartial, supply.WrapClassFull} {
		sac.LedgerSequence = classic.LedgerSequence - supply.CrossCheckLedgerTolerance - 1
		if _, err := supply.CrossCheckForClass(classic, sac, class); !errors.Is(err, supply.ErrCrossCheckMisaligned) {
			t.Errorf("%s: gap tolerance+1: err = %v, want ErrCrossCheckMisaligned", class, err)
		}

		sac.LedgerSequence = classic.LedgerSequence - supply.CrossCheckLedgerTolerance
		res, err := supply.CrossCheckForClass(classic, sac, class)
		if err != nil {
			t.Fatalf("%s: gap == tolerance: err = %v, want a verdict", class, err)
		}
		if res.ClassicLedger != classic.LedgerSequence || res.SACLedger != sac.LedgerSequence {
			t.Errorf("%s: result ledgers = (%d,%d), want (%d,%d)", class,
				res.ClassicLedger, res.SACLedger, classic.LedgerSequence, sac.LedgerSequence)
		}

		sac.LedgerSequence = 0 // no anchor recorded: permissive, as the freshness gate is
		if _, err := supply.CrossCheckForClass(classic, sac, class); err != nil {
			t.Errorf("%s: unanchored snapshot: err = %v, want a verdict", class, err)
		}
	}
}

// fakeSnapshotReader returns canned per-key supplies. Errors come
// from a parallel map so tests can wire ErrNoSnapshot / transient
// errors per asset_key.
type fakeSnapshotReader struct {
	mu       sync.Mutex
	supplies map[string]supply.Supply
	errs     map[string]error
}

func (f *fakeSnapshotReader) LatestSupply(_ context.Context, k string) (supply.Supply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.errs[k]; ok {
		return supply.Supply{}, e
	}
	if s, ok := f.supplies[k]; ok {
		return s, nil
	}
	return supply.Supply{}, supply.ErrNoSnapshot
}

// captureEmitter records every emission so tests can assert on the
// exact gauge + counter calls.
type captureEmitter struct {
	mu          sync.Mutex
	divergences []divergenceCall
	outcomes    []outcomeCall
	// gauge mirrors the live GaugeVec: Divergence sets a series,
	// ClearDivergence deletes it.
	gauge map[string]float64
}

func gaugeSeries(classicKey string, wrapClass supply.WrapClass) string {
	return classicKey + "|" + string(wrapClass)
}

type divergenceCall struct {
	ClassicKey string
	WrapClass  supply.WrapClass
	Stroops    float64
}

type outcomeCall struct {
	Kind      supply.CrossCheckOutcomeKind
	WrapClass supply.WrapClass
}

func (c *captureEmitter) Divergence(k string, wrapClass supply.WrapClass, s float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.divergences = append(c.divergences, divergenceCall{ClassicKey: k, WrapClass: wrapClass, Stroops: s})
	if c.gauge == nil {
		c.gauge = map[string]float64{}
	}
	c.gauge[gaugeSeries(k, wrapClass)] = s
}

func (c *captureEmitter) ClearDivergence(k string, wrapClass supply.WrapClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.gauge, gaugeSeries(k, wrapClass))
}

func (c *captureEmitter) liveSeries(k string, wrapClass supply.WrapClass) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.gauge[gaugeSeries(k, wrapClass)]
	return v, ok
}

func (c *captureEmitter) Outcome(k supply.CrossCheckOutcomeKind, wrapClass supply.WrapClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes = append(c.outcomes, outcomeCall{Kind: k, WrapClass: wrapClass})
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestCrossCheckRefresher_NoPairsIsNoOp — empty pairs slice returns
// nil and emits no metrics. No-op-when-unconfigured is the same
// pattern as the watched-set decoders.
func TestCrossCheckRefresher_NoPairsIsNoOp(t *testing.T) {
	t.Parallel()
	emitter := &captureEmitter{}
	r, err := supply.NewCrossCheckRefresher(nil, &fakeSnapshotReader{}, emitter, newSilentLogger())
	if err != nil {
		t.Fatalf("NewCrossCheckRefresher: %v", err)
	}
	if got := r.Tick(context.Background()); got != nil {
		t.Fatalf("Tick on empty pairs: got %v, want nil", got)
	}
	if len(emitter.outcomes) != 0 || len(emitter.divergences) != 0 {
		t.Fatalf("emitted on empty pairs: outcomes=%v divergences=%v", emitter.outcomes, emitter.divergences)
	}
}

// TestCrossCheckRefresher_RejectsBadInput — empty keys and duplicate
// classic_keys are operator-config bugs caught at construction.
func TestCrossCheckRefresher_RejectsBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		pairs []supply.CrossCheckPair
	}{
		{"empty classic", []supply.CrossCheckPair{{ClassicKey: "", SACKey: "C..."}}},
		{"empty sac", []supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: ""}}},
		{"duplicate classic", []supply.CrossCheckPair{
			{ClassicKey: "USDC:G...", SACKey: "C1"},
			{ClassicKey: "USDC:G...", SACKey: "C2"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := supply.NewCrossCheckRefresher(tc.pairs, &fakeSnapshotReader{}, &captureEmitter{}, newSilentLogger())
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestCrossCheckRefresher_WithinTolerance — equal totals → "within" outcome + gauge=0. WrapClass is
// left unset (defaults to [supply.WrapClassPartial]); an exact match is Within under either class,
// so the class-dependent branch is covered by the WrapClass-specific tests below.
func TestCrossCheckRefresher_WithinTolerance(t *testing.T) {
	t.Parallel()
	classic := supply.Supply{
		AssetKey:    "USDC:G...",
		TotalSupply: big.NewInt(100_000_000_000), SACWrappedStroops: big.NewInt(100_000_000_000),
	}
	sac := supply.Supply{
		AssetKey:    "CCONTRACT",
		TotalSupply: big.NewInt(100_000_000_000),
	}
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": classic,
		"CCONTRACT": sac,
	}}
	emitter := &captureEmitter{}
	r, err := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT"}},
		reader, emitter, newSilentLogger(),
	)
	if err != nil {
		t.Fatalf("NewCrossCheckRefresher: %v", err)
	}
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("Tick: got %#v, want one Within", got)
	}
	if len(emitter.outcomes) != 1 || emitter.outcomes[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("outcomes: %v", emitter.outcomes)
	}
	if emitter.outcomes[0].WrapClass != supply.WrapClassPartial {
		t.Fatalf("outcome wrap_class = %q, want %q", emitter.outcomes[0].WrapClass, supply.WrapClassPartial)
	}
	if len(emitter.divergences) != 1 || emitter.divergences[0].Stroops != 0 {
		t.Fatalf("divergence: %v", emitter.divergences)
	}
}

// TestCrossCheckRefresher_PartialWrapClassicExceedsSacIsBenign pins that a default (partial-wrap)
// pair whose classic total vastly exceeds the SAC total (the AQUA shape, a false positive under an
// equality compare) lands Within with zero divergence, NOT Over.
func TestCrossCheckRefresher_PartialWrapClassicExceedsSacIsBenign(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"AQUA:G...": {AssetKey: "AQUA:G...", TotalSupply: big.NewInt(86_400_000_000_0000000), SACWrappedStroops: big.NewInt(0)},
		"CAQUASAC":  {AssetKey: "CAQUASAC", TotalSupply: big.NewInt(0)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		// WrapClass intentionally left unset — the default must be safe.
		[]supply.CrossCheckPair{{ClassicKey: "AQUA:G...", SACKey: "CAQUASAC"}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("Tick: got %#v, want one Within (the AQUA false positive must be fixed)", got)
	}
	if got[0].Result.DivergenceStroops.Sign() != 0 {
		t.Fatalf("divergence stroops: got %s, want 0", got[0].Result.DivergenceStroops)
	}
	if len(emitter.divergences) != 1 || emitter.divergences[0].Stroops != 0 {
		t.Fatalf("emitted divergence: got %v, want 0", emitter.divergences)
	}
	if emitter.divergences[0].WrapClass != supply.WrapClassPartial {
		t.Fatalf("emitted wrap_class: got %q, want %q", emitter.divergences[0].WrapClass, supply.WrapClassPartial)
	}
}

// For a partial-wrap pair, SAC total exceeding classic total is impossible under correct accounting and MUST still fire
// an Over outcome (escrow != minted).
func TestCrossCheckRefresher_PartialWrapOverMintIsDiagnostic(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		// Escrow equals the SAC total, so leg 2 is evaluated and clean.
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_000), SACWrappedStroops: big.NewInt(100_000_000_002)},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_002)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	// Over-mint is diagnostic-only (the BLND/PHO false-positive
	// class) — the outcome stays within-tolerance and
	// the gap is carried on OverMintStroops, not the paging gauge.
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("Tick: got %#v, want one Within (over-mint diagnostic)", got)
	}
	if got[0].Result.OverMintStroops.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("over-mint stroops: got %s, want 2", got[0].Result.OverMintStroops)
	}
	if got[0].Result.DivergenceStroops.Sign() != 0 {
		t.Fatalf("divergence stroops: got %s, want 0", got[0].Result.DivergenceStroops)
	}
	if emitter.outcomes[0].WrapClass != supply.WrapClassPartial {
		t.Fatalf("outcome wrap_class: got %q, want %q", emitter.outcomes[0].WrapClass, supply.WrapClassPartial)
	}
}

// A partial-wrap pair whose classic snapshot has no escrow (SACWrappedStroops nil, no sac_balance_observations row)
// evaluated no paging leg; it must report unchecked and clear the gauge, never publish "within" with divergence 0.
func TestCrossCheckRefresher_PartialWrapWithoutEscrowIsUnchecked(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_000)},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_002)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeUnchecked {
		t.Fatalf("Tick: got %#v, want one Unchecked", got)
	}
	if len(emitter.outcomes) != 1 || emitter.outcomes[0].Kind != supply.CrossCheckOutcomeUnchecked {
		t.Fatalf("emitted outcomes %#v, want one unchecked", emitter.outcomes)
	}
	if len(emitter.divergences) != 0 {
		t.Fatalf("divergence gauge written %#v; an unchecked pair must not publish a verdict", emitter.divergences)
	}
	if _, ok := emitter.gauge[gaugeSeries("USDC:G...", supply.WrapClassPartial)]; ok {
		t.Fatal("divergence gauge series still present; want it cleared")
	}
}

// Each side's snapshot is the LATEST from its own refresher, so they need not be contemporaneous; a SAC snapshot far
// ahead of a stalled classic one would fake an over-mint. The refresher must report `misaligned`: no verdict, no gauge.
func TestCrossCheckRefresher_MisalignedLedgersNeitherPassesNorPages(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		// Classic snapshot stalled at ledger 50,000,000 with a total
		// that has since grown; SAC is current at 50,050,000.
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_000), LedgerSequence: 50_000_000},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_002), LedgerSequence: 50_050_000},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeMisaligned {
		t.Fatalf("Tick: got %#v, want one Misaligned", got)
	}
	if got[0].Err == nil {
		t.Error("Misaligned outcome carries no Err; operators need the gap in the log line")
	}
	// Neither passes nor pages: no divergence gauge emission at all —
	// recording 0 would read as "checked, agreed", recording 2 pages.
	if len(emitter.divergences) != 0 {
		t.Errorf("emitted divergence gauge on a misaligned pair: %#v", emitter.divergences)
	}
	if len(emitter.outcomes) != 1 || emitter.outcomes[0].Kind != supply.CrossCheckOutcomeMisaligned {
		t.Errorf("outcomes = %#v, want one misaligned", emitter.outcomes)
	}
}

// The alignment gate must not swallow the real check: a gap inside supply.CrossCheckLedgerTolerance still yields a
// verdict, and a genuine over-mint at aligned ledgers still pages.
func TestCrossCheckRefresher_AlignedLedgersStillCompare(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_000), LedgerSequence: 50_000_000, SACWrappedStroops: big.NewInt(100_000_000_002)},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_002), LedgerSequence: 50_000_000 + supply.CrossCheckLedgerTolerance},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	// Still COMPARED at the ledger-tolerance boundary (that is what
	// this test pins); the over-mint result itself is diagnostic-only.
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("Tick: got %#v, want one Within — the boundary gap is still comparable", got)
	}
	if got[0].Result.OverMintStroops.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("over-mint stroops: got %s, want 2", got[0].Result.OverMintStroops)
	}
}

// An operator-attested supply.WrapClassFull pair keeps the original ADR-0011 equality semantics: classic exceeding sac
// by more than tolerance still fires.
func TestCrossCheckRefresher_FullWrapStillAlertsOnMismatch(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(100_000_000_002)},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_000)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassFull}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeOver {
		t.Fatalf("Tick: got %#v, want one Over", got)
	}
	if got[0].Result.DivergenceStroops.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("divergence stroops: got %s, want 2", got[0].Result.DivergenceStroops)
	}
	if emitter.outcomes[0].WrapClass != supply.WrapClassFull {
		t.Fatalf("outcome wrap_class: got %q, want %q", emitter.outcomes[0].WrapClass, supply.WrapClassFull)
	}
}

// TestCrossCheckRefresher_PartialWrapEscrowExceedsSacFires pins leg 2: leg 1 is diagnostic-only,
// so leg 2 (classic.SACWrappedStroops ≤ sac.TotalSupply) is the ONLY direction that can raise
// stellarindex_supply_cross_check_divergence_stroops, yet every other partial-wrap fixture here
// leaves SACWrappedStroops nil, so leg 2 never evaluates through the production entry point. A
// classic snapshot with more escrowed-in-SAC stroops than the SAC's own total_supply is impossible
// under correct accounting and MUST page.
func TestCrossCheckRefresher_PartialWrapEscrowExceedsSacFires(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
		"USDC:G...": {
			AssetKey:          "USDC:G...",
			TotalSupply:       big.NewInt(100_000_000_000),
			SACWrappedStroops: big.NewInt(100_000_000_010),
		},
		"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(100_000_000_000)},
	}}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT", WrapClass: supply.WrapClassPartial}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeOver {
		t.Fatalf("Tick: got %#v, want one Over (escrow exceeding sac_total must page)", got)
	}
	if !got[0].Result.SubsetBoundChecked {
		t.Fatal("SubsetBoundChecked = false, want true: leg 2 must have evaluated")
	}
	if got[0].Result.EscrowExcessStroops == nil || got[0].Result.EscrowExcessStroops.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("escrow excess stroops: got %v, want 10", got[0].Result.EscrowExcessStroops)
	}
	if len(emitter.divergences) != 1 || emitter.divergences[0].Stroops != 10 {
		t.Fatalf("emitted divergence: got %v, want 10", emitter.divergences)
	}
	if len(emitter.outcomes) != 1 || emitter.outcomes[0].Kind != supply.CrossCheckOutcomeOver {
		t.Fatalf("outcomes: %v, want one over", emitter.outcomes)
	}
}

// TestCrossCheckRefresher_MissingSnapshot — no rows yet for either
// side → "missing_snapshot" outcome and NO gauge update (the
// bootstrap state must not look like "checked, agreed").
func TestCrossCheckRefresher_MissingSnapshot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		supplies map[string]supply.Supply
	}{
		{
			name:     "neither side has a snapshot",
			supplies: map[string]supply.Supply{},
		},
		{
			name: "only classic has a snapshot",
			supplies: map[string]supply.Supply{
				"USDC:G...": {AssetKey: "USDC:G...", TotalSupply: big.NewInt(1)},
			},
		},
		{
			name: "only sac has a snapshot",
			supplies: map[string]supply.Supply{
				"CCONTRACT": {AssetKey: "CCONTRACT", TotalSupply: big.NewInt(1)},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeSnapshotReader{supplies: tc.supplies}
			emitter := &captureEmitter{}
			r, _ := supply.NewCrossCheckRefresher(
				[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT"}},
				reader, emitter, newSilentLogger(),
			)
			got := r.Tick(context.Background())
			if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeMissing {
				t.Fatalf("Tick: got %#v, want one Missing", got)
			}
			if len(emitter.divergences) != 0 {
				t.Fatalf("expected no gauge update on missing, got %v", emitter.divergences)
			}
		})
	}
}

// TestCrossCheckRefresher_ReadError — non-ErrNoSnapshot read failure
// surfaces as "read_error" without updating the gauge.
func TestCrossCheckRefresher_ReadError(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{
		errs: map[string]error{"USDC:G...": errors.New("connection refused")},
	}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{{ClassicKey: "USDC:G...", SACKey: "CCONTRACT"}},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 1 || got[0].Kind != supply.CrossCheckOutcomeReadError {
		t.Fatalf("Tick: got %#v, want one ReadError", got)
	}
	if len(emitter.divergences) != 0 {
		t.Fatalf("expected no gauge update on read error, got %v", emitter.divergences)
	}
}

// TestCrossCheckRefresher_PerPairIsolation — one pair failing
// doesn't drop the next pair's cross-check.
func TestCrossCheckRefresher_PerPairIsolation(t *testing.T) {
	t.Parallel()
	reader := &fakeSnapshotReader{
		supplies: map[string]supply.Supply{
			"BTC:G...": {AssetKey: "BTC:G...", TotalSupply: big.NewInt(50), SACWrappedStroops: big.NewInt(50)},
			"CSACBTC":  {AssetKey: "CSACBTC", TotalSupply: big.NewInt(50)},
		},
		errs: map[string]error{
			"USDC:G...": errors.New("transient"),
		},
	}
	emitter := &captureEmitter{}
	r, _ := supply.NewCrossCheckRefresher(
		[]supply.CrossCheckPair{
			{ClassicKey: "USDC:G...", SACKey: "CSACUSDC"},
			{ClassicKey: "BTC:G...", SACKey: "CSACBTC"},
		},
		reader, emitter, newSilentLogger(),
	)
	got := r.Tick(context.Background())
	if len(got) != 2 {
		t.Fatalf("Tick: want 2 outcomes, got %d", len(got))
	}
	// Sorted by ClassicKey: BTC:G... before USDC:G...
	if got[0].Pair.ClassicKey != "BTC:G..." || got[0].Kind != supply.CrossCheckOutcomeWithin {
		t.Fatalf("got[0]: %#v", got[0])
	}
	if got[1].Pair.ClassicKey != "USDC:G..." || got[1].Kind != supply.CrossCheckOutcomeReadError {
		t.Fatalf("got[1]: %#v", got[1])
	}
}

// A pair that stops being evaluable must stop serving its last agreement: Prometheus re-exports a GaugeVec series on
// every scrape, so a stalled classic refresher would otherwise read as a healthy 0 forever while the divergence alert
// stays silent.
func TestCrossCheckRefresher_NonEvaluableOutcomeClearsGauge(t *testing.T) {
	t.Parallel()
	const classicKey, sacKey = "USDC:G...", "CCONTRACT"
	cases := []struct {
		name  string
		want  supply.CrossCheckOutcomeKind
		stall func(f *fakeSnapshotReader)
	}{
		{"read_error", supply.CrossCheckOutcomeReadError, func(f *fakeSnapshotReader) {
			f.errs = map[string]error{classicKey: errors.New("connection refused")}
		}},
		{"missing_snapshot", supply.CrossCheckOutcomeMissing, func(f *fakeSnapshotReader) {
			delete(f.supplies, sacKey)
		}},
		{"misaligned", supply.CrossCheckOutcomeMisaligned, func(f *fakeSnapshotReader) {
			s := f.supplies[sacKey]
			s.LedgerSequence += supply.CrossCheckLedgerTolerance + 1
			f.supplies[sacKey] = s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeSnapshotReader{supplies: map[string]supply.Supply{
				classicKey: {AssetKey: classicKey, TotalSupply: big.NewInt(100), LedgerSequence: 50_000_000, SACWrappedStroops: big.NewInt(100)},
				sacKey:     {AssetKey: sacKey, TotalSupply: big.NewInt(100), LedgerSequence: 50_000_000},
			}}
			emitter := &captureEmitter{}
			r, err := supply.NewCrossCheckRefresher(
				[]supply.CrossCheckPair{{ClassicKey: classicKey, SACKey: sacKey, WrapClass: supply.WrapClassPartial}},
				reader, emitter, newSilentLogger(),
			)
			if err != nil {
				t.Fatalf("NewCrossCheckRefresher: %v", err)
			}
			if got := r.Tick(context.Background()); got[0].Kind != supply.CrossCheckOutcomeWithin {
				t.Fatalf("first tick: got %q, want within", got[0].Kind)
			}
			if v, ok := emitter.liveSeries(classicKey, supply.WrapClassPartial); !ok || v != 0 {
				t.Fatalf("after within: series=(%v, %v), want (0, true)", v, ok)
			}

			reader.mu.Lock()
			tc.stall(reader)
			reader.mu.Unlock()

			if got := r.Tick(context.Background()); got[0].Kind != tc.want {
				t.Fatalf("second tick: got %q, want %q", got[0].Kind, tc.want)
			}
			if v, ok := emitter.liveSeries(classicKey, supply.WrapClassPartial); ok {
				t.Fatalf("after %s the gauge still serves %v — a stale verdict re-exported every scrape", tc.want, v)
			}
		})
	}
}
