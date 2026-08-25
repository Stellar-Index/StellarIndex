// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"
	"sort"
	"sync/atomic"
)

// XLMSacContractID is the pubnet Stellar Asset Contract address that
// wraps native XLM — XLM's third canonical identity, alongside `native`
// (the per-network strkey-less form SDEX writes) and `crypto:XLM` (the
// cross-network ticker form every CEX writes).
//
// It is NOT a fixture: it is the deterministic derivation of
// `NativeAsset().SacContractID()` against [PubnetPassphrase], pinned
// both by TestSacContractID_Golden (sac_test.go) and by
// TestXLMSacContractID_MatchesDerivation here, so the literal can never
// drift from the derivation. The literal is kept as a constant rather
// than derived at init because [Asset.SacContractID] returns an error
// and this value is needed in constant position (SQL predicates,
// orientation ranking).
const XLMSacContractID = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

// xlmAliasFamily is the XLM equivalence class in CANONICAL PRIORITY
// ORDER: the two established forms first, the SAC wrapper last. See
// [AssetAliases] for why the order is load-bearing.
//
// Constructed as struct literals rather than through the New* helpers
// because those return errors and this must be a package-level var;
// TestXLMAliasFamily_IsValid pins that every member passes Validate().
var xlmAliasFamily = []Asset{
	{Type: AssetNative},
	{Type: AssetCrypto, Code: "XLM"},
	{Type: AssetSoroban, ContractID: XLMSacContractID},
}

// baseAliasFamilies maps every canonical asset_id that belongs to a
// COMPILE-TIME-KNOWN multi-form equivalence class to that class. XLM is
// the only such member: its three-way split (`native` / `crypto:XLM` /
// SAC) is live on the served price path regardless of operator config,
// so it is unified here unconditionally and forms the baseline every
// [AliasRegistry] starts from. Config-declared classic↔SAC pairs are
// layered on top by [NewAliasRegistry].
var baseAliasFamilies = func() map[string][]Asset {
	m := make(map[string][]Asset, len(xlmAliasFamily))
	for _, a := range xlmAliasFamily {
		m[a.String()] = xlmAliasFamily
	}
	return m
}()

// AliasRegistry is an immutable asset equivalence-class table: it maps
// every canonical FORM that belongs to a multi-form asset to that
// asset's family, in the CANONICAL PRIORITY ORDER the read paths must
// try (SAC form LAST — see [AssetAliases] for why that ordering is a
// money-safety invariant, not a style choice).
//
// It is the explicit value alias.go's history called for: the XLM
// baseline plus every classic↔SAC pair declared in the operator's
// `[supply].sac_wrappers`, resolved ONCE at binary start-up and then
// read-only. A nil *AliasRegistry is valid and behaves as the XLM-only
// baseline, so leaf-package callers with no config in scope still get
// correct (if XLM-limited) behaviour.
type AliasRegistry struct {
	families map[string][]Asset
}

// defaultAliasRegistry is the XLM-only registry used until a
// config-derived one is installed via [InstallAliasRegistry]. It is what
// [AssetAliases] resolves against in unit tests and in binaries that
// never call InstallAliasRegistry.
var defaultAliasRegistry = &AliasRegistry{families: baseAliasFamilies}

// activeRegistry holds the process-wide registry [AssetAliases] reads.
// It is written at most once, at binary start-up, before any read path
// runs; the atomic pointer makes that publish race-free (go test -race)
// and lets tests swap a config-derived registry in and out without a
// lock. A nil load means "not yet installed" → the default is used.
var activeRegistry atomic.Pointer[AliasRegistry]

// InstallAliasRegistry publishes r as the process-wide registry that
// [AssetAliases] and [AssetAliasStrings] resolve against. Call it ONCE,
// at binary start-up, after config load and before serving — this is the
// seam that turns the ~11 alias-looping read paths (and the handlers
// that adopt the same loop) alias-complete for every configured
// classic↔SAC pair, with no change to AssetAliases's signature or to any
// call site. A nil r resets to the XLM-only default (used by tests).
func InstallAliasRegistry(r *AliasRegistry) {
	activeRegistry.Store(r)
}

// activeAliasRegistry returns the installed registry, or the XLM-only
// default when none has been installed.
func activeAliasRegistry() *AliasRegistry {
	if r := activeRegistry.Load(); r != nil {
		return r
	}
	return defaultAliasRegistry
}

