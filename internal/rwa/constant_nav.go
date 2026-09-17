package rwa

import (
	"strings"
	"time"
)

// Prospectus constant-NAV bindings — share classes whose net asset value
// is fixed at one unit of currency by the fund's own rules, so that the
// reference price is a matter of the prospectus and not of a feed.
//
// A short-term public-debt CNAV money market fund under the EU Money
// Market Fund Regulation is authorised to maintain a constant NAV per
// share of 1 in its base currency, and publishes that NAV daily beside a
// mark-to-market shadow price. For such a share class "what is one
// share worth" has the same answer every day the fund exists, and the
// price a feed would carry is the number the regulation prescribes. This
// table binds an exact (code, issuer) to that prescribed value, with the
// ISIN it was checked against and the issuer's own page the NAV was read
// from — the reference is the issuer's published NAV, and it is served
// under its own provenance so a consumer can tell it from an oracle's
// measurement and from a listing platform's aggregate.
//
// EXACT on both halves, for the reason [InstrumentFeed] is: codes are
// not unique on this network, and a constant price on a code alone would
// value an impostor at par.
//
// A rule, but not a permanent one. The regulation lets a CNAV fund's
// mark-to-market price deviate from the constant by up to 20 basis
// points before it must convert or impose fees, and a share class is
// converted to variable NAV, merged or liquidated by a decision of the
// fund that leaves the issuer's SEP-1 exactly as it was. Nothing on
// this platform observes that decision, so every binding carries the
// date it was last read against the issuer's page and the date by which
// it must be read again — [ConstantNAVReviewInterval] — and past that
// date it is served labelled as due for re-verification. That is the
// same bound every other reference on the surface carries, at the
// cadence a prospectus fact moves rather than the cadence a feed does.
//
// What is deliberately NOT here: any accumulating share class. An
// accumulating class's NAV grows with the fund's income and is a
// measurement, not a prescription — Franklin's Singapore sgBENJI
// (SGXZ71843866, "A (acc) USD") is the case at hand, and it stays
// unpriced until a feed carries its NAV.
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
	// ReviewBy is the UTC date (YYYY-MM-DD) by which the binding must be
	// re-read against Source: VerifiedOn plus [ConstantNAVReviewInterval].
	// DERIVED when the table is indexed, never declared, so it cannot
	// drift from the verification date it is a function of. Re-verifying
	// a binding means advancing VerifiedOn; ReviewBy follows.
	ReviewBy string
}

// ConstantNAVReviewInterval is how long a constant-NAV binding stands on
// one reading of the issuer's page before it is due to be read again.
//
// Ninety days — a quarter. A public-debt CNAV fund under the MMFR
// publishes its constant NAV daily, so the figure itself never goes
// stale in the 72-hour sense an oracle observation does: it is 1.00
// every day until the fund changes regime. What CAN change is the
// regime, and a regime change is a corporate action announced to
// shareholders with notice and reflected in the fund's reports, which
// are quarterly at the shortest. A quarterly re-read of the issuer's
// page therefore catches a conversion, merger or liquidation within one
// reporting cycle of it, without asking a human to re-confirm a number
// that cannot have moved in between. Shorter would flag the binding
// stale on a cadence nothing about the fund justifies; longer would let
// a converted class be served at par for the better part of a year.
const ConstantNAVReviewInterval = 90 * 24 * time.Hour

// constantNAVDateLayout is the layout VerifiedOn and ReviewBy are
// written in — a calendar date, because the issuer's page is read on a
// day, not at an instant.
const constantNAVDateLayout = "2006-01-02"

const (
	franklinLuxIBIssuer = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
	franklinLuxABIssuer = "GA3ZBL3LBRKOF7CZ6MCA7JLPHWQCGYCCGKH4GVWNEDZOXW4IPXFGN2FQ"
)

// The issuer's Luxembourg price-and-performance pages, one per share
// class, as its own product sitemap enumerates them
// (franklintempleton.lu/binaries/content/assets/global/sitemaps/google/en-lu_product.xml):
// product 41372 is the fund, the path segment after it is the share
// class's own code, and the URL ends in the class's ISIN. The site
// answers 200 with the same application shell for ANY trailing ISIN,
// so a URL is verified against the sitemap, not against its status.
const franklinOnChainLiquidityPages = "https://www.franklintempleton.lu/our-funds/price-and-performance-money-funds/products/41372/"

var constantNAVBindings = []ConstantNAVBinding{
	// Franklin OnChain U.S. Government Liquidity Fund (Luxembourg SICAV
	// "Franklin Templeton OnChain Funds", CSSF-authorised): "qualifies as
	// a short-term public debt constant net asset value (CNAV) money
	// market fund under the European Money Market Fund Regulation",
	// objective "to maintain a constant net asset value per share of 1
	// US dollar". The issuer's SEP-1 at www.franklintempleton.com binds
	// each class to its own account and declares the ISIN; the AB class
	// page showed NAV $1.00 (mark-to-market $0.9999) on 2026-09-16.
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

// constantNAVIndex keys the table on the exact pair and stamps each
// binding's ReviewBy from its VerifiedOn. A verification date that does
// not parse is a defect in this file, and the process refuses to start
// on it rather than serve a binding whose bound cannot be computed.
var constantNAVIndex = func() map[instrumentKey]ConstantNAVBinding {
	m := make(map[instrumentKey]ConstantNAVBinding, len(constantNAVBindings))
	for i, b := range constantNAVBindings {
		verified, err := time.Parse(constantNAVDateLayout, b.VerifiedOn)
		if err != nil {
			panic("rwa: constant NAV binding " + b.Code + ": VerifiedOn " + b.VerifiedOn + " is not a " + constantNAVDateLayout + " date")
		}
		b.ReviewBy = verified.Add(ConstantNAVReviewInterval).Format(constantNAVDateLayout)
		constantNAVBindings[i] = b
		m[instrumentKey{code: b.Code, issuer: b.Issuer}] = b
	}
	return m
}()

// ReviewDeadline is the instant the binding falls due for
// re-verification: the start of the ReviewBy date, UTC.
//
// A ReviewBy that does not parse is answered with the zero time, which
// [ConstantNAVBinding.ReviewDue] reads as already due. The field is
// derived from a date the index has already parsed, so this cannot
// happen to a binding the table produced; a hand-built one fails closed
// rather than being served at par indefinitely.
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
	b, ok := constantNAVIndex[instrumentKey{code: strings.TrimSpace(code), issuer: strings.TrimSpace(issuer)}]
	return b, ok
}

// ConstantNAVBindings returns the table in declaration order, for
// documentation surfaces and tests.
func ConstantNAVBindings() []ConstantNAVBinding {
	out := make([]ConstantNAVBinding, len(constantNAVBindings))
	copy(out, constantNAVBindings)
	return out
}
