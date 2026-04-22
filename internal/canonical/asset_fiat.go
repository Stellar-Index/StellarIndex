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
// Codes chosen from ISO-4217 plus currencies the RFPs explicitly
// name or that our CEX/FX connectors will price against.
var knownFiatCodes = map[string]struct{}{
	"AUD": {}, "BRL": {}, "CAD": {}, "CHF": {}, "CNY": {},
	"EUR": {}, "GBP": {}, "HKD": {}, "INR": {}, "JPY": {},
	"KRW": {}, "MXN": {}, "NGN": {}, "NZD": {}, "RUB": {},
	"SGD": {}, "TRY": {}, "USD": {}, "ZAR": {},
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
