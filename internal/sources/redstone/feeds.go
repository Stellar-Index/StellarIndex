package redstone

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// feedEntry is one row of the RedStone feed registry: the canonical
// (base, quote) pair a feed_id prices, plus whether the on-chain
// value is published in the inverse (market-FX) orientation.
type feedEntry struct {
	Base  canonical.Asset
	Quote canonical.Asset

	// Invert is true for feeds RedStone publishes in market-FX
	// convention (units-per-USD, e.g. USDMXN ≈ 17.4 pesos/USD) rather
	// than our canonical "<Base> in <Quote>" convention. The decoder
	// reciprocates the raw value (1/x, exact big.Int arithmetic) so
	// the stored row reads "<Base> in USD" like every other feed.
	//
	// Only MXNe needs this today: RedStone emits ~17.4 (pesos/USD)
	// while every other currency/RWA feed — including the Mexican
	// CETES bond (~$0.067) and the EUR-pegged EUROB (~$1.17) — is
	// already emitted as value-in-USD. Post-invert MXNe reads
	// ~0.0575, matching reflector-fx MXN (~0.0573). Verified against
	// live r1 rows 2026-07-07. See docs/adr/0028 + the reflector
	// stablecoin-proxy note in CLAUDE.md (normalise orientation here,
	// NOT the asset identity — a MXNe depeg still shows through 1/x).
	Invert bool
}

// quoteUSD / quoteEUR are the two quote currencies the registry
// uses. RedStone publishes USD-denominated prices unless the feed_id
// carries an explicit `/<QUOTE>` suffix — only EUROC/EUR does today.
// See ADR-0028 §The RedStone 19-feed registry.
var (
	quoteUSD = mustFiat("USD")
	quoteEUR = mustFiat("EUR")
)

// feedRegistry maps each EXACT on-chain feed_id() string to the
// canonical (base, quote) pair it prices — the 19 RedStone Stellar
// mainnet feeds, captured on-chain 2026-05-22 (#53; see ADR-0028).
//
// The key is the string the relayer passes in
// write_prices(updater, feed_ids, payload) — which is NOT always the
// display name. EUROC's feed_id is `EUROC/EUR`; BENJI's is
// `BENJI_ETHEREUM_FUNDAMENTAL`. Matching a plain-ticker allow-list
// against these silently dropped 5 feeds (the pre-#53 bug — EUROC
// among them never decoded).
//
// Pre-#53 this was `canonical.IsKnownCrypto(feedID)`; an explicit
// registry is required because (a) feed_id ≠ ticker for 5 feeds and
// (b) the quote currency is per-feed, not a global USD assumption.
var feedRegistry = map[string]feedEntry{
	// Crypto / stablecoin feeds.
	"BTC":       {Base: mustCrypto("BTC"), Quote: quoteUSD},
	"ETH":       {Base: mustCrypto("ETH"), Quote: quoteUSD},
	"USDC":      {Base: mustCrypto("USDC"), Quote: quoteUSD},
	"XLM":       {Base: mustCrypto("XLM"), Quote: quoteUSD},
	"PYUSD":     {Base: mustCrypto("PYUSD"), Quote: quoteUSD},
	"EUROC/EUR": {Base: mustCrypto("EUROC"), Quote: quoteEUR}, // EUR-denominated — note the suffix
	"EUROB":     {Base: mustCrypto("EUROB"), Quote: quoteUSD},
	// MXNe is published units-per-USD (USDMXN market convention);
	// Invert reciprocates it to MXNe-in-USD. See feedEntry.Invert.
	"MXNe": {Base: mustCrypto("MXNe"), Quote: quoteUSD, Invert: true},

	// Tokenized-BTC feeds — BTC-backed crypto tokens (crypto, not rwa).
	"SolvBTC":                 {Base: mustCrypto("SolvBTC"), Quote: quoteUSD},
	"SolvBTC_FUNDAMENTAL":     {Base: mustCrypto("SolvBTC_FUNDAMENTAL"), Quote: quoteUSD},
	"SolvBTC.BBN_FUNDAMENTAL": {Base: mustCrypto("SolvBTC.BBN_FUNDAMENTAL"), Quote: quoteUSD},

	// Tokenized real-world assets — ADR-0028 `rwa` AssetType.
	"BENJI_ETHEREUM_FUNDAMENTAL":  {Base: mustRWA("BENJI"), Quote: quoteUSD},
	"iBENJI_ETHEREUM_FUNDAMENTAL": {Base: mustRWA("iBENJI"), Quote: quoteUSD},
	"GILTS":                       {Base: mustRWA("GILTS"), Quote: quoteUSD},
	"CETES":                       {Base: mustRWA("CETES"), Quote: quoteUSD},
	"KTB":                         {Base: mustRWA("KTB"), Quote: quoteUSD},
	"TESOURO":                     {Base: mustRWA("TESOURO"), Quote: quoteUSD},
	"USTRY":                       {Base: mustRWA("USTRY"), Quote: quoteUSD},
	"SPXU":                        {Base: mustRWA("SPXU"), Quote: quoteUSD},
}

// lookupFeed resolves a feed_id to its registry entry. ok is false
// for a feed_id outside the registry — RedStone deploying a 20th
// feed surfaces here; the decoder skips + counts it, the same
// graceful per-feed skip as the pre-#53 unknown path.
func lookupFeed(feedID string) (entry feedEntry, ok bool) {
	entry, ok = feedRegistry[feedID]
	return entry, ok
}

// reciprocalAtScale returns 1/value for a fixed-point Amount at the
// given decimal scale, computed in exact big.Int arithmetic
// (ADR-0003 — never via float or int64 truncation). A raw integer r
// encodes value = r / 10^d; its reciprocal at the SAME scale d is
// 10^(2d) / r, rounded half-up. Used for [feedEntry.Invert] feeds
// (e.g. MXNe: r≈17.4·10^8 pesos/USD → ≈0.0575·10^8 USD/MXNe). The
// caller guarantees r > 0 (the decoder skips non-positive prices
// before inverting), so there is no divide-by-zero.
func reciprocalAtScale(a canonical.Amount, decimals uint8) canonical.Amount {
	r := a.BigInt()
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	num := new(big.Int).Mul(scale, scale) // 10^(2d)
	// round half-up = floor((2*num + r) / (2*r)) for r > 0.
	twoNum := new(big.Int).Lsh(num, 1)
	twoNum.Add(twoNum, r)
	twoR := new(big.Int).Lsh(r, 1)
	return canonical.NewAmount(twoNum.Quo(twoNum, twoR))
}

// mustCrypto / mustRWA / mustFiat build a canonical reference asset
// for the registry. The codes are compile-time constants vetted
// against the ADR-0014 / ADR-0028 allow-lists — an error means a
// typo in this file, so panic at init rather than degrade silently.
func mustCrypto(code string) canonical.Asset {
	a, err := canonical.NewCryptoAsset(code)
	if err != nil {
		panic("redstone: feed registry: " + err.Error())
	}
	return a
}

func mustRWA(code string) canonical.Asset {
	a, err := canonical.NewRWAAsset(code)
	if err != nil {
		panic("redstone: feed registry: " + err.Error())
	}
	return a
}

func mustFiat(code string) canonical.Asset {
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		panic("redstone: feed registry: " + err.Error())
	}
	return a
}