// NewAliasRegistry builds the process registry from the operator's SAC
// wrapper map (`[supply].sac_wrappers`: SAC contract C-strkey →
// classic `CODE:ISSUER`). The result is the XLM baseline PLUS one
// two-form family per wrapper, with the SAC form ordered LAST so a thin
// Soroban pool can never outrank the classic asset's depth on a
// classic-keyed read (the invariant [AssetAliases] documents).
//
// It is strict: a malformed contract id or asset key is returned as an
// error rather than silently dropped, because a dropped wrapper is
// invisible under-counted volume — the exact class of defect this
// registry exists to remove. Entries whose classic form equals their SAC
// form (the pure-SEP-41 `contract_id → contract_id` convention) are a
// single identity with nothing to alias and are skipped. Wrappers that
// would touch a form already claimed by the baseline (e.g. an operator
// listing the XLM SAC itself) are skipped so the baseline's canonical
// families are never overwritten.
func NewAliasRegistry(sacWrappers map[string]string) (*AliasRegistry, error) {
	families := make(map[string][]Asset, len(baseAliasFamilies)+2*len(sacWrappers))
	for k, v := range baseAliasFamilies {
		families[k] = v
	}

	// Deterministic iteration: stable error reporting and stable
	// first-wins resolution of any duplicate classic key.
	sacIDs := make([]string, 0, len(sacWrappers))
	for id := range sacWrappers {
		sacIDs = append(sacIDs, id)
	}
	sort.Strings(sacIDs)

	for _, sacID := range sacIDs {
		classicKey := sacWrappers[sacID]
		sac, err := NewSorobanAsset(sacID)
		if err != nil {
			return nil, fmt.Errorf("alias registry: sac wrapper contract id %q: %w", sacID, err)
		}
		classic, err := ParseAsset(classicKey)
		if err != nil {
			return nil, fmt.Errorf("alias registry: sac wrapper %q asset key %q: %w", sacID, classicKey, err)
		}
		sacStr, classicStr := sac.String(), classic.String()
		if sacStr == classicStr {
			// Pure SEP-41 self-map: one identity, nothing to unify.
			continue
		}
		if _, ok := families[sacStr]; ok {
			continue
		}
		if _, ok := families[classicStr]; ok {
			continue
		}
		// SAC form LAST: money-safety invariant. The read paths take the
		// FIRST alias that produces a usable answer, so on a classic-keyed
		// read the deep classic form is tried before the thin SAC pool.
		family := []Asset{classic, sac}
		families[classicStr] = family
		families[sacStr] = family
	}
	return &AliasRegistry{families: families}, nil
}

