// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

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

// aliasFamilies maps every canonical asset_id that belongs to a
// multi-form equivalence class to that class. XLM is the only member
// today; adding a second family is a new entry here plus a case in
// TestAssetAliases — no call site changes, because every read path goes
// through [AssetAliases].
var aliasFamilies = func() map[string][]Asset {
	m := make(map[string][]Asset, len(xlmAliasFamily))
	for _, a := range xlmAliasFamily {
		m[a.String()] = xlmAliasFamily
	}
	return m
}()

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
// # Known limit: only XLM, and only the leg that names it
//
// EVERY SAC-wrapped classic asset has the same dual identity (USDC's SAC,
// AQUA's SAC, …), and this primitive unifies only XLM. Generalising it
// needs a classic↔SAC registry, and the honest options are both larger
// than a leaf-package constant:
//
//   - Derive: [Asset.SacContractID] is a pure function, so the
//     classic→SAC direction needs no registry at all. The REVERSE
//     (C-address → classic) is not invertible — it is a hash — so a
//     C-address seen in a trade row can only be resolved through a
//     lookup table.
//   - Register: `[supply].sac_wrappers` (operator TOML, 38 entries) is
//     exactly that table, but it is loaded by the indexer/aggregator
//     binaries, not by `canonical`, which is a leaf package with no
//     config dependency. Wiring it in would mean either package-level
//     MUTABLE state in a leaf package (a data race and a
//     test-pollution hazard, and it would make AssetAliases
//     process-dependent) or threading a registry parameter through the
//     ten-odd api/v1 + aggregate call sites and into cmd/stellarindex-api.
//
// The deliberate decision is therefore: unify XLM here (the only asset
// whose three-way split is demonstrably live on the served price path),
// and leave the general case to a future explicit `AliasRegistry` value
// constructed at binary start-up from `[supply].sac_wrappers` and passed
// to the read paths — a change that touches wiring in three fences and
// should land as its own unit of work, not smuggled into a leaf constant.
func AssetAliases(asset Asset) []Asset {
	family, ok := aliasFamilies[asset.String()]
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

// AssetAliasStrings is the string-set projection of [AssetAliases] —
// what SQL membership predicates (`= ANY($1)`) bind. Same order, same
// completeness; separated so callers building a bound array don't
// re-implement the String() loop.
func AssetAliasStrings(asset Asset) []string {
	forms := AssetAliases(asset)
	out := make([]string, 0, len(forms))
	for _, f := range forms {
		out = append(out, f.String())
	}
	return out
}
