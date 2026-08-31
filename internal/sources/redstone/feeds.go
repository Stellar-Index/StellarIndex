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

// quoteUSD / quoteEUR are the two quote CURRENCIES the registry
// uses. RedStone publishes USD-denominated MARKET prices unless the
// feed_id carries an explicit `/<QUOTE>` suffix — EUROC/EUR is the
// only non-USD suffix; the 2026-07-24 feeds carry explicit `/USD`
// suffixes that simply restate the default. NAV feeds are the
// exception to the default — see quoteBTC / quoteSolvBTC below.
// See ADR-0028 §The RedStone 19-feed registry.
var (
	quoteUSD = mustFiat("USD")
	quoteEUR = mustFiat("EUR")
)

// quoteBTC / quoteSolvBTC are the non-currency denominators. A bare
// `_FUNDAMENTAL` feed publishes net asset value in the token's
// RESERVE asset, which is only a currency when the reserve is cash or
// T-bills (BENJI, USST, savUSD — see the attestation list in
// feeds_test.go). For the SolvBTC family the reserve is crypto, so
// the published number is a RATIO and the quote must name the asset
// the ratio is denominated in. Registering those two feeds as
// `fiat:USD` was D8: it served "a BTC-backed token is worth $1.00"
// on /v1/oracle/streams with mapped=true, for a token its own
// `_FUNDAMENTAL/USD` sibling priced at $78,313.
var (
	quoteBTC     = mustCrypto("BTC")
	quoteSolvBTC = mustCrypto("SolvBTC")
)

