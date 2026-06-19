package canonical

import "strings"

// nativeSAC is the Stellar Asset Contract address that wraps native
// XLM (the Soroban-side alias for `native`). Both forms rank as XLM
// for orientation. Kept in sync with the SAC literal used across the
// storage layer's XLM/USD CTEs.
const nativeSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

// StablecoinCodes are the asset CODES treated as fiat-pegged
// stablecoins for canonical pair ORIENTATION — i.e. deciding which
// side of a market is the quote. It mirrors the KEYS of the
// aggregator's stablecoinFiatProxy map (internal/aggregate/stablecoin.go),
// which is the pricing-policy source of truth; this copy lives in the
// leaf `canonical` package so the storage layer can orient pairs
// without importing the aggregator (which would be a cycle). A test
// pins the two in sync.
var StablecoinCodes = map[string]bool{
	"USDT": true, "USDC": true, "DAI": true, "PYUSD": true, "USDP": true,
	"EURC": true, "EUROC": true, "EUROB": true, "MXNe": true,
}

// assetCode extracts the asset CODE from a canonical asset_id for
// stablecoin matching: classic `CODE-ISSUER` → `CODE`; CEX `crypto:CODE`
// → `CODE`; everything else unchanged.
func assetCode(assetID string) string {
	code := assetID
	if i := strings.IndexByte(code, '-'); i > 0 {
		code = code[:i]
	}
	return strings.TrimPrefix(code, "crypto:")
}

// quoteRank scores how "quote-like" (numeraire-like) an asset is.
// Higher = more likely to be the quote in a canonical pair:
// fiat (4) > stablecoin (3) > XLM (2) > any other token (1). This is
// what makes XLM/USDC orient as base=XLM, quote=USDC (price in USDC),
// while XLM/AQUA orients as base=AQUA, quote=XLM (AQUA priced in XLM).
func quoteRank(assetID string) int {
	if strings.HasPrefix(assetID, "fiat:") {
		return 4
	}
	if StablecoinCodes[assetCode(assetID)] {
		return 3
	}
	if assetID == "native" || assetID == nativeSAC {
		return 2
	}
	return 1
}

// Orient returns the canonical (base, quote) orientation of the market
// formed by asset_ids a and b, plus whether the INPUT order (a=base,
// b=quote) is flipped relative to canonical. Feeding the same market in
// either order yields the same (base, quote); `flipped` tells a caller
// holding a row stored as a/b whether to invert that row's price
// (canonical price = 1 / stored price) and swap its per-leg amounts
// before combining the two directions.
//
// The canonical quote is the higher-quoteRank asset; ties break by
// string order (the lexicographically greater asset_id is the quote)
// purely for determinism.
func Orient(a, b string) (base, quote string, flipped bool) {
	ra, rb := quoteRank(a), quoteRank(b)
	bIsQuote := rb > ra || (rb == ra && b > a)
	if bIsQuote {
		return a, b, false // a/b is already canonical
	}
	return b, a, true // canonical is b/a; the input a/b is flipped
}

// Canonical returns p re-oriented to its canonical (base, quote) form,
// and whether p was flipped to get there (so the caller can invert a
// derived price). XLM/USDC and USDC/XLM both return the same canonical
// pair.
func (p Pair) Canonical() (Pair, bool) {
	_, _, flipped := Orient(p.Base.String(), p.Quote.String())
	if !flipped {
		return p, false
	}
	return Pair{Base: p.Quote, Quote: p.Base}, true
}
