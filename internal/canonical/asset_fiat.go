package canonical

import "sort"

// Off-chain fiat asset helpers — see ADR-0010.
//
// The Asset type carries an AssetFiat variant for off-chain fiat
// currencies (USD, EUR, …). These are NOT Stellar assets; they're
// abstract reference currencies used by oracle prices + FX feeds.
//
// Wire form: `fiat:<ISO4217>` (e.g. `fiat:USD`). The `fiat:` prefix
// is unambiguous, so ParseAsset dispatches in O(1).

// knownFiatCodes is the allow-list of 3-letter fiat codes. Extending
// it is a one-line change here (ADR-0010 needs no amendment).
// Codes chosen from ISO-4217 plus currencies the spec explicitly
// names or that our CEX/FX connectors will price against.
//
// It covers the codes seen in Reflector FX oracle traffic plus wider ISO-4217
// codes. Crypto tickers (BTC, ETH, SOL …) are NOT on this list; they have
// their own canonical type (asset_crypto.go).
//
// It also carries the FULL set the massive.com FX feed
// (internal/sources/external/forex) publishes into fx_quotes. The batch price
// endpoint rejects the whole request on the first code it can't parse, so the
// /assets converter can only offer the intersection of this list and the feed.
// Obsolete codes (CYP/EEK/LTL/LVL/MTL/ROL/SIT/SKK/TRL) are inert without a
// fresh rate and never resolve to a price.
//
// VES is listed because the Reflector FX oracle publishes a VES slot on every
// event; the massive.com feed does not carry it, so it exists only so the
// reflector-fx row is typed fiat:VES. XAU (gold) deliberately stays OFF this
// list: it is a commodity and maps to rwa:XAU (ADR-0028).
var knownFiatCodes = map[string]struct{}{
	"AED": {}, "ALL": {}, "ARS": {}, "AUD": {}, "AWG": {}, "BAM": {},
	"BBD": {}, "BDT": {}, "BGN": {}, "BHD": {}, "BIF": {}, "BND": {},
	"BOB": {}, "BRL": {}, "BSD": {}, "BWP": {}, "BZD": {}, "CAD": {},
	"CDF": {}, "CHF": {}, "CLP": {}, "CNH": {}, "CNY": {}, "COP": {},
	"CRC": {}, "CUP": {}, "CVE": {}, "CYP": {}, "CZK": {}, "DJF": {},
	"DKK": {}, "DOP": {}, "DZD": {}, "EEK": {}, "EGP": {}, "ETB": {},
	"EUR": {}, "FJD": {}, "GBP": {}, "GHS": {}, "GMD": {}, "GNF": {},
	"GTQ": {}, "GYD": {}, "HKD": {}, "HNL": {}, "HRK": {}, "HTG": {},
	"HUF": {}, "IDR": {}, "ILS": {}, "INR": {}, "IQD": {}, "ISK": {},
	"JMD": {}, "JPY": {}, "KES": {}, "KHR": {}, "KMF": {}, "KRW": {},
	"KWD": {}, "KYD": {}, "KZT": {}, "LAK": {}, "LBP": {}, "LKR": {},
	"LRD": {}, "LSL": {}, "LTL": {}, "LVL": {}, "LYD": {}, "MAD": {},
	"MDL": {}, "MGA": {}, "MKD": {}, "MOP": {}, "MTL": {}, "MUR": {},
	"MVR": {}, "MWK": {}, "MXN": {}, "MYR": {}, "MZN": {}, "NAD": {},
	"NGN": {}, "NIO": {}, "NOK": {}, "NPR": {}, "NZD": {}, "OMR": {},
	"PAB": {}, "PEN": {}, "PGK": {}, "PHP": {}, "PKR": {}, "PLN": {},
	"PYG": {}, "QAR": {}, "ROL": {}, "RON": {}, "RSD": {}, "RUB": {},
	"RWF": {}, "SAR": {}, "SCR": {}, "SDG": {}, "SEK": {}, "SGD": {},
	"SIT": {}, "SKK": {}, "SOS": {}, "SVC": {}, "SZL": {}, "THB": {},
	"TJS": {}, "TMT": {}, "TND": {}, "TRL": {}, "TRY": {}, "TTD": {},
	"TWD": {}, "TZS": {}, "UAH": {}, "UGX": {}, "USD": {}, "UYU": {},
	"UZS": {}, "VES": {}, "VND": {}, "XPF": {}, "YER": {}, "ZAR": {},
	"ZMW": {},
}

// IsKnownFiat reports whether code is in the ADR-0010 allow-list.
// Callers use this to validate operator-supplied fiat configuration
// before constructing an [Asset] at startup.
func IsKnownFiat(code string) bool {
	_, ok := knownFiatCodes[code]
	return ok
}

// KnownFiatCodes returns the ADR-0010 allow-list, sorted. A COPY: the
// map above is the single source of truth and callers must not be able
// to widen it.
//
// It exists for the readers that need the closed set rather than a
// membership test — a query that has to bound itself to fiat-quoted rows
// cannot ask [IsKnownFiat] per row, and hard-coding a second list beside
// this one is how the two drift.
func KnownFiatCodes() []string {
	out := make([]string, 0, len(knownFiatCodes))
	for code := range knownFiatCodes {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// NewFiatAsset constructs a fiat asset. Returns ErrInvalidAsset if
// the code isn't allow-listed.
func NewFiatAsset(code string) (Asset, error) {
	if !IsKnownFiat(code) {
		return Asset{}, errorf(ErrInvalidAsset, "unknown fiat code %q (see ADR-0010)", code)
	}
	return Asset{Type: AssetFiat, Code: code}, nil
}
