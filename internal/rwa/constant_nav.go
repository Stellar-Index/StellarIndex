package rwa

import (
	"fmt"
	"strings"
	"time"
)

// ConstantNAVBinding binds an exact (code, issuer) to the NAV an EU MMFR CNAV prospectus
// fixes, served under the issuer's own provenance and re-read every [ConstantNAVReviewInterval]
// because a conversion leaves SEP-1 unchanged. Accumulating classes (e.g. sgBENJI) are measured, not fixed.
type ConstantNAVBinding struct {
	Code   string
	Issuer string
	// ISIN of the share class whose prospectus fixes the NAV.
	ISIN string
	// NAVUSD is the prescribed NAV per share, as a decimal string.
	NAVUSD string
	// Fund is the fund's own name for the share class.
	Fund string
	// Regime names the rule that fixes the NAV.
	Regime string
	// Source is the issuer's published NAV page for THIS share class —
	// the page whose URL carries the binding's own ISIN — and
	// VerifiedOn the UTC date (YYYY-MM-DD) it was read there.
	Source     string
	VerifiedOn string
	// ReviewBy is VerifiedOn plus [ConstantNAVReviewInterval], derived at indexing so it
	// cannot drift; re-verifying means advancing VerifiedOn.
	ReviewBy string
}

// ConstantNAVReviewInterval is a quarter: the NAV is fixed until the regime changes, and a
// regime change is announced in reports that are quarterly at the shortest. Longer would serve
// a converted class at par for most of a year.
const ConstantNAVReviewInterval = 90 * 24 * time.Hour

// constantNAVDateLayout is the layout VerifiedOn and ReviewBy are
// written in — a calendar date, because the issuer's page is read on a
// day, not at an instant.
const constantNAVDateLayout = "2006-01-02"

const (
	franklinLuxIBIssuer = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
	franklinLuxABIssuer = "GA3ZBL3LBRKOF7CZ6MCA7JLPHWQCGYCCGKH4GVWNEDZOXW4IPXFGN2FQ"
)

// franklinOnChainLiquidityPages is the issuer's per-class price page prefix, from its
// product sitemap. The site answers 200 for ANY trailing ISIN, so a URL is verified against
// the sitemap, not its status.
const franklinOnChainLiquidityPages = "https://www.franklintempleton.lu/our-funds/price-and-performance-money-funds/products/41372/"

var constantNAVBindings = []ConstantNAVBinding{
	// Franklin OnChain U.S. Government Liquidity Fund: CSSF-authorised short-term public-debt
	// CNAV MMF targeting $1 per share; SEP-1 at www.franklintempleton.com binds each class and
	// declares its ISIN (AB page: NAV $1.00, mark-to-market $0.9999).
	{
		Code: "gBENJI", Issuer: franklinLuxIBIssuer, ISIN: "LU2900381208", NAVUSD: "1.00",
		Fund: "Franklin OnChain U.S. Government Liquidity Fund — IB (Ddis) USD", Regime: "EU MMFR short-term public-debt CNAV",
		Source: franklinOnChainLiquidityPages + "QY2/franklin-on-chain-u-s-government-liquidity-fund/LU2900381208", VerifiedOn: "2026-09-16",
	},
	{
		Code: "grBENJI", Issuer: franklinLuxABIssuer, ISIN: "LU3258450587", NAVUSD: "1.00",
		Fund: "Franklin OnChain U.S. Government Liquidity Fund — AB (Ddis) USD", Regime: "EU MMFR short-term public-debt CNAV",
		Source: franklinOnChainLiquidityPages + "PX1/franklin-on-chain-u-s-government-liquidity-fund/LU3258450587", VerifiedOn: "2026-09-16",
	},
}

// constantNAVIndex refuses to start the process on a table defect rather than serve a
// binding it cannot vouch for.
var constantNAVIndex = func() constantNAVTable {
	t, err := indexConstantNAV(constantNAVBindings)
	if err != nil {
		panic("rwa: " + err.Error())
	}
	return t
}()