// Aliases is the [AssetAliases] contract resolved against this specific
// registry: the literal input first, then the rest of its equivalence
// class in canonical priority order (SAC last). A form with no family
// returns just itself, so callers loop unconditionally.
func (r *AliasRegistry) Aliases(asset Asset) []Asset {
	fams := baseAliasFamilies
	if r != nil {
		fams = r.families
	}
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

// Canonical returns the PRIORITY-FIRST form of asset's equivalence class:
// the classic asset for a configured classic↔SAC pair, `native` for any
// XLM form (native / crypto:XLM / the XLM SAC). Unlike [AliasRegistry.Aliases],
// which leads with the caller's own spelling, Canonical always returns the
// family's FIRST member — the established form the SAC is deliberately
// ordered behind (see [AssetAliases]) — so it is a STABLE group key
// independent of which form a row arrived as.
//
// It is the directory fold key: two listing rows whose Canonical forms are
// Equal are the same asset and must collapse to one row (the SAC twin that
// leaks past literal-asset_id catalogue SQL — the "USDC shows twice"
// defect). An asset in no family is its own canonical form, so callers fold
// unconditionally.
func (r *AliasRegistry) Canonical(asset Asset) Asset {
	fams := baseAliasFamilies
	if r != nil {
		fams = r.families
	}
	if family, ok := fams[asset.String()]; ok && len(family) > 0 {
		return family[0]
	}
	return asset
}

// AliasForms returns every alias FORM this registry knows that is NOT its
// own family-canonical form, mapped to that canonical form's string. It is
// the SQL-translation projection of the registry: a rollup that folds
// trades onto their canonical asset — the same fold [AssetAliasStrings]
// applies per-asset through the alias array — GROUP BYs on
// COALESCE(map[form], form), so a SAC twin and its classic agree. A form
// that is already its family's canonical (native; the classic side of a
// classic↔SAC pair) is omitted: it maps to itself, which the COALESCE
// fallback already covers. A nil registry projects the XLM-only baseline.
func (r *AliasRegistry) AliasForms() map[string]string {
	fams := baseAliasFamilies
	if r != nil {
		fams = r.families
	}
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

// AssetAliases returns every canonical FORM equivalent to `asset` that a
// read path should try, in priority order: the literal input first, then
// the rest of its equivalence class in canonical order. An asset with no
// known second form returns just itself, so callers can loop
// unconditionally.
//
// # Why XLM has three identities
//
//   - `native` — the per-network, strkey-less classic form. SDEX trade
//     rows, prices_1m, the CAGGs and every on-chain surface write this.
//   - `crypto:XLM` — the cross-network global-ticker form (ADR-0014).
//     Every CEX venue and Reflector's CEX oracle publish under it.
//   - `CAS3J7GY…` — the Stellar Asset Contract that wraps native XLM.
//     Soroban AMMs (Aquarius, Phoenix, Soroswap) trade the SAC, so
//     Soroban-sourced trade rows carry the C-address on the leg where
//     `native` would appear on SDEX.
//
// All three are the SAME asset with the same economic value. A read
// keyed by one form that does not try the others silently omits every
// venue publishing under the alias — the failure this primitive exists
// to prevent, observed live on 2026-05-29 (/v1/price?asset=native fell
// through to a 39h-stale triangulated bucket while a fresh CEX VWAP sat
// under `crypto:XLM`).
//
// # Why the SAC form is LAST, deliberately
//
// Priority order is a manipulation-surface decision, not a style
// choice. The read paths that loop these aliases (v1.readPriceWithAliases,
// aggregate.tryVWAPTier, v1.lookupPriceAt, the OHLC/chart pair walks)
// take the FIRST form that produces a usable answer. Soroban XLM pools
// are, today, orders of magnitude thinner than SDEX + the CEX feeds:
// putting the SAC form anywhere but last would let a few thousand
// dollars of liquidity in one Soroban pool become THE served XLM price
// while deep, corroborated SDEX and CEX data sat one loop iteration
// away. Last means the SAC form is reached ONLY when both established
// forms miss — i.e. when the alternative is no price at all, where a
// thin-pool print beats a 404 and still carries the same freshness,
// trade-count-floor (aggregate.GlobalPriceOptions.VWAPMinTradeCount) and
// divergence-freeze guards as any other source.
//
// The literal input still comes first even when it IS the SAC form: a
// caller who names the C-address is asking about that form, and it is
// what the pre-alias code already served for that spelling — the
// addition is the native/crypto:XLM FALLBACK behind it, never a
// promotion of the thin pool over the deep ones for a `native` query.
//
// # Set-shaped callers
//
// Membership filters (v1.sourceStatsAliases → the timescale per-source
// breakdowns) consume this as a SET, where order is irrelevant but
// COMPLETENESS is not: those SQL predicates already treat the SAC
// literal as XLM in their volume CASE, so a filter that omitted it
// undercounted Soroban XLM volume while valuing it as XLM wherever it
// did match.
//
// # Generalising beyond XLM: the alias registry
//
// EVERY SAC-wrapped classic asset has the same dual identity (USDC's SAC,
// AQUA's SAC, …). XLM's three-way split is unconditional (see
// [baseAliasFamilies]); every OTHER classic↔SAC pair is operator data,
// declared in `[supply].sac_wrappers` (SAC C-strkey → `CODE:ISSUER`).
//
//   - Derive: [Asset.SacContractID] is a pure function, so the
//     classic→SAC direction needs no table at all. The REVERSE
//     (C-address → classic) is not invertible — it is a hash — so a
//     C-address seen in a trade row can only be resolved through a
//     lookup table, which is exactly what `sac_wrappers` is.
//   - Register: [NewAliasRegistry] builds that table from the config
//     map at binary start-up, and [InstallAliasRegistry] publishes it as
//     the process-wide registry this function resolves against. The
//     publish is a single atomic store before serving, so there is no
//     data race and no per-call config dependency threaded into the leaf
//     package — AssetAliases keeps its signature and every call site is
//     unchanged, while becoming alias-complete for the configured pairs.
//
// Until a registry is installed (unit tests, or a binary that never
// serves reads) the function resolves against the XLM-only baseline, so
// the XLM three-form behaviour is invariant either way.
func AssetAliases(asset Asset) []Asset {
	return activeAliasRegistry().Aliases(asset)
}

// AssetAliasStrings is the string-set projection of [AssetAliases] —
// what SQL membership predicates (`= ANY($1)`) bind. Same order, same
// completeness; separated so callers building a bound array don't
// re-implement the String() loop.
func AssetAliasStrings(asset Asset) []string {
	return activeAliasRegistry().AliasStrings(asset)
}

// AllAliasForms is the process-registry projection of
// [AliasRegistry.AliasForms] — every non-canonical alias form mapped to its
// canonical form's string, resolved against the installed registry (the
// XLM-only baseline until one installs). It is what a SQL rollup binds to
// fold each raw trades asset side onto its canonical asset, keeping the
// fold consistent with the per-asset [AssetAliasStrings] read.
func AllAliasForms() map[string]string {
	return activeAliasRegistry().AliasForms()
}

// CanonicalAsset resolves asset to the priority-first form of its
// equivalence class against the process-wide registry — the directory
// fold key the listing paths group non-canonical (SAC / crypto:XLM) twins
// onto, and the per-asset detail path collapses a SAC-form request onto.
// See [AliasRegistry.Canonical]; mirrors [AssetAliases]'s process-registry
// resolution and its XLM-only fallback before a config registry installs.
func CanonicalAsset(asset Asset) Asset {
	return activeAliasRegistry().Canonical(asset)
}
