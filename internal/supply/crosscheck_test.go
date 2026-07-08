package supply_test

import (
	"errors"
	"math/big"
	"testing"

	"github.com/StellarIndex/stellar-index/internal/supply"
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

// TestCrossCheck_PreservesInputs — ClassicTotal / SACTotal on the
// result are independent copies of the inputs (so log lines and
// dashboards can quote them without re-querying), AND mutating them
// later does not corrupt the originals.
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

// ─── CrossCheckSubsetBound (2026-07-08 decision, BACKLOG #59) ─────────
//
// These tests are the direct regression coverage for the category-
// error fix: `stellarindex_supply_cross_check_divergence` fired 8
// false positives because Algorithm 2's classic TOTAL was compared
// for equality against Algorithm 3's SAC-wrapped total, which is only
// a true invariant for a fully-SAC-represented asset. The corrected
// invariant is a subset bound: sac_total can never exceed
// classic_total (SACWrapped is one of Algorithm 2's own non-negative
// addends — see ClassicSupplyComponents), so only sac_total >
// classic_total is a genuine violation; classic_total > sac_total
// (the AQUA-shaped case) is the expected, unremarkable state for a
// partially-wrapped asset.

// TestCrossCheckSubsetBound_ClassicExceedsSacIsBenign — the AQUA
// example from the crosscheck.go package doc: classic total ≈ 86.4B,
// SAC total ≈ 0. Under the OLD equality compare this fired a false
// positive; under the subset bound it must report zero divergence.
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

// TestCrossCheckSubsetBound_ExactMatchIsWithin — a fully-wrapped
// asset's totals happening to match exactly is still a benign zero
// divergence under the subset bound (it's a special case of ≤, not a
// distinct code path).
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

// TestCrossCheckSubsetBound_OneStroopOvershootTolerated — sac
// exceeding classic by exactly the documented 1-stroop tolerance must
// not alert, matching CrossCheck's equality-side tolerance boundary.
func TestCrossCheckSubsetBound_OneStroopOvershootTolerated(t *testing.T) {
	got, err := supply.CrossCheckSubsetBound(
		supplyWithTotal(usdcClassicKey, 1_000_000_000),
		supplyWithTotal(usdcSACKey, 1_000_000_001),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.DivergenceStroops.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("DivergenceStroops = %s, want 1", got.DivergenceStroops)
	}
	if !got.WithinTolerance {
		t.Errorf("1-stroop overshoot must be tolerated; got WithinTolerance=false")
	}
}

// TestCrossCheckSubsetBound_OverMintFires — sac exceeding classic by
// MORE than tolerance is impossible under correct accounting (the
// wrapped amount cannot exceed the total it's wrapped from) and MUST
// fire regardless of wrap fraction — this is the "genuine
// escrow != minted violation" the 2026-07-08 decision requires to
// keep working.
func TestCrossCheckSubsetBound_OverMintFires(t *testing.T) {
	got, err := supply.CrossCheckSubsetBound(
		supplyWithTotal(usdcClassicKey, 1_000_000_000),
		supplyWithTotal(usdcSACKey, 1_000_000_005),
	)
	if err != nil {
		t.Fatalf("CrossCheckSubsetBound: %v", err)
	}
	if got.DivergenceStroops.Cmp(big.NewInt(5)) != 0 {
		t.Errorf("DivergenceStroops = %s, want 5", got.DivergenceStroops)
	}
	if got.WithinTolerance {
		t.Error("5-stroop over-mint must trigger alert")
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

// TestCrossCheckForClass_Dispatch table-tests every WrapClass input
// (including the zero value and an unrecognized string) against the
// same (classic, sac) pair where classic > sac by 2 stroops — a value
// that is a VIOLATION under WrapClassFull (equality) but BENIGN under
// WrapClassPartial (subset bound). This is the crux of the fix: which
// class a pair carries changes whether identical inputs alert.
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
