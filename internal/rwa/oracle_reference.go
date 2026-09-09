package rwa

import (
	"sort"
	"strings"
)

// Oracle reference scope — which ADR-0028 feeds may be compared against
// the price of a Stellar token, and which may not.
//
// The RWA surface admits an asset on identity and attestation
// ([Qualify]); this file answers a narrower and later question: given an
// admitted (code, issuer), is the oracle feed of that code measuring the
// SAME QUANTITY the Stellar market prices?
//
// # Why the question is not "does a feed exist"
//
// A feed prices an instrument in the instrument's own unit. Two of the
// ADR-0028 codes name a quantity that is not one token:
//
//   - `XAU` is the Reflector FX oracle's SPOT GOLD slot — one troy
//     ounce, in USD. The ADR records it as "a DISTINCT asset (spot vs
//     the Matrixdock token)". A Stellar token coded XAU is a token of
//     unstated size; nothing in the index says one of them is an ounce.
//     Publishing "$4,400" beside it would assert exactly that.
//   - `SPXU` is an exchange-traded fund's own share price. A share is
//     not a token either.
//
// Every other allow-listed code names a TOKENIZED instrument whose feed
// prices one token of it — which is the only case where the oracle
// figure and a market price are the same quantity and their ratio is a
// premium rather than a unit conversion.
//
// # Fail closed
//
// [TokenizedInstrumentCode] is an ALLOW-list, not the complement of the
// two exceptions. A code added to ADR-0028 without being classified here
// gets no reference and no premium — silence, not a wrong number — and
// TestTokenizedInstrumentCodes_ClassifyEveryKnownCode turns RED until
// someone classifies it. The opposite arrangement (deny-list) would give
// the next spot-commodity slot a token comparison by default, which is
// the D8 failure mode (a NAV published in the wrong denominator) in a
// different coordinate.
//
// # What this does NOT establish
//
// That one unit of the admitted Stellar token is one unit of the named
// instrument. The evidence for that is the issuer's own domain-bound
// SEP-1 declaration (R2) plus the independent recognition of the issuing
// account (R3) — the same evidence that admitted the asset — and the
// surface says so beside the figure rather than presenting the
// comparison as a verified equivalence.

// tokenizedInstrumentCodes are the ADR-0028 codes whose oracle feed
// prices ONE TOKEN of a tokenized instrument, so the feed value and a
// token's market price are the same quantity.
//
// Each entry is the tokenized product named in the ADR-0028 allow-list
// comment beside the code, and in the RedStone feed registry comment
// beside its feed_id.
var tokenizedInstrumentCodes = map[string]struct{}{
	"BENJI":   {}, // tokenized money-market fund share
	"iBENJI":  {}, // its index variant, likewise a token
	"GILTS":   {}, // tokenized UK gilts
	"CETES":   {}, // tokenized Mexican treasury
	"KTB":     {}, // tokenized Korean treasury bonds
	"TESOURO": {}, // tokenized Brazilian treasury
	"USTRY":   {}, // tokenized US treasury
	"USDY":    {}, // tokenized treasury-backed note
	"USST":    {}, // tokenized treasury-backed token
	"XAUm":    {}, // tokenized gold, one token per troy ounce by construction
	"deJAAA":  {}, // tokenized CLO ETF
	"deJTRSY": {}, // tokenized treasury fund
}

// offChainReferenceCodes are the ADR-0028 codes whose feed prices an
// OFF-CHAIN quantity in its own unit — a troy ounce of spot metal, one
// share of an exchange-traded fund. Held explicitly, rather than as
// "whatever is not tokenized", so the completeness test can prove every
// allow-listed code was classified deliberately.
var offChainReferenceCodes = map[string]struct{}{
	"XAU":  {}, // spot gold, one troy ounce (Reflector FX slot)
	"SPXU": {}, // one share of an inverse S&P 500 ETF
}

// TokenizedInstrumentCode reports whether the ADR-0028 oracle code
// prices one token of a tokenized instrument, and may therefore be
// compared against a Stellar token's market price.
//
// Case-insensitive, for the same reason [isOracleRWACode] is: the
// allow-list spells codes as instrument tickers (XAUm, deJAAA) while an
// on-chain asset code carries whatever case its issuer chose.
func TokenizedInstrumentCode(code string) bool {
	return inCodeSet(tokenizedInstrumentCodes, code)
}

// OffChainReferenceCode reports whether the code's feed prices an
// off-chain quantity rather than a token. Served as the stated reason a
// row carries no reference price, so the refusal reads as a measurement
// statement rather than as missing data.
func OffChainReferenceCode(code string) bool {
	return inCodeSet(offChainReferenceCodes, code)
}

// TokenizedInstrumentCodes lists the comparable codes in a stable order,
// for the same reason [AnchorClasses] is served: a consumer reads the
// rule from the response instead of inferring it from the rows present
// on the day.
func TokenizedInstrumentCodes() []string {
	out := make([]string, 0, len(tokenizedInstrumentCodes))
	for c := range tokenizedInstrumentCodes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func inCodeSet(set map[string]struct{}, code string) bool {
	code = strings.TrimSpace(code)
	if _, ok := set[code]; ok {
		return true
	}
	for known := range set {
		if strings.EqualFold(known, code) {
			return true
		}
	}
	return false
}
