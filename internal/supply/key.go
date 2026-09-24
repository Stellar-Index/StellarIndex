package supply

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
// Off-chain assets (fiat, crypto-pure, RWA) and raw oracle symbols
// have no on-chain supply we publish; AssetKey returns ("", error)
// for those — the supply package never derives values for them, so
// the key is meaningless.
func AssetKey(a canonical.Asset) (string, error) {
	switch a.Type {
	case canonical.AssetNative:
		return "XLM", nil
	case canonical.AssetClassic:
		return a.Code + ":" + a.Issuer, nil
	case canonical.AssetSoroban:
		return a.ContractID, nil
	case canonical.AssetFiat, canonical.AssetCrypto, canonical.AssetRWA, canonical.AssetOracleRaw:
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

// ParseAssetKey resolves any operator spelling [canonical.ParseAsset]
// accepts ("native"/"XLM", CODE-ISSUER or CODE:ISSUER, a C-strkey) to
// the [AssetKey] form snapshots are keyed on.
func ParseAssetKey(raw string) (string, error) {
	a, err := canonical.ParseAsset(raw)
	if err != nil {
		return "", fmt.Errorf("supply: asset key %q: %w", raw, err)
	}
	return AssetKey(a)
}

// CanonicalizeStaleComponentLedgers re-keys an operator's
// [supply] stale_component_ledgers_by_asset map onto [AssetKey] form,
// the exact-match key the Refresher's per-asset gate reads. An
// unparseable key, or two spellings of one asset, is an error: either
// would otherwise leave the global threshold silently in force.
func CanonicalizeStaleComponentLedgers(byAsset map[string]uint32) (map[string]uint32, error) {
	if len(byAsset) == 0 {
		return nil, nil
	}
	raws := make([]string, 0, len(byAsset))
	for raw := range byAsset {
		raws = append(raws, raw)
	}
	sort.Strings(raws)
	out := make(map[string]uint32, len(byAsset))
	spelledAs := make(map[string]string, len(byAsset))
	for _, raw := range raws {
		key, err := ParseAssetKey(raw)
		if err != nil {
			return nil, err
		}
		if prev, dup := spelledAs[key]; dup {
			return nil, fmt.Errorf("supply: asset keys %q and %q both name %q", prev, raw, key)
		}
		spelledAs[key] = raw
		out[key] = byAsset[raw]
	}
	return out, nil
}
