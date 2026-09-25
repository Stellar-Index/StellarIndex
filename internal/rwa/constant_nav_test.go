package rwa

import (
	"math/big"
	"strings"
	"testing"
	"time"
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

// Every binding is evidenced by its OWN share class's page and carries
// a review bound derived from the date that page was read. The IB
// binding once cited the AB class's page — a VerifiedOn claim pointing
// at evidence for a different share class — which is the drift this
// pins shut: the source URL must name the binding's own ISIN.
func TestConstantNAV_EveryBindingIsEvidencedAndBounded(t *testing.T) {
	bindings := ConstantNAVBindings()
	if len(bindings) == 0 {
		t.Fatal("no bindings: the table under test is empty")
	}
	for _, b := range bindings {
		if b.Source == "" || b.ISIN == "" || b.VerifiedOn == "" || b.ReviewBy == "" {
			t.Errorf("%s: source %q, ISIN %q, verified %q, review by %q — every one must be set", b.Code, b.Source, b.ISIN, b.VerifiedOn, b.ReviewBy)
			continue
		}
		if !strings.HasPrefix(b.Source, "https://") {
			t.Errorf("%s: source %q is not an https URL", b.Code, b.Source)
		}
		if !strings.Contains(b.Source, b.ISIN) {
			t.Errorf("%s: source %q does not name the binding's own ISIN %s — it is evidence for a different share class", b.Code, b.Source, b.ISIN)
		}
		verified, err := time.Parse(constantNAVDateLayout, b.VerifiedOn)
		if err != nil {
			t.Errorf("%s: VerifiedOn %q: %v", b.Code, b.VerifiedOn, err)
			continue
		}
		reviewBy := b.ReviewDeadline()
		if reviewBy.IsZero() {
			t.Errorf("%s: ReviewBy %q does not parse", b.Code, b.ReviewBy)
			continue
		}
		if !reviewBy.After(verified) {
			t.Errorf("%s: ReviewBy %s is not after VerifiedOn %s", b.Code, b.ReviewBy, b.VerifiedOn)
		}
		if got := reviewBy.Sub(verified); got != ConstantNAVReviewInterval {
			t.Errorf("%s: ReviewBy − VerifiedOn = %s, want the documented interval %s", b.Code, got, ConstantNAVReviewInterval)
		}
	}
	// The sibling classes are keyed to different pages: a copy-paste of
	// one class's URL onto the other is exactly the defect above.
	seen := map[string]string{}
	for _, b := range bindings {
		if other, dup := seen[b.Source]; dup {
			t.Errorf("%s and %s cite the same page %s", other, b.Code, b.Source)
		}
		seen[b.Source] = b.Code
	}
}

// The review bound flips exactly once, at the deadline: standing through
// the instant the interval ends, due from the first instant after it.
func TestConstantNAV_ReviewDueFlipsAtTheDeadline(t *testing.T) {
	b, ok := ConstantNAV("gBENJI", franklinLuxIBIssuer)
	if !ok {
		t.Fatal("gBENJI binding missing")
	}
	if b.VerifiedOn != "2026-09-16" || b.ReviewBy != "2026-12-15" {
		t.Fatalf("gBENJI verified %s review by %s, want 2026-09-16 → 2026-12-15 (90 days)", b.VerifiedOn, b.ReviewBy)
	}
	deadline := time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	if got := b.ReviewDeadline(); !got.Equal(deadline) {
		t.Fatalf("ReviewDeadline = %s, want %s", got, deadline)
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		due  bool
	}{
		{"the day it was verified", time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC), false},
		{"the day before the deadline", deadline.Add(-24 * time.Hour), false},
		{"one second before", deadline.Add(-time.Second), false},
		{"the deadline itself", deadline, false},
		{"one second after", deadline.Add(time.Second), true},
		{"a year later", deadline.AddDate(1, 0, 0), true},
	} {
		if got := b.ReviewDue(tc.now); got != tc.due {
			t.Errorf("%s (%s): due = %v, want %v", tc.name, tc.now.Format(time.RFC3339), got, tc.due)
		}
	}
	// A binding whose bound cannot be read is due, not indefinitely at
	// par: the failure mode is a label, never a silent constant.
	broken := ConstantNAVBinding{Code: "x", ReviewBy: "not a date"}
	if !broken.ReviewDue(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("an unreadable ReviewBy must fail closed as due")
	}
}

