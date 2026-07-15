package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// StandardDecimals is the decimals scale every DEX-traded Stellar asset
// carries by policy default: SACs are always 7, classic credits are
// uniformly 7, and every SEP-41 token is assumed 7 unless a confirmed
// exception says otherwise. Mirrors internal/decimalsguard.StandardDecimals
// (duplicated, not imported, to keep this package's dependency set
// unchanged — aggregate.DecimalsLookup is a pure consumer of whatever
// table backs the guard, never the detection logic itself).
const StandardDecimals = 7

// DecimalsLookup resolves an asset's confirmed on-chain decimals() when it
// is known to differ from [StandardDecimals]. Satisfied structurally (no
// import needed) by both *internal/api/v1.NonstandardDecimalsCache and any
// equivalent process-local cache backed by the `nonstandard_decimals_assets`
// table (migration 0093) — the source of truth this package's callers are
// expected to consult. found=false (including for a nil receiver, or a nil
// DecimalsLookup passed to [ResolveDecimals]) means "nothing on record for
// this asset" and MUST be treated as [StandardDecimals], never as an error —
// everything absent from that table is 7dp by policy.
type DecimalsLookup interface {
	Lookup(assetID string) (decimals int, found bool)
}

// ResolveDecimals returns lookup's confirmed decimals for asset, or
// [StandardDecimals] when lookup is nil or has no confirmed row for it.
// Never errors — an unresolvable asset is exactly the common case (every
// 7-decimal asset in existence) and must fall through silently.
func ResolveDecimals(lookup DecimalsLookup, asset canonical.Asset) int {
	if lookup == nil {
		return StandardDecimals
	}
	if d, ok := lookup.Lookup(asset.String()); ok {
		return d
	}
	return StandardDecimals
}

// DecimalsAdjustment returns the exact rational factor K such that
// K × (rawQuoteAmount / rawBaseAmount) == the true quote-per-base price,
// when rawQuoteAmount/rawBaseAmount are smallest-unit integers and the two
// legs are declared at baseDecimals/quoteDecimals respectively.
//
// Derivation: true price = (quote / 10^quoteDecimals) / (base / 10^baseDecimals)
//
//	= (quote × 10^baseDecimals) / (base × 10^quoteDecimals)
//	= (quote / base) × 10^(baseDecimals − quoteDecimals).
//
// So K = 10^(baseDecimals − quoteDecimals), computed as an exact
// *big.Rat — never a float — so it composes losslessly with the exact
// *big.Rat outputs of [VWAP], [TWAP], and [ComputeOHLC] (ADR-0003).
//
// baseDecimals == quoteDecimals (the overwhelmingly common case — every
// pair observed on Stellar mainnet until 2026-06-22, and every pair
// today whose legs are both absent from `nonstandard_decimals_assets`)
// returns the exact rational 1, making [AdjustPrice] a byte-identical
// no-op for that case.
func DecimalsAdjustment(baseDecimals, quoteDecimals int) *big.Rat {
	delta := baseDecimals - quoteDecimals
	if delta == 0 {
		return big.NewRat(1, 1)
	}
	abs := delta
	if abs < 0 {
		abs = -abs
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs)), nil)
	if delta > 0 {
		return new(big.Rat).SetInt(scale)
	}
	return new(big.Rat).SetFrac(big.NewInt(1), scale)
}

// AdjustPrice scales a raw quote/base price ratio by the decimals factor
// implied by baseDecimals and quoteDecimals — see [DecimalsAdjustment].
//
// This is the single normalization primitive every ratio-producing call
// site in this codebase (VWAP, TWAP, OHLC open/high/low/close, and any
// CAGG-sourced ratio read back at serve time) should apply before a price
// leaves the process. It is deliberately a POST-HOC scalar multiply, not a
// change to how VWAP/TWAP/OHLC sum trades — because every trade contributing
// to one aggregation call shares the same (base_asset, quote_asset) pair,
// the correction factor is a single per-call constant, so multiplying the
// finished ratio (or, for VWAP/TWAP, the finished weighted-average ratio —
// linear operations commute with a constant scalar) is exactly equivalent
// to normalizing every trade before summing, without the risk of touching
// the summation itself. See docs/operations/runbooks/dex-nonstandard-decimals.md
// for the full rationale (why this replaces the deferred "rewrite the CAGGs"
// plan) and CLAUDE.md's ADR-0003 note.
//
// Returns nil unchanged (nothing to scale). baseDecimals == quoteDecimals
// returns raw itself (not a copy) — callers that need to avoid aliasing
// should not rely on this, but every current call site treats the result
// as an immutable value immediately consumed for formatting.
func AdjustPrice(raw *big.Rat, baseDecimals, quoteDecimals int) *big.Rat {
	if raw == nil {
		return nil
	}
	if baseDecimals == quoteDecimals {
		return raw
	}
	return new(big.Rat).Mul(raw, DecimalsAdjustment(baseDecimals, quoteDecimals))
}
