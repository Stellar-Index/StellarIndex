package aggregate

import (
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Stablecoin to fiat proxy mapping is aggregator policy, not decoder policy: pegs break (USDT at 0.9680 is news), so we store the raw pair (XLM/USDT)
// and rewrite quote-side stablecoins to fiat at VWAP compute time only (ADR-0014). To add a ticker, add it here and to knownCryptoCodes in
// internal/canonical/asset_crypto.go; a token with a disputed peg belongs behind a per-deployment flag, not here.

var stablecoinFiatProxy = map[string]string{
	// USD-pegged stablecoins.
	"USDT":  "USD",
	"USDC":  "USD",
	"DAI":   "USD",
	"PYUSD": "USD",
	"USDP":  "USD",
	// EUR-pegged stablecoins. EURC/EUROC are one asset (canonical
	// eurcAliasFamily); both keys stay because this map is keyed by the
	// raw Asset.Code, before alias resolution.
	"EURC":  "EUR",
	"EUROC": "EUR",
	"EUROB": "EUR",
	// MXN-pegged stablecoin.
	"MXNe": "MXN",
}

// FiatProxy returns the fiat a known-peg crypto:<STABLE> ticker tracks; ok=false for everything else, including classic-issued stablecoins, which keep their issuer identity.
func FiatProxy(a canonical.Asset) (canonical.Asset, bool) {
	if a.Type != canonical.AssetCrypto {
		return canonical.Asset{}, false
	}
	fiat, ok := stablecoinFiatProxy[a.Code]
	if !ok {
		return canonical.Asset{}, false
	}
	// All targets ("USD", "EUR", "MXN") are on the ADR-0010 fiat
	// allow-list — NewFiatAsset cannot fail here. Construct via the
	// typed ctor so future additions catch mis-spellings at startup.
	proxy, err := canonical.NewFiatAsset(fiat)
	if err != nil {
		// Unreachable unless the allow-list regresses; keep the "no proxy available" semantic rather than panic, so a
		// misconfiguration degrades to "stablecoin stays crypto" at the aggregator.
		return canonical.Asset{}, false
	}
	return proxy, true
}

// IsFiatProxyFor reports whether a is a crypto:<STABLE> ticker pegged to fiatCode, from the same map as [FiatProxy] so USD-volume accounting recognises every peg without a second list.
func IsFiatProxyFor(a canonical.Asset, fiatCode string) bool {
	proxy, ok := FiatProxy(a)
	return ok && proxy.Code == fiatCode
}

// ProxyPair rewrites only the QUOTE side through the stablecoin-to-fiat map (rewriting the base would pose the question on the wrong axis); ok=false when the quote is not a known stablecoin.
func ProxyPair(p canonical.Pair) (canonical.Pair, bool) {
	proxy, ok := FiatProxy(p.Quote)
	if !ok {
		return canonical.Pair{}, false
	}
	rewritten, err := canonical.NewPair(p.Base, proxy)
	if err != nil {
		// Base and proxy quote collide (e.g. base fiat:USD, quote crypto:USDT): leave the pair alone rather than fail the tick.
		return canonical.Pair{}, false
	}
	return rewritten, true
}

// ProxyTrade returns a copy of the trade with its Pair rewritten via [ProxyPair], or the original and ok=false when no proxy applies.
func ProxyTrade(t canonical.Trade) (canonical.Trade, bool) {
	p, ok := ProxyPair(t.Pair)
	if !ok {
		return t, false
	}
	t.Pair = p
	return t, true
}

// FiatBackers returns the stablecoin tickers pegged to fiat, SORTED so the fetch plan and merged window order are identical on every call (the local outlier filter references neighbours by position); nil if none.
func FiatBackers(fiat string) []string {
	var out []string
	for stable, target := range stablecoinFiatProxy {
		if target == fiat {
			out = append(out, stable)
		}
	}
	sort.Strings(out)
	return out
}

// ExpandTargetPair enumerates the source pairs to fetch for a target: for a fiat quote the direct pair plus one per stablecoin backer, otherwise just the target; an error means a malformed target.
func ExpandTargetPair(target canonical.Pair) ([]canonical.Pair, error) {
	return ExpandTargetPairWithClassicPegs(target, nil)
}

// ExpandTargetPairWithClassicPegs is [ExpandTargetPair] plus operator-declared classic USD-pegged credits (each Type=AssetClassic, appended only for a USD target), which keep issuer identity and are not in the abstract map.
func ExpandTargetPairWithClassicPegs(target canonical.Pair, classicUSDPegs []canonical.Asset) ([]canonical.Pair, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	if target.Quote.Type != canonical.AssetFiat {
		// Not a fiat target — no stablecoin expansion.
		return []canonical.Pair{target}, nil
	}
	backers := FiatBackers(target.Quote.Code)
	out := make([]canonical.Pair, 0, 1+len(backers)+len(classicUSDPegs))
	out = append(out, target)
	for _, ticker := range backers {
		stable, err := canonical.NewCryptoAsset(ticker)
		if err != nil {
			// Already guarded by TestFiatProxy_CodesAreOnCanonicalAllowList
			// — skip defensively so a future registry change can't crash
			// the tick loop.
			continue
		}
		src, err := canonical.NewPair(target.Base, stable)
		if err != nil {
			// Skip when target.Base equals the stablecoin (target USDC/fiat:USD would build `crypto:USDC/crypto:USDC`); the direct
			// target entry still stands.
			continue
		}
		out = append(out, src)
	}
	if target.Quote.Code == "USD" {
		for _, classic := range classicUSDPegs {
			if classic.Type != canonical.AssetClassic {
				continue
			}
			src, err := canonical.NewPair(target.Base, classic)
			if err != nil {
				// Same defensive-skip rationale as above — never
				// abort the tick loop on a single malformed entry.
				continue
			}
			out = append(out, src)
		}
	}
	return out, nil
}
