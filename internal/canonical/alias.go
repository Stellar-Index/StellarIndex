// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"
	"sort"
	"sync/atomic"
)

// XLMSacContractID is the PUBNET SAC address wrapping native XLM, its third identity beside `native` and `crypto:XLM`; follow the configured network via [NativeSACContractID].
const XLMSacContractID = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

// xlmAliasFamily is the XLM equivalence class in canonical priority order: established forms
// first, SAC last (see [AssetAliases]). Struct literals, because the New* helpers return errors.
var xlmAliasFamily = xlmAliasFamilyFor(XLMSacContractID)

func xlmAliasFamilyFor(sac string) []Asset {
	return []Asset{
		{Type: AssetNative},
		{Type: AssetCrypto, Code: "XLM"},
		{Type: AssetSoroban, ContractID: sac},
	}
}

// eurcAliasFamily covers Circle's EUROC to EURC rename: the catalogue uses `EURC` while RedStone
// publishes `EUROC`, so a read keyed on one spelling would miss the other. EUROB is a different token.
var eurcAliasFamily = []Asset{
	{Type: AssetCrypto, Code: "EURC"},
	{Type: AssetCrypto, Code: "EUROC"},
}

// baseAliasFamilies maps every asset_id in a compile-time-known equivalence class (XLM's three
// forms, EURC/EUROC) to its class. They apply regardless of operator config, so every
// [AliasRegistry] starts from them; [NewAliasRegistry] layers config-declared pairs on top.
var baseAliasFamilies = baseAliasFamiliesFor(XLMSacContractID)

// baseAliasFamiliesFor is [baseAliasFamilies] with the XLM family's SAC
// member set to the given network's native SAC.
func baseAliasFamiliesFor(xlmSAC string) map[string][]Asset {
	compileTimeFamilies := [][]Asset{xlmAliasFamilyFor(xlmSAC), eurcAliasFamily}
	m := make(map[string][]Asset)
	for _, fam := range compileTimeFamilies {
		for _, a := range fam {
			m[a.String()] = fam
		}
	}
	return m
}

// AliasRegistry is an immutable asset equivalence-class table (baseline plus `[supply].sac_wrappers` pairs) in read-path priority order, SAC LAST; a nil registry is the network baseline.
type AliasRegistry struct {
	families map[string][]Asset
}

// defaultAliasRegistry is the compile-time registry used until [InstallAliasRegistry] runs.
var defaultAliasRegistry = &AliasRegistry{families: baseAliasFamilies}

// activeRegistry holds the process-wide registry [AssetAliases] reads; the atomic pointer lets
// tests swap registries without a lock. A nil load means "not installed", so the baseline is used.
var activeRegistry atomic.Pointer[AliasRegistry]

// InstallAliasRegistry publishes r as the process-wide registry behind [AssetAliases]; call once at start-up before serving, nil resets to the baseline.
func InstallAliasRegistry(r *AliasRegistry) {
	activeRegistry.Store(r)
}

// activeAliasRegistry returns the installed registry, or the baseline
// when none has been installed.
func activeAliasRegistry() *AliasRegistry {
	if r := activeRegistry.Load(); r != nil {
		return r
	}
	return baselineAliasRegistry()
}

// networkBaseRegistry is the compile-time baseline re-derived for the
// passphrase [InstallNetworkPassphrase] published; nil until one is.
var networkBaseRegistry atomic.Pointer[AliasRegistry]

// baselineAliasRegistry is the installed network's baseline, else
// pubnet's, so the XLM family never disagrees with [NativeSACContractID].
func baselineAliasRegistry() *AliasRegistry {
	if r := networkBaseRegistry.Load(); r != nil {
		return r
	}
	return defaultAliasRegistry
}

// familyMap resolves a nil receiver to the baseline registry's table.
func (r *AliasRegistry) familyMap() map[string][]Asset {
	if r == nil {
		return baselineAliasRegistry().families
	}
	return r.families
}

// NewAliasRegistry builds the registry from `[supply].sac_wrappers`, SAC LAST, strictly: it errors on a malformed id, a SAC that is not the deterministic derivation of its classic asset, or a form already in another family.
func NewAliasRegistry(passphrase string, sacWrappers map[string]string) (*AliasRegistry, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("alias registry: empty network passphrase")
	}
	xlmSAC, err := NativeAsset().sacContractIDOn(passphrase)
	if err != nil {
		return nil, fmt.Errorf("alias registry: native SAC: %w", err)
	}
	base := baseAliasFamiliesFor(xlmSAC)
	families := make(map[string][]Asset, len(base)+2*len(sacWrappers))
	for k, v := range base {
		families[k] = v
	}

	// Deterministic iteration: stable error reporting.
	sacIDs := make([]string, 0, len(sacWrappers))
	for id := range sacWrappers {
		sacIDs = append(sacIDs, id)
	}
	sort.Strings(sacIDs)

	for _, sacID := range sacIDs {
		if err := addSACWrapperFamily(families, passphrase, sacID, sacWrappers[sacID]); err != nil {
			return nil, err
		}
	}
	return &AliasRegistry{families: families}, nil
}

