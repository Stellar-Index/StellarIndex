package canonical

// Off-chain crypto-ticker asset helpers — see ADR-0014.
//
// The Asset type carries an AssetCrypto variant for off-chain crypto
// reference tickers (BTC, ETH, USDT, …). These are NOT Stellar assets
// — they're abstract ticker references used by oracle prices (notably
// Reflector's CEX oracle) where the oracle quotes "BTC/USD" against
// a global crypto asset concept rather than any specific on-chain
// token.
//
// Wire form: `crypto:<TICKER>` (e.g. `crypto:BTC`). The `crypto:`
// prefix is unambiguous, so ParseAsset dispatches in O(1).
//
// Distinction from classic assets: `USDC:GA5ZSEJY…` is Circle's
// Stellar-classic USDC (a specific on-chain token), whereas
// `crypto:USDC` is the global USDC concept. They are intentionally
// NOT Equal() under canonical.Asset.

// knownCryptoCodes is the allow-list of recognized crypto tickers.
// Extension is a one-line amendment to ADR-0014 (never a superseding
// ADR). Codes chosen from mainnet Reflector CEX oracle traffic
// observed 2026-04-23 plus the largest-cap global crypto assets that
// are likely to appear.
var knownCryptoCodes = map[string]struct{}{
	"ADA": {}, "ATOM": {}, "AVAX": {}, "BCH": {}, "BNB": {},
	"BTC": {}, "DASH": {}, "DOGE": {}, "DOT": {}, "ETH": {},
	"LINK": {}, "LTC": {}, "MATIC": {}, "NEAR": {}, "SHIB": {},
	"SOL": {}, "TON": {}, "TRX": {}, "UNI": {}, "USDC": {},
	"USDT": {}, "XLM": {}, "XRP": {},
	// Stablecoins + fiat-pegged crypto tokens published by RedStone's
	// Stellar adapter (2026-04-24). Kept here as crypto (not fiat) so
	// the decoder stays fiat-proxy-agnostic — the aggregator converts
	// stablecoins → fiat at VWAP time per the "stablecoin-as-fiat is
	// aggregator policy" rule in CLAUDE.md.
	"DAI": {}, "PYUSD": {}, "USDP": {},
	// USDT0 — the omnichain USDT representation, published by RedStone's
	// Stellar adapter. A DISTINCT asset from `USDT`, deliberately: it is
	// a different token with its own issuance and its own peg risk, and
	// collapsing the two here would be exactly the eager normalisation
	// the stablecoin rule above rejects. Whether it should ALSO proxy to
	// fiat:USD at VWAP time is an aggregator-policy decision, taken in
	// internal/aggregate/stablecoin.go, not here (2026-08-31: recorded
	// as crypto only; it was arriving as `raw:USDT0` and ticketing
	// stellarindex_ingestion_oracle_unknown_symbols).
	"USDT0": {},
	// Euro-pegged stablecoins. Same reasoning — keep as crypto here,
	// let the aggregator decide to map them to fiat:EUR.
	"EURC": {}, "EUROC": {}, "EUROB": {},
	// Mexican Peso stablecoin (Bitso MXNe). Aggregator maps to fiat:MXN.
	"MXNe": {},
	// Tokenized-BTC variants published by RedStone's Stellar feeds
	// (2026-05-22, #53). SolvBTC is a BTC-backed crypto token — crypto,
	// not RWA (ADR-0028 reserves `rwa` for tokenized tradfi assets).
	// `_FUNDAMENTAL` feeds publish NAV; each feed_id is its own code so
	// market and NAV observations never collide on one asset. Those
	// two NAV feeds are quoted in their RESERVE asset, not USD —
	// crypto:BTC and crypto:SolvBTC respectively (D8, 2026-08-29); see
	// redstone.feedRegistry for the live derivation.
	"SolvBTC": {}, "SolvBTC_FUNDAMENTAL": {}, "SolvBTC.BBN_FUNDAMENTAL": {},
	// 2026-07-24 RedStone relayer expansion (ledger 63624934; ADR-0014
	// Amendments). Ethena's synthetic-dollar tokens — crypto-native
	// (delta-neutral basis strategies), not ADR-0028 rwa; USDe stays
	// crypto like USDT/USDC per the stablecoin-as-fiat-is-aggregator-
	// policy rule above. sUSDe is the staked, value-accruing form.
	"USDe": {}, "sUSDe": {},
	// Avant Protocol staked USD (on-chain feed_id `savUSD_FUNDAMENTAL`).
	// Crypto-native yield vault over delta-neutral strategies — the
	// same class as sUSDe, NOT ADR-0028 rwa (reserved for tokenized
	// tradfi assets). Code keeps the full feed_id per the
	// each-feed_id-is-its-own-code convention above.
	"savUSD_FUNDAMENTAL": {},
	// USD-quoted SolvBTC NAV feeds (on-chain feed_ids
	// `SolvBTC_FUNDAMENTAL/USD`, `SolvBTC.BBN_FUNDAMENTAL/USD`). These
	// publish the NAV **in USD** (~65,430 on 2026-07-27) — a DIFFERENT
	// quantity from the unsuffixed `_FUNDAMENTAL` feeds above, which
	// publish the NAV RATIO against their reserve asset (~1.003 vs BTC
	// and 1.0000 vs SolvBTC; verified live 2026-07-27 against
	// api.redstone.finance AND r1 oracle_updates).
	// Distinct codes so the two series never collide. The feed_id's
	// `/` is normalized to `_` here because canonical codes travel as
	// URL path segments (`/v1/assets/{id}`) where a literal `/` would
	// split the segment.
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
