package canonical

import "sort"

// Off-chain fiat asset helpers — see ADR-0010.
//
// AssetFiat carries off-chain fiat currencies (USD, EUR, …) used by oracle prices and FX feeds;
// they are not Stellar assets. Wire form `fiat:<ISO4217>`.

// knownFiatCodes is the allow-list of 3-letter fiat codes: Reflector FX oracle traffic, wider ISO-4217, and the FULL set the
// massive.com FX feed publishes into fx_quotes. The batch price endpoint rejects the whole request on the first unparseable code,
// so the /assets converter can only offer the intersection with the feed. Crypto tickers are NOT here (see asset_crypto.go).
// VES is listed only so the reflector-fx row types as fiat:VES. XAU (gold) stays OFF: it is a commodity, rwa:XAU (ADR-0028).
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
func IsKnownFiat(code string) bool {
	_, ok := knownFiatCodes[code]
	return ok
}

// KnownFiatCodes returns the ADR-0010 allow-list, sorted, as a copy.
func KnownFiatCodes() []string {
	out := make([]string, 0, len(knownFiatCodes))
	for code := range knownFiatCodes {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// NewFiatAsset constructs a fiat asset, returning ErrInvalidAsset if the code isn't allow-listed.
func NewFiatAsset(code string) (Asset, error) {
	if !IsKnownFiat(code) {
		return Asset{}, errorf(ErrInvalidAsset, "unknown fiat code %q (see ADR-0010)", code)
	}
	return Asset{Type: AssetFiat, Code: code}, nil
}
