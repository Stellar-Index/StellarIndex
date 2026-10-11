// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"
	"sort"
	"sync/atomic"
)

// XLMSacContractID is the PUBNET Stellar Asset Contract address wrapping native XLM, XLM's third
// identity alongside `native` (SDEX) and `crypto:XLM` (CEX tickers).
//
// Pubnet default only; code that must follow the configured network calls [NativeSACContractID].
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

// AliasRegistry is an immutable asset equivalence-class table mapping every form of a multi-form
// asset to its family, in the priority order read paths must try (SAC LAST, a money-safety
// invariant; see [AssetAliases]).
//
// It is the compile-time baseline plus every classic↔SAC pair in `[supply].sac_wrappers`. A nil
// *AliasRegistry behaves as the installed network's baseline (pubnet when none).
type AliasRegistry struct {
	families map[string][]Asset
}

// defaultAliasRegistry is the compile-time registry used until [InstallAliasRegistry] runs.
var defaultAliasRegistry = &AliasRegistry{families: baseAliasFamilies}

// activeRegistry holds the process-wide registry [AssetAliases] reads; the atomic pointer lets
// tests swap registries without a lock. A nil load means "not installed", so the baseline is used.
var activeRegistry atomic.Pointer[AliasRegistry]

// InstallAliasRegistry publishes r as the process-wide registry behind [AssetAliases] and
// [AssetAliasStrings]. Call it once at start-up, after config load and before serving, so every
// alias-looping read path covers each configured classic↔SAC pair. A nil r resets to the baseline.
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

// NewAliasRegistry builds the registry from `[supply].sac_wrappers` (SAC C-strkey → classic
// `CODE:ISSUER`) on the network named by passphrase: the baseline plus one two-form family per
// wrapper, SAC LAST so a thin Soroban pool cannot outrank the classic asset's depth.
//
// It is strict, returning an error instead of dropping an entry: a dropped wrapper is invisible
// under-counted volume, and a wrong one merges an arbitrary contract into a classic identity.
// It rejects:
//   - a malformed contract id or asset key;
//   - a SAC id that is not the deterministic SAC derivation of its classic asset on this
//     network (e.g. a scam token aliased onto the classic asset);
//   - a wrapper claiming a form already in another family.
//
// Self-mapped entries (pure SEP-41) and a verified XLM SAC → `native` entry are no-ops.
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

// Aliases is [AssetAliases] resolved against this registry: the input first, then its class in
// priority order (SAC last). A form with no family returns just itself.
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

// Canonical returns the priority-first form of asset's family: the classic asset for a
// classic↔SAC pair, `native` for any XLM form. Unlike [AliasRegistry.Aliases] it ignores the
// caller's spelling, so it is a stable group key.
//
// It is the directory fold key: listing rows with Equal Canonical forms are one asset and must
// collapse to one row (else a SAC twin shows twice). An asset in no family is its own
// canonical form, so callers fold unconditionally.
func (r *AliasRegistry) Canonical(asset Asset) Asset {
	fams := r.familyMap()
	if family, ok := fams[asset.String()]; ok && len(family) > 0 {
		return family[0]
	}
	return asset
}

// AliasForms maps every non-canonical alias form to its family's canonical form. It is the SQL
// projection: a rollup GROUP BYs on COALESCE(map[form], form) so a SAC twin and its classic
// agree, matching the per-asset fold of [AssetAliasStrings]. Canonical forms are omitted (the
// COALESCE covers them). A nil registry projects the baseline.
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

// AssetAliases returns every form equivalent to asset that a read path should try: the literal
// input first, then the rest of its class in canonical order. No second form returns just itself.
//
// XLM has three identities, all the same asset: `native` (SDEX rows, the CAGGs), `crypto:XLM`
// (CEX venues, Reflector's CEX oracle) and the SAC wrapping native XLM (Soroban AMMs). A read
// keyed by one form silently omits every venue publishing under another.
//
// The SAC form is LAST on purpose: read paths take the FIRST form that answers, and Soroban XLM
// pools are far thinner than SDEX and CEX feeds, so one small pool could become THE served XLM
// price. A caller naming the C-address still gets it first.
//
// Other SAC-wrapped classic assets come from [NewAliasRegistry] and [InstallAliasRegistry].
func AssetAliases(asset Asset) []Asset {
	return activeAliasRegistry().Aliases(asset)
}

// AssetAliasStrings is the string projection of [AssetAliases], what SQL `= ANY($1)` predicates
// bind. Same order and completeness.
func AssetAliasStrings(asset Asset) []string {
	return activeAliasRegistry().AliasStrings(asset)
}

// AllAliasForms is the process-registry projection of [AliasRegistry.AliasForms]. A SQL rollup
// binds it to fold each raw trades asset side onto its canonical asset, consistent with the
// per-asset [AssetAliasStrings] read.
func AllAliasForms() map[string]string {
	return activeAliasRegistry().AliasForms()
}

// CanonicalAsset resolves asset to its family's priority-first form against the process-wide
// registry: the directory fold key for listing twins (SAC / crypto:XLM). See
// [AliasRegistry.Canonical]; falls back to the baseline like [AssetAliases].
func CanonicalAsset(asset Asset) Asset {
	return activeAliasRegistry().Canonical(asset)
}
