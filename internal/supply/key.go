package supply

import (
	"fmt"
	"strings"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// AssetKey produces the supply-package canonical key for a
// [canonical.Asset]. Output shape per ADR-0011:
//
//   - native XLM         → "XLM"
//   - classic (CODE:G…)  → "CODE:ISSUER"   (colon, matches the ADR
//     schema; canonical.Asset
//     uses dash for the API
//     surface, supply uses
//     colon for storage)
//   - SEP-41 Soroban     → "<contract_id>" (bare C-strkey)
//
// Off-chain assets (fiat, crypto-pure, RWA) have no on-chain supply we
// publish; AssetKey returns ("", error) for those — the supply
// package never derives values for them, so the key is meaningless.
func AssetKey(a canonical.Asset) (string, error) {
	switch a.Type {
	case canonical.AssetNative:
		return "XLM", nil
	case canonical.AssetClassic:
		return a.Code + ":" + a.Issuer, nil
	case canonical.AssetSoroban:
		return a.ContractID, nil
	case canonical.AssetFiat, canonical.AssetCrypto, canonical.AssetRWA:
		return "", fmt.Errorf("supply: off-chain asset %q has no on-chain supply key", a.String())
	default:
		return "", fmt.Errorf("supply: unknown asset type %q", a.Type)
	}
}

// CanonicalizeWatchedClassic converts operator-config entries
// (canonical "CODE-ISSUER" wire form, per the [supply]
// watched_classic_assets doc) into the CODE:ISSUER AssetKey form the
// classic-supply observers' decoders produce. THE BUG THIS FIXES
// (2026-07-02, found by verify-served-values): the raw config strings
// went straight into the observers' watched sets, so dash-form
// entries never matched colon-form decoded keys and the trustline /
// claimable / LP observers silently observed NOTHING — every classic
// asset's served supply degraded to its SAC-held slice (USDC read
// 40M vs ~266M real, an 85% under-read on the flagship stablecoin).
// Entries already in colon form pass through; anything unparseable
// is a loud error so a config typo can never silently zero a supply
// component again.
func CanonicalizeWatchedClassic(entries []string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(e, ":") { // already CODE:ISSUER
			out = append(out, e)
			continue
		}
		a, err := canonical.ParseAsset(e)
		if err != nil || a.Type != canonical.AssetClassic {
			return nil, fmt.Errorf("supply: watched classic asset %q: want canonical CODE-ISSUER form: %w", e, err)
		}
		out = append(out, a.Code+":"+a.Issuer)
	}
	return out, nil
}