type constantNAVTable struct {
	byPair map[instrumentKey]ConstantNAVBinding
	byISIN map[string]ConstantNAVBinding
}

// indexConstantNAV stamps ReviewBy and indexes bindings; a repeated ISIN or pair (two
// securities, one identity) or a non-canonical ISIN is refused.
func indexConstantNAV(bindings []ConstantNAVBinding) (constantNAVTable, error) {
	t := constantNAVTable{
		byPair: make(map[instrumentKey]ConstantNAVBinding, len(bindings)),
		byISIN: make(map[string]ConstantNAVBinding, len(bindings)),
	}
	for i, b := range bindings {
		verified, err := time.Parse(constantNAVDateLayout, b.VerifiedOn)
		if err != nil {
			return constantNAVTable{}, fmt.Errorf("constant NAV binding %s: VerifiedOn %q is not a %s date", b.Code, b.VerifiedOn, constantNAVDateLayout)
		}
		if norm, ok := upperASCII12(b.ISIN); !ok || norm != b.ISIN || !IsISIN(b.ISIN) {
			return constantNAVTable{}, fmt.Errorf("constant NAV binding %s: ISIN %q is not a well-formed upper-case ISIN", b.Code, b.ISIN)
		}
		key := instrumentKey{code: b.Code, issuer: b.Issuer}
		if prev, dup := t.byPair[key]; dup {
			return constantNAVTable{}, fmt.Errorf("constant NAV binding %s: pair %s-%s bound twice (ISINs %s and %s)", b.Code, b.Code, b.Issuer, prev.ISIN, b.ISIN)
		}
		if prev, dup := t.byISIN[b.ISIN]; dup {
			return constantNAVTable{}, fmt.Errorf("constant NAV binding %s: ISIN %s already bound to %s-%s", b.Code, b.ISIN, prev.Code, prev.Issuer)
		}
		b.ReviewBy = verified.Add(ConstantNAVReviewInterval).Format(constantNAVDateLayout)
		bindings[i] = b
		t.byPair[key] = b
		t.byISIN[b.ISIN] = b
	}
	return t, nil
}

// ReviewDeadline is the start of the ReviewBy date, UTC; an unparseable date returns the
// zero time so a hand-built binding fails closed as already due.
func (b ConstantNAVBinding) ReviewDeadline() time.Time {
	t, err := time.Parse(constantNAVDateLayout, b.ReviewBy)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ReviewDue reports whether the binding has passed its review deadline
// at now — strictly after it, so the binding stands through the exact
// instant [ConstantNAVReviewInterval] ends and is due from the next.
func (b ConstantNAVBinding) ReviewDue(now time.Time) bool {
	return now.After(b.ReviewDeadline())
}

// ConstantNAV returns the prospectus constant-NAV binding for this exact
// (code, issuer), and whether one exists.
func ConstantNAV(code, issuer string) (ConstantNAVBinding, bool) {
	b, ok := constantNAVIndex.byPair[instrumentKey{code: strings.TrimSpace(code), issuer: strings.TrimSpace(issuer)}]
	return b, ok
}

// ConstantNAVISINConflict reports whether the SEP-1-declared ISIN contradicts the table,
// so a row would name one security and be valued as another; a malformed ISIN contradicts nothing.
func ConstantNAVISINConflict(code, issuer, declaredAnchorAsset string) bool {
	isin, ok := CanonicalISIN(declaredAnchorAsset)
	if !ok {
		return false
	}
	key := instrumentKey{code: strings.TrimSpace(code), issuer: strings.TrimSpace(issuer)}
	if b, bound := constantNAVIndex.byPair[key]; bound && b.ISIN != isin {
		return true
	}
	b, bound := constantNAVIndex.byISIN[isin]
	return bound && (instrumentKey{code: b.Code, issuer: b.Issuer}) != key
}

// ConstantNAVBindings returns the table in declaration order, for
// documentation surfaces and tests.
func ConstantNAVBindings() []ConstantNAVBinding {
	out := make([]ConstantNAVBinding, len(constantNAVBindings))
	copy(out, constantNAVBindings)
	return out
}
