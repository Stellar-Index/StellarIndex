package canonical

import (
	"sort"
	"strings"
)

// StablecoinCodes are the asset CODES treated as fiat-pegged stablecoins for pair ORIENTATION, mirroring the aggregator's stablecoinFiatProxy keys (a test pins them in sync).
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

// quoteRank scores how quote-like an asset is: fiat (4) > stablecoin (3) > XLM (2) > any other token (1). So XLM/USDC orients
// base=XLM, quote=USDC, while XLM/AQUA orients base=AQUA, quote=XLM.
//
// ISSUER-AGNOSTIC BY DESIGN: rank 3 keys on the bare CODE, so any classic token calling itself "USDC", scams included, ranks as a
// stablecoin. Safe ONLY because ranking decides orientation and never asserts a dollar value; any path mapping a rank-3 asset to a
// fiat value MUST do its own issuer check (aggregate.FiatProxy refuses classic assets). A SAC declared in `[supply].sac_wrappers`
// ranks as the classic asset it wraps; an undeclared C-address ranks 1.
func quoteRank(assetID string) int {
	if strings.HasPrefix(assetID, "fiat:") {
		return 4
	}
	if StablecoinCodes[assetCode(assetID)] || isStablecoinSACForm(assetID) {
		return 3
	}
	if isXLMQuoteForm(assetID) {
		return 2
	}
	return 1
}

// isXLMQuoteForm reports every canonical asset_id spelling of XLM
// (native, crypto:XLM, the installed network's SAC wrapper), so quoteRank
// ranks all three forms identically.
func isXLMQuoteForm(assetID string) bool {
	return assetID == "native" || assetID == "crypto:XLM" || assetID == NativeSACContractID()
}

// isStablecoinSACForm reports whether assetID is a Soroban form whose
// alias family leads with a stablecoin-coded asset in the active registry.
func isStablecoinSACForm(assetID string) bool {
	fam, ok := activeAliasRegistry().families[assetID]
	if !ok || len(fam) == 0 || fam[0].String() == assetID {
		return false
	}
	head := fam[0]
	return head.Type == AssetClassic && StablecoinCodes[head.Code]
}

// StablecoinSACForms returns, sorted, every SAC form in the active alias registry that [quoteRank] ranks as a stablecoin.
func StablecoinSACForms() []string {
	var out []string
	for form := range activeAliasRegistry().families {
		if isStablecoinSACForm(form) {
			out = append(out, form)
		}
	}
	sort.Strings(out)
	return out
}

// Orient returns the canonical (base, quote) of the market formed by asset_ids a and b, and whether the input order is flipped (invert the stored price and swap per-leg amounts).
func Orient(a, b string) (base, quote string, flipped bool) {
	ra, rb := quoteRank(a), quoteRank(b)
	bIsQuote := rb > ra || (rb == ra && b > a)
	if bIsQuote {
		return a, b, false // a/b is already canonical
	}
	return b, a, true // canonical is b/a; the input a/b is flipped
}

// Canonical returns p re-oriented to its canonical (base, quote) form and whether it was flipped (so the caller can invert a derived price).
func (p Pair) Canonical() (Pair, bool) {
	_, _, flipped := Orient(p.Base.String(), p.Quote.String())
	if !flipped {
		return p, false
	}
	return Pair{Base: p.Quote, Quote: p.Base}, true
}