// addSACWrapperFamily validates one SAC wrapper entry and, unless it is a
// no-op, registers its two-form family in families.
func addSACWrapperFamily(families map[string][]Asset, passphrase, sacID, classicKey string) error {
	sac, err := NewSorobanAsset(sacID)
	if err != nil {
		return fmt.Errorf("alias registry: sac wrapper contract id %q: %w", sacID, err)
	}
	classic, err := ParseAsset(classicKey)
	if err != nil {
		return fmt.Errorf("alias registry: sac wrapper %q asset key %q: %w", sacID, classicKey, err)
	}
	sacStr, classicStr := sac.String(), classic.String()
	if sacStr == classicStr {
		// Pure SEP-41 self-map: one identity, nothing to unify.
		return nil
	}
	derived, err := classic.sacContractIDOn(passphrase)
	if err != nil {
		return fmt.Errorf("alias registry: sac wrapper %q asset key %q: %w", sacID, classicKey, err)
	}
	if derived != sacStr {
		return fmt.Errorf("alias registry: sac wrapper %q is not the SAC of %q on this network (derived %s)",
			sacID, classicKey, derived)
	}
	if classic.Type == AssetNative {
		// Proven the network's XLM SAC above; the baseline owns the XLM family.
		return nil
	}
	if _, ok := families[sacStr]; ok {
		return fmt.Errorf("alias registry: sac wrapper %q: contract already belongs to another alias family", sacID)
	}
	if _, ok := families[classicStr]; ok {
		return fmt.Errorf("alias registry: sac wrapper %q: asset %q already belongs to another alias family", sacID, classicKey)
	}
	// SAC form LAST (money safety): read paths take the FIRST alias that answers, so the deep
	// classic form is tried before the thin SAC pool.
	family := []Asset{classic, sac}
	families[classicStr] = family
	families[sacStr] = family
	return nil
}

// Aliases is [AssetAliases] resolved against this registry: the input first, then its class in priority order (SAC last).
func (r *AliasRegistry) Aliases(asset Asset) []Asset {
	fams := r.familyMap()
	family, ok := fams[asset.String()]
	if !ok {
		return []Asset{asset}
	}
	out := make([]Asset, 0, len(family))
	out = append(out, asset)
	for _, f := range family {
		if f.String() == asset.String() {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Canonical returns the priority-first form of asset's family (classic for a SAC pair, `native` for any XLM form), the stable key listings fold on; an asset in no family is its own canonical form.
func (r *AliasRegistry) Canonical(asset Asset) Asset {
	fams := r.familyMap()
	if family, ok := fams[asset.String()]; ok && len(family) > 0 {
		return family[0]
	}
	return asset
}

// AliasForms maps every non-canonical alias form to its family's canonical form, for SQL rollups that GROUP BY COALESCE(map[form], form); nil registry projects the baseline.
func (r *AliasRegistry) AliasForms() map[string]string {
	fams := r.familyMap()
	out := make(map[string]string, len(fams))
	for form, family := range fams {
		if len(family) == 0 {
			continue
		}
		canon := family[0].String()
		if form == canon {
			continue
		}
		out[form] = canon
	}
	return out
}

// AliasStrings is the string-set projection of [AliasRegistry.Aliases].
func (r *AliasRegistry) AliasStrings(asset Asset) []string {
	forms := r.Aliases(asset)
	out := make([]string, 0, len(forms))
	for _, f := range forms {
		out = append(out, f.String())
	}
	return out
}

// AssetAliases returns every equivalent form a read path must try, input first, SAC LAST (XLM has three identities: `native`, `crypto:XLM`, its SAC; a read keyed by one silently omits the others).
func AssetAliases(asset Asset) []Asset {
	return activeAliasRegistry().Aliases(asset)
}

// AssetAliasStrings is the string projection of [AssetAliases] for SQL `= ANY($1)` binds, same order and completeness.
func AssetAliasStrings(asset Asset) []string {
	return activeAliasRegistry().AliasStrings(asset)
}

// AllAliasForms is the process-registry projection of [AliasRegistry.AliasForms], consistent with the per-asset [AssetAliasStrings] read.
func AllAliasForms() map[string]string {
	return activeAliasRegistry().AliasForms()
}

// CanonicalAsset resolves asset to its family's priority-first form against the process-wide registry; see [AliasRegistry.Canonical].
func CanonicalAsset(asset Asset) Asset {
	return activeAliasRegistry().Canonical(asset)
}