// The table's own ISINs are keys: well-formed, upper-case and each on
// exactly one pair, or the process refuses to start.
func TestIndexConstantNAV_RefusesAnAmbiguousTable(t *testing.T) {
	ib := ConstantNAVBinding{Code: "gBENJI", Issuer: franklinLuxIBIssuer, ISIN: "LU2900381208", NAVUSD: "1.00", VerifiedOn: "2026-09-16"}
	ab := ConstantNAVBinding{Code: "grBENJI", Issuer: franklinLuxABIssuer, ISIN: "LU3258450587", NAVUSD: "1.00", VerifiedOn: "2026-09-16"}
	with := func(b ConstantNAVBinding, f func(*ConstantNAVBinding)) ConstantNAVBinding { f(&b); return b }
	cases := map[string][]ConstantNAVBinding{
		"one ISIN on two issuers": {ib, with(ab, func(b *ConstantNAVBinding) { b.ISIN = ib.ISIN })},
		"one pair bound twice":    {ib, with(ab, func(b *ConstantNAVBinding) { b.Code, b.Issuer = ib.Code, ib.Issuer })},
		"lower-case ISIN":         {with(ib, func(b *ConstantNAVBinding) { b.ISIN = "lu2900381208" })},
		"bad check digit":         {with(ib, func(b *ConstantNAVBinding) { b.ISIN = "LU2900381209" })},
		"unparseable date":        {with(ib, func(b *ConstantNAVBinding) { b.VerifiedOn = "16/09/2026" })},
	}
	for name, bindings := range cases {
		if _, err := indexConstantNAV(bindings); err == nil {
			t.Errorf("%s: indexed without error", name)
		}
	}
	good := []ConstantNAVBinding{ib, ab}
	tbl, err := indexConstantNAV(good)
	if err != nil {
		t.Fatalf("a well-formed table: %v", err)
	}
	if tbl.byISIN["LU3258450587"].Code != "grBENJI" || good[0].ReviewBy != "2026-12-15" {
		t.Errorf("index = %+v, bindings = %+v", tbl, good)
	}
}

// A declared ISIN contradicts the table when the pair is bound to
// another class or the ISIN is bound to another pair; a matching or
// non-ISIN declaration contradicts nothing.
func TestConstantNAVISINConflict(t *testing.T) {
	cases := []struct {
		code, issuer, declared string
		want                   bool
	}{
		{"gBENJI", franklinLuxIBIssuer, "LU2900381208", false},
		{"gBENJI", franklinLuxIBIssuer, " lu2900381208 ", false},
		{"gBENJI", franklinLuxIBIssuer, "LU3258450587", true},
		{"gBENJI", franklinLuxIBIssuer, "US0378331005", true},
		{"gBENJI", franklinLuxIBIssuer, "LU2900381209", false},
		{"gBENJI", franklinLuxIBIssuer, "Franklin OnChain Fund", false},
		{"gBENJI", "GAIMPOSTOR", "LU2900381208", true},
		{"FAKE", franklinLuxIBIssuer, "LU2900381208", true},
		{"FAKE", "GAIMPOSTOR", "US0378331005", false},
	}
	for _, c := range cases {
		if got := ConstantNAVISINConflict(c.code, c.issuer, c.declared); got != c.want {
			t.Errorf("ConstantNAVISINConflict(%q, %q, %q) = %v, want %v", c.code, c.issuer, c.declared, got, c.want)
		}
	}
}