// feedRegistry maps each EXACT on-chain feed_id() string to the
// canonical (base, quote) pair it prices — the 30 RedStone Stellar
// mainnet feeds: 19 captured on-chain 2026-05-22 (#53; see
// ADR-0028) plus 11 from the 2026-07-24 relayer expansion (ledger
// 63624934 — unknown ids were skipped fail-closed, ~5,600 events
// dropped, until the expansion block below landed).
//
// Invariant (pinned by TestFeedRegistry_UniquePairs): no two
// feed_ids map to the same (Base, Quote) pair — feeds arrive
// together in one batch, so a shared pair would interleave two
// different quantities into one price series.
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
	"USDT0":     {Base: mustCrypto("USDT0"), Quote: quoteUSD},
	"XLM":       {Base: mustCrypto("XLM"), Quote: quoteUSD},
	"PYUSD":     {Base: mustCrypto("PYUSD"), Quote: quoteUSD},
	"EUROC/EUR": {Base: mustCrypto("EUROC"), Quote: quoteEUR}, // EUR-denominated — note the suffix
	"EUROB":     {Base: mustCrypto("EUROB"), Quote: quoteUSD},
	// MXNe is published units-per-USD (USDMXN market convention);
	// Invert reciprocates it to MXNe-in-USD. See feedEntry.Invert.
	"MXNe": {Base: mustCrypto("MXNe"), Quote: quoteUSD, Invert: true},

	// Tokenized-BTC feeds — BTC-backed crypto tokens (crypto, not rwa).
	// `SolvBTC` is the market price in dollars; the two bare
	// `_FUNDAMENTAL` feeds are NAV RATIOS, denominated in the reserve
	// each token is a claim on — NOT in USD (D8, fixed 2026-08-29;
	// the `/USD` siblings further down carry the dollar figures).
	//
	// Denominators derived from the live r1 rows
	// (/v1/oracle/streams?include_unmapped=true, 2026-08-29), where the
	// two `_FUNDAMENTAL/USD` legs are byte-identical — 78313.02974310
	// each, as they were on 2026-07-27 (6543063913439 each):
	//
	//   SolvBTC_FUNDAMENTAL     1.00295305 = NAV_USD / BTC_USD
	//     (78313.03 / 78082.5) ⇒ denominated in BTC.
	//   SolvBTC.BBN_FUNDAMENTAL 1.00000000 exactly, on three
	//     independent captures (lake ledger 60104689, 2026-07-27,
	//     2026-08-29), while its NAV_USD equals SolvBTC's NAV_USD
	//     ⇒ SolvBTC.BBN is 1:1 with SolvBTC and the ratio is
	//     denominated in SolvBTC. Quoting it BTC would contradict our
	//     own SolvBTC.BBN_FUNDAMENTAL_USD row by the SolvBTC premium.
	//
	// The base codes are unchanged (each feed_id keeps its own code
	// per ADR-0028 §3) — only the mislabelled denominator moves.
	"SolvBTC":                 {Base: mustCrypto("SolvBTC"), Quote: quoteUSD},
	"SolvBTC_FUNDAMENTAL":     {Base: mustCrypto("SolvBTC_FUNDAMENTAL"), Quote: quoteBTC},
	"SolvBTC.BBN_FUNDAMENTAL": {Base: mustCrypto("SolvBTC.BBN_FUNDAMENTAL"), Quote: quoteSolvBTC},

	// Tokenized real-world assets — ADR-0028 `rwa` AssetType.
	"BENJI_ETHEREUM_FUNDAMENTAL":  {Base: mustRWA("BENJI"), Quote: quoteUSD},
	"iBENJI_ETHEREUM_FUNDAMENTAL": {Base: mustRWA("iBENJI"), Quote: quoteUSD},
	"GILTS":                       {Base: mustRWA("GILTS"), Quote: quoteUSD},
	"CETES":                       {Base: mustRWA("CETES"), Quote: quoteUSD},
	"KTB":                         {Base: mustRWA("KTB"), Quote: quoteUSD},
	"TESOURO":                     {Base: mustRWA("TESOURO"), Quote: quoteUSD},
	"USTRY":                       {Base: mustRWA("USTRY"), Quote: quoteUSD},
	"SPXU":                        {Base: mustRWA("SPXU"), Quote: quoteUSD},

	// ── 2026-07-24 relayer expansion (ledger 63624934) ─────────────
	// Orientation + magnitude for every entry below verified live
	// 2026-07-27 against api.redstone.finance (?provider=redstone)
	// with CoinGecko cross-checks, plus r1 oracle_updates for the
	// pre-existing SolvBTC baselines. None needs Invert — all are
	// published token-in-quote like the rest of the registry.

	// Bare EUROC is USD-quoted: live 1.1398 ≈ EUR/USD (CG euro-coin
	// 1.14) — DISTINCT from the EUR-quoted `EUROC/EUR` feed above
	// (live 1.0003). Same base asset, different quote → two separate
	// price series; both can arrive in one batch without colliding.
	"EUROC": {Base: mustCrypto("EUROC"), Quote: quoteUSD},

	// Ethena synthetic dollars. USDe live 0.9998 (CG ethena-usde
	// 0.99957); sUSDe is the staked accruing form, live 1.2407 (CG
	// ethena-staked-usde 1.24). Crypto, not RWA — see ADR-0014.
	"USDe":  {Base: mustCrypto("USDe"), Quote: quoteUSD},
	"sUSDe": {Base: mustCrypto("sUSDe"), Quote: quoteUSD},

	// Avant Protocol staked USD — crypto-native yield vault, same
	// class as sUSDe (NOT ADR-0028 rwa). Live 1.1877, accruing.
	"savUSD_FUNDAMENTAL": {Base: mustCrypto("savUSD_FUNDAMENTAL"), Quote: quoteUSD},

	// USD-quoted SolvBTC NAV feeds. These publish NAV **in USD**
	// (live 65,430 vs RedStone BTC 65,234) — a DIFFERENT quantity
	// from the unsuffixed `_FUNDAMENTAL` feeds above, which publish
	// the NAV RATIO against their reserve asset (crypto:BTC /
	// crypto:SolvBTC; live 1.0029 and 1.0000, r1 oracle_updates
	// agrees). Hence distinct base codes, with `/` normalized to `_`
	// (see canonical.knownCryptoCodes for the URL-path rationale) AND
	// distinct quotes — the `/USD` suffix is what makes these two,
	// and only these two, dollar-denominated.
	"SolvBTC_FUNDAMENTAL/USD":     {Base: mustCrypto("SolvBTC_FUNDAMENTAL_USD"), Quote: quoteUSD},
	"SolvBTC.BBN_FUNDAMENTAL/USD": {Base: mustCrypto("SolvBTC.BBN_FUNDAMENTAL_USD"), Quote: quoteUSD},

	// Tokenized RWAs (ADR-0028 Amendments, 2026-07-27). RWA codes
	// strip the feed-id suffix per the BENJI precedent.
	"USDY_FUNDAMENTAL/USD":    {Base: mustRWA("USDY"), Quote: quoteUSD},    // Ondo USDY: live 1.1408 (CG 1.14, accruing note)
	"USST_FUNDAMENTAL":        {Base: mustRWA("USST"), Quote: quoteUSD},    // STBL USST: live 1.0096
	"XAUm_FUNDAMENTAL/USD":    {Base: mustRWA("XAUm"), Quote: quoteUSD},    // Matrixdock gold: live 4115.67/oz (CG pax-gold 4088)
	"deJAAA_FUNDAMENTAL/USD":  {Base: mustRWA("deJAAA"), Quote: quoteUSD},  // deRWA JAAA: live 1.0404
	"deJTRSY_FUNDAMENTAL/USD": {Base: mustRWA("deJTRSY"), Quote: quoteUSD}, // deRWA JTRSY: live 1.0315
}

// lookupFeed resolves a feed_id to its registry entry. ok is false
// for a feed_id outside the registry — RedStone deploying a feed
// beyond the registered set surfaces here (as the 2026-07-24
// expansion did); the decoder skips + counts it, the same graceful
// per-feed skip as the pre-#53 unknown path.
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
