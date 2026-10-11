package canonical

// Off-chain crypto-ticker asset helpers — see ADR-0014.
//
// AssetCrypto carries off-chain crypto reference tickers (BTC, ETH, USDT, …), not Stellar
// assets: oracle prices (notably Reflector's CEX oracle) quote against a global asset concept,
// not an on-chain token. Wire form `crypto:<TICKER>`.
//
// `USDC:GA5ZSEJY…` (Circle's on-chain USDC) and `crypto:USDC` (the global concept) are
// intentionally NOT Equal().

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
	// USDT0 (omnichain USDT) is a DISTINCT asset from USDT: its own issuance and peg risk, and
	// merging them would be the eager normalisation the stablecoin rule rejects. Any fiat:USD
	// proxying is decided in internal/aggregate/stablecoin.go.
	"USDT0": {},
	// Euro-pegged stablecoins stay crypto; the aggregator maps them to fiat:EUR. EURC and EUROC
	// are one asset (Circle's rename, see eurcAliasFamily); EUROB is a different token.
	"EURC": {}, "EUROC": {}, "EUROB": {},
	// Mexican Peso stablecoin (Bitso MXNe); the aggregator maps it to fiat:MXN.
	"MXNe": {},
	// Tokenized-BTC variants. SolvBTC is BTC-backed crypto, not RWA (ADR-0028 reserves `rwa`
	// for tokenized tradfi). `_FUNDAMENTAL` feeds publish NAV; each feed_id is its own code so
	// market and NAV observations never collide. These two NAV feeds are quoted in their
	// RESERVE asset (crypto:BTC and crypto:SolvBTC), not USD; see redstone.feedRegistry.
	"SolvBTC": {}, "SolvBTC_FUNDAMENTAL": {}, "SolvBTC.BBN_FUNDAMENTAL": {},
	// Ethena's synthetic-dollar tokens are crypto-native (delta-neutral basis), not ADR-0028
	// rwa. sUSDe is the staked, value-accruing form.
	"USDe": {}, "sUSDe": {},
	// Avant Protocol staked USD: a crypto-native yield vault like sUSDe, not rwa. The code
	// keeps the full feed_id.
	"savUSD_FUNDAMENTAL": {},
	// USD-quoted SolvBTC NAV feeds publish the NAV in USD, a DIFFERENT quantity from the
	// unsuffixed `_FUNDAMENTAL` feeds (NAV ratio against the reserve asset), so they get
	// distinct codes. The feed_id's `/` becomes `_` because codes travel as URL path segments
	// (`/v1/assets/{id}`).
	"SolvBTC_FUNDAMENTAL_USD": {}, "SolvBTC.BBN_FUNDAMENTAL_USD": {},
}

// IsKnownCrypto reports whether code is in the ADR-0014 allow-list.
// Callers use this to filter Reflector CEX-oracle symbols on the
// decoder hot path — unknown tickers are skipped rather than
// silently coerced.
func IsKnownCrypto(code string) bool {
	_, ok := knownCryptoCodes[code]
	return ok
}

// NewCryptoAsset constructs a crypto-ticker asset. Returns
// ErrInvalidAsset if the code isn't allow-listed.
func NewCryptoAsset(code string) (Asset, error) {
	if !IsKnownCrypto(code) {
		return Asset{}, errorf(ErrInvalidAsset, "unknown crypto code %q (see ADR-0014)", code)
	}
	return Asset{Type: AssetCrypto, Code: code}, nil
}
