package redstone

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
)

// feedEntry is one feed-registry row: the canonical (base, quote) a feed_id prices, and its orientation.
type feedEntry struct {
	Base  canonical.Asset
	Quote canonical.Asset

	// Invert marks feeds published in market-FX convention (units-per-USD): the decoder reciprocates
	// exactly so the row reads "<Base> in USD", keeping the lossy on-chain integer as PublishedPrice. Only
	// MXNe today (~17.4 → ~0.0575, matching reflector-fx MXN). Orientation only; a depeg still shows (ADR-0028).
	Invert bool
}

// quoteUSD / quoteEUR are the registry's quote currencies: USD unless the feed_id carries a `/<QUOTE>`
// suffix (only EUROC/EUR differs); NAV feeds use quoteBTC / quoteSolvBTC. See ADR-0028 §Decision.
var (
	quoteUSD = mustFiat("USD")
	quoteEUR = mustFiat("EUR")
)

// quoteBTC / quoteSolvBTC denominate the SolvBTC `_FUNDAMENTAL` NAV ratios, whose reserve is crypto.
// Quoting them `fiat:USD` would serve "a BTC-backed token is worth $1.00" with mapped=true, beside its
// own `_FUNDAMENTAL/USD` sibling at $78,313.
var (
	quoteBTC     = mustCrypto("BTC")
	quoteSolvBTC = mustCrypto("SolvBTC")
)

// earnUSDCVaultContract is the Gami earnUSDC tokenized vault (the contract IS the share token),
// verified in the lake: 115 deposit / 35 withdraw / 8 transfer events from ledger 62938336, while the other
// circulated address has none. An alias of upshift's constant so the price and the decoded activity
// cannot drift onto different asset ids.
const earnUSDCVaultContract = upshift.MainnetVaultEarnUSDC

// feedRegistry maps each EXACT on-chain feed_id() to its canonical (base, quote) — the 32 RedStone
// Stellar mainnet feeds (TestFeedRegistry_CountMatchesItsDocComment). No two feed_ids share a pair
// (TestFeedRegistry_UniquePairs): one batch would interleave two quantities into one series.
//
// The key is the write_prices feed_id, not the display name (EUROC/EUR, BENJI_ETHEREUM_FUNDAMENTAL), and
// the quote is per-feed, so a ticker allow-list like canonical.IsKnownCrypto would drop 5 feeds.
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

	// earnUSDC — an Upshift vault share, keyed on the vault contract, NOT USDC: collapsing a
	// yield-bearing claim into USDC is the asset-identity error class.
	//
	// REPORTED AS PUBLISHED: RedStone prices it ~0.72 against a ~1.01 share price, confirmed with them as
	// their figure; BTC and EUROC on the same contract decode right. Do not "correct" this entry.
	"earnUSDC_FUNDAMENTAL": {Base: mustSoroban(earnUSDCVaultContract), Quote: quoteUSD},
	// MXNe is published units-per-USD (USDMXN market convention);
	// Invert reciprocates it to MXNe-in-USD. See feedEntry.Invert.
	"MXNe": {Base: mustCrypto("MXNe"), Quote: quoteUSD, Invert: true},

	// Tokenized-BTC feeds (crypto). `SolvBTC` is a dollar price; the bare `_FUNDAMENTAL` feeds are NAV
	// RATIOS in their reserve asset, derived from live rows where both `_FUNDAMENTAL/USD` legs read 78313.03:
	//
	//   SolvBTC_FUNDAMENTAL     1.00295305 = 78313.03 / 78082.5 (BTC) ⇒ denominated in BTC.
	//   SolvBTC.BBN_FUNDAMENTAL 1.00000000 on three captures, with NAV_USD equal to SolvBTC's
	//     ⇒ denominated in SolvBTC; quoting BTC would contradict its own _USD row.
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

	// ── Relayer expansion (ledger 63624934) ───────────────────────
	// Orientation and magnitude verified live against api.redstone.finance with CoinGecko cross-checks;
	// none needs Invert.

	// Bare EUROC is USD-quoted (live 1.1398 ≈ EUR/USD), a separate series from `EUROC/EUR` (1.0003).
	"EUROC": {Base: mustCrypto("EUROC"), Quote: quoteUSD},

	// Ethena synthetic dollars (crypto, not RWA — ADR-0014); sUSDe is the accruing staked form.
	"USDe":  {Base: mustCrypto("USDe"), Quote: quoteUSD},
	"sUSDe": {Base: mustCrypto("sUSDe"), Quote: quoteUSD},

	// Avant Protocol staked USD — crypto-native yield vault, same
	// class as sUSDe (NOT ADR-0028 rwa). Live 1.1877, accruing.
	"savUSD_FUNDAMENTAL": {Base: mustCrypto("savUSD_FUNDAMENTAL"), Quote: quoteUSD},

	// USD-quoted SolvBTC NAV feeds: NAV in USD (live 65,430), a different quantity from the unsuffixed
	// ratio feeds, so distinct base codes (`/` → `_`, see canonical.knownCryptoCodes) and quotes.
	"SolvBTC_FUNDAMENTAL/USD":     {Base: mustCrypto("SolvBTC_FUNDAMENTAL_USD"), Quote: quoteUSD},
	"SolvBTC.BBN_FUNDAMENTAL/USD": {Base: mustCrypto("SolvBTC.BBN_FUNDAMENTAL_USD"), Quote: quoteUSD},

	// Tokenized RWAs (ADR-0028). RWA codes strip the feed-id suffix
	// per the BENJI precedent.
	"USDY_FUNDAMENTAL/USD":    {Base: mustRWA("USDY"), Quote: quoteUSD},    // Ondo USDY: live 1.1408 (CG 1.14, accruing note)
	"USST_FUNDAMENTAL":        {Base: mustRWA("USST"), Quote: quoteUSD},    // STBL USST: live 1.0096
	"XAUm_FUNDAMENTAL/USD":    {Base: mustRWA("XAUm"), Quote: quoteUSD},    // Matrixdock gold: live 4115.67/oz (CG pax-gold 4088)
	"deJAAA_FUNDAMENTAL/USD":  {Base: mustRWA("deJAAA"), Quote: quoteUSD},  // deRWA JAAA: live 1.0404
	"deJTRSY_FUNDAMENTAL/USD": {Base: mustRWA("deJTRSY"), Quote: quoteUSD}, // deRWA JTRSY: live 1.0315
}

// lookupFeed resolves a feed_id to its registry entry; ok is false off-registry (then recorded raw).
func lookupFeed(feedID string) (entry feedEntry, ok bool) {
	entry, ok = feedRegistry[feedID]
	return entry, ok
}

// reciprocalAtScale returns 1/value for a fixed-point Amount at the same scale, in exact big.Int
// arithmetic (ADR-0003): 10^(2d) / r, rounded half-up. The caller guarantees r > 0.
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

// mustSoroban / mustCrypto / mustRWA / mustFiat build registry assets from compile-time constants;
// an error is a typo, so panic at init rather than degrade silently.
func mustSoroban(contractID string) canonical.Asset {
	a, err := canonical.NewSorobanAsset(contractID)
	if err != nil {
		panic("redstone: feed registry: " + err.Error())
	}
	return a
}

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
