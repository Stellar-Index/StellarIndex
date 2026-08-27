package canonical

// Off-chain fiat asset helpers — see ADR-0010.
//
// The Asset type carries an AssetFiat variant for off-chain fiat
// currencies (USD, EUR, …). These are NOT Stellar assets; they're
// abstract reference currencies used by oracle prices + FX feeds.
//
// Wire form: `fiat:<ISO4217>` (e.g. `fiat:USD`). The `fiat:` prefix
// is unambiguous, so ParseAsset dispatches in O(1).

// knownFiatCodes is the allow-list of 3-letter fiat codes. Extending
// it is a one-line amendment to ADR-0010 (never a superseding ADR).
// Codes chosen from ISO-4217 plus currencies the spec explicitly
// names or that our CEX/FX connectors will price against.
//
// 2026-04-23: extended after observing real Reflector FX oracle
// traffic — ARS (seen in mainnet capture under v6-2026-04-23/), plus
// a wider set of ISO-4217 fiat codes that Reflector-operator-grade
// FX feeds publish. Crypto tickers (BTC, ETH, SOL …) emitted by the
// CEX feed are NOT on this list — they represent a different asset
// class that needs its own canonical type (tracked separately,
// outside PR 164a's scope).
//
// 2026-08-27: widened to the FULL set the active massive.com FX feed
// (internal/sources/external/forex) actually publishes into fx_quotes —
// 132 codes observed on r1. The batch price endpoint rejects the whole
// request on the first code it can't parse, so the /assets converter
// could only ever offer the intersection of this list and the feed;
// aligning them lets the converter surface every currency the feed
// carries (~109 with live daily rates; the remainder — incl. legacy
// pre-euro/redenominated codes like CYP/EEK/LTL/LVL/MTL/ROL/SIT/SKK/TRL
// — are inert without a fresh rate and simply never resolve to a price).
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
	"UZS": {}, "VND": {}, "XPF": {}, "YER": {}, "ZAR": {}, "ZMW": {},
}

// IsKnownFiat reports whether code is in the ADR-0010 allow-list.
// Callers use this to validate operator-supplied fiat configuration
// before constructing an [Asset] at startup.
func IsKnownFiat(code string) bool {
	_, ok := knownFiatCodes[code]
	return ok
}

// NewFiatAsset constructs a fiat asset. Returns ErrInvalidAsset if
// the code isn't allow-listed.
func NewFiatAsset(code string) (Asset, error) {
	if !IsKnownFiat(code) {
		return Asset{}, errorf(ErrInvalidAsset, "unknown fiat code %q (see ADR-0010)", code)
	}
	return Asset{Type: AssetFiat, Code: code}, nil
}
