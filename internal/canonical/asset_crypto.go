package canonical

// Off-chain crypto-ticker asset helpers — see ADR-0014.
//
// AssetCrypto is a global reference ticker (wire form `crypto:<TICKER>`), not a Stellar asset.
// `USDC:GA5ZSEJY…` (Circle's on-chain USDC) and `crypto:USDC` are intentionally NOT Equal().

// knownCryptoCodes is the allow-list of recognized crypto tickers: observed Reflector CEX
// oracle traffic plus the largest-cap global assets.
var knownCryptoCodes = map[string]struct{}{
	"ADA": {}, "ATOM": {}, "AVAX": {}, "BCH": {}, "BNB": {},
	"BTC": {}, "DASH": {}, "DOGE": {}, "DOT": {}, "ETH": {},
	"LINK": {}, "LTC": {}, "MATIC": {}, "NEAR": {}, "SHIB": {},
	"SOL": {}, "TON": {}, "TRX": {}, "UNI": {}, "USDC": {},
	"USDT": {}, "XLM": {}, "XRP": {},
	// Stablecoins and fiat-pegged tokens stay crypto, not fiat: the aggregator converts them at
	// VWAP time (stablecoin-as-fiat is aggregator policy), so the decoder stays proxy-agnostic.
	"DAI": {}, "PYUSD": {}, "USDP": {},
	// USDT0 is DISTINCT from USDT (own issuance and peg risk); merging them would be eager
	// normalisation. Fiat proxying is decided in internal/aggregate/stablecoin.go.
	"USDT0": {},
	// Euro-pegged stablecoins stay crypto; the aggregator maps them to fiat:EUR. EURC and EUROC
	// are one asset (Circle's rename, see eurcAliasFamily); EUROB is a different token.
	"EURC": {}, "EUROC": {}, "EUROB": {},
	// Mexican Peso stablecoin (Bitso MXNe); the aggregator maps it to fiat:MXN.
	"MXNe": {},
	// Tokenized-BTC variants: BTC-backed crypto, not RWA (ADR-0028). Each `_FUNDAMENTAL` NAV
	// feed_id is its own code so market and NAV observations never collide; these two are quoted
	// in their RESERVE asset, not USD (see redstone.feedRegistry).
	"SolvBTC": {}, "SolvBTC_FUNDAMENTAL": {}, "SolvBTC.BBN_FUNDAMENTAL": {},
	// Ethena's synthetic-dollar tokens are crypto-native (delta-neutral basis), not ADR-0028
	// rwa. sUSDe is the staked, value-accruing form.
	"USDe": {}, "sUSDe": {},
	// Avant Protocol staked USD: a crypto-native yield vault like sUSDe, not rwa. The code
	// keeps the full feed_id.
	"savUSD_FUNDAMENTAL": {},
	// USD-quoted NAV feeds are a DIFFERENT quantity from the unsuffixed ones (a ratio against the
	// reserve asset), so they get distinct codes. The feed_id's `/` becomes `_` because codes
	// travel as URL path segments.
	"SolvBTC_FUNDAMENTAL_USD": {}, "SolvBTC.BBN_FUNDAMENTAL_USD": {},
}

// IsKnownCrypto reports whether code is in the ADR-0014 allow-list; unknown tickers are skipped, never coerced.
func IsKnownCrypto(code string) bool {
	_, ok := knownCryptoCodes[code]
	return ok
}

// NewCryptoAsset constructs a crypto-ticker asset, returning ErrInvalidAsset if the code isn't allow-listed.
func NewCryptoAsset(code string) (Asset, error) {
	if !IsKnownCrypto(code) {
		return Asset{}, errorf(ErrInvalidAsset, "unknown crypto code %q (see ADR-0014)", code)
	}
	return Asset{Type: AssetCrypto, Code: code}, nil
}
