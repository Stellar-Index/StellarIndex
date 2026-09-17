package rwa

import (
	"math/big"
	"testing"
)

func TestConstantNAV_IsExactOnBothHalvesAndWellFormed(t *testing.T) {
	b, ok := ConstantNAV("gBENJI", franklinLuxIBIssuer)
	if !ok || b.ISIN != "LU2900381208" || b.NAVUSD != "1.00" {
		t.Fatalf("gBENJI binding = %+v, %v", b, ok)
	}
	if _, ok := ConstantNAV("gBENJI", "GAIMPOSTOR"); ok {
		t.Error("a code alone must not bind: an impostor would be valued at par")
	}
	if _, ok := ConstantNAV("GBENJI", franklinLuxIBIssuer); ok {
		t.Error("codes are case-exact")
	}
	for _, b := range ConstantNAVBindings() {
		if !IsISIN(b.ISIN) {
			t.Errorf("%s: ISIN %q malformed", b.Code, b.ISIN)
		}
		if _, ok := new(big.Rat).SetString(b.NAVUSD); !ok {
			t.Errorf("%s: NAV %q is not a decimal", b.Code, b.NAVUSD)
		}
		if b.Source == "" || b.VerifiedOn == "" || b.Regime == "" {
			t.Errorf("%s: a constant NAV must name its source, regime and verification date", b.Code)
		}
	}
	// An accumulating class is deliberately absent.
	if _, ok := ConstantNAV("sgBENJI", "GAGICV3VBJSKKH5H5MQQIUTUP462YVHC23KUHZY6FJERRJFBDIVZBM5C"); ok {
		t.Error("sgBENJI accumulates; its NAV is a measurement and must not be bound as a constant")
	}
}
