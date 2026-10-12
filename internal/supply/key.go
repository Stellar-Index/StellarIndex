package supply

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const nativeAssetKey = "XLM"

// AssetKey produces the supply key for a [canonical.Asset]: native is "XLM", classic "CODE:ISSUER" (colon), SEP-41 the bare C-strkey; off-chain and raw oracle assets return ("", error).
func AssetKey(a canonical.Asset) (string, error) {
	switch a.Type {
	case canonical.AssetNative:
		return nativeAssetKey, nil
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

// CanonicalizeWatchedClassic converts operator "CODE-ISSUER" entries into the CODE:ISSUER form observers produce (dash form would match nothing); unparseable or non-classic entries are a loud error.
func CanonicalizeWatchedClassic(entries []string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		a, err := canonical.ParseAsset(e)
		if err != nil {
			return nil, fmt.Errorf("supply: watched classic asset %q: want canonical CODE-ISSUER form: %w", e, err)
		}
		if a.Type != canonical.AssetClassic {
			return nil, fmt.Errorf("supply: watched classic asset %q: want canonical CODE-ISSUER form, got a %s asset", e, a.Type)
		}
		out = append(out, a.Code+":"+a.Issuer)
	}
	return out, nil
}

// SACWrapperAssetKey is the asset_key the sac_balances observer stamps on a sac_wrappers entry's rows: CODE:ISSUER for a SAC-wrapped classic asset (classic=true), else the value verbatim.
func SACWrapperAssetKey(value string) (key string, classic bool, err error) {
	if !strings.ContainsAny(value, "-:") {
		return value, false, nil
	}
	canon, err := CanonicalizeWatchedClassic([]string{value})
	if err != nil {
		return "", false, err
	}
	return canon[0], true, nil
}

// SACClassicKeys maps each SAC contract of a sac_wrappers map to the classic asset_key its balance rows are stored under, omitting pure SEP-41 self-maps.
func SACClassicKeys(wrappers map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(wrappers))
	for cid, v := range wrappers {
		key, classic, err := SACWrapperAssetKey(v)
		if err != nil {
			return nil, fmt.Errorf("supply: sac_wrappers[%q]: %w", cid, err)
		}
		if classic {
			out[cid] = key
		}
	}
	return out, nil
}

// ParseAssetKey resolves any operator spelling [canonical.ParseAsset] accepts to the [AssetKey] form snapshots are keyed on.
func ParseAssetKey(raw string) (string, error) {
	a, err := canonical.ParseAsset(raw)
	if err != nil {
		return "", fmt.Errorf("supply: asset key %q: %w", raw, err)
	}
	return AssetKey(a)
}

// CanonicalizeStaleComponentLedgers re-keys stale_component_ledgers_by_asset onto [AssetKey] form; an unparseable key or two spellings of one asset is an error.
func CanonicalizeStaleComponentLedgers(byAsset map[string]uint32) (map[string]uint32, error) {
	return canonicalizeAssetKeys(byAsset)
}

// errNativePolicyKey rejects an XLM entry in [Policy.PerAsset] or
// [Policy.MaxSupplyOverrides]: only the classic and SEP-41 computers
// read those maps, so the entry would silently apply nothing.
var errNativePolicyKey = errors.New("native XLM takes no per-asset locked set or max_supply override " +
	"(Algorithm 1 reads neither; exclude XLM reserves via sdf_reserve_accounts)")

// CanonicalizePolicyKeys re-keys a per_asset_locked_sets or max_supply_overrides map onto [AssetKey] form; an unparseable key, two spellings of one asset, or native XLM is an error.
func CanonicalizePolicyKeys[V any](byAsset map[string]V) (map[string]V, error) {
	out, err := canonicalizeAssetKeys(byAsset)
	if err != nil {
		return nil, err
	}
	if _, ok := out[nativeAssetKey]; ok {
		return nil, fmt.Errorf("supply: %w", errNativePolicyKey)
	}
	return out, nil
}

// validatePolicyKey rejects a [Policy] map key the computers' exact
// match can never hit: unparseable, native XLM, or not in [AssetKey]
// form (e.g. the dash spelling of a classic asset).
func validatePolicyKey(field, key string) error {
	canon, err := ParseAssetKey(key)
	switch {
	case err != nil:
		return fmt.Errorf("supply: %s key: %w", field, err)
	case canon == nativeAssetKey:
		return fmt.Errorf("supply: %s key %q: %w", field, key, errNativePolicyKey)
	case canon != key:
		return fmt.Errorf("supply: %s key %q is not in asset_key form (want %q) and would never match", field, key, canon)
	}
	return nil
}

func canonicalizeAssetKeys[V any](byAsset map[string]V) (map[string]V, error) {
	if len(byAsset) == 0 {
		return nil, nil
	}
	raws := make([]string, 0, len(byAsset))
	for raw := range byAsset {
		raws = append(raws, raw)
	}
	sort.Strings(raws)
	out := make(map[string]V, len(byAsset))
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
