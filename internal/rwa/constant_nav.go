package rwa

import "strings"

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
	// Source is the issuer's published NAV page the binding was checked
	// against, and VerifiedOn the date it read the prescribed value.
	Source     string
	VerifiedOn string
}

const (
	franklinLuxIBIssuer = "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP"
	franklinLuxABIssuer = "GA3ZBL3LBRKOF7CZ6MCA7JLPHWQCGYCCGKH4GVWNEDZOXW4IPXFGN2FQ"
)

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
		Source: "https://www.franklintempleton.lu/our-funds/price-and-performance-money-funds/products/41372/PX1/franklin-on-chain-u-s-government-money-fund/LU3258450587", VerifiedOn: "2026-09-16",
	},
	{
		Code: "grBENJI", Issuer: franklinLuxABIssuer, ISIN: "LU3258450587", NAVUSD: "1.00",
		Fund: "Franklin OnChain U.S. Government Liquidity Fund — AB (Ddis) USD", Regime: "EU MMFR short-term public-debt CNAV",
		Source: "https://www.franklintempleton.lu/our-funds/price-and-performance-money-funds/products/41372/PX1/franklin-on-chain-u-s-government-money-fund/LU3258450587", VerifiedOn: "2026-09-16",
	},
}

var constantNAVIndex = func() map[instrumentKey]ConstantNAVBinding {
	m := make(map[instrumentKey]ConstantNAVBinding, len(constantNAVBindings))
	for _, b := range constantNAVBindings {
		m[instrumentKey{code: b.Code, issuer: b.Issuer}] = b
	}
	return m
}()

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
