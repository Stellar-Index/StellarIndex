package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// Off-chain amount scaling is NOT uniform (AGENTS.md): CEX and
// reference-aggregator sources stamp amounts at 10^8, but the FX pollers
// (ecb / exchangeratesapi / massive) stamp 10^6. The
// per-source authority is external.Registry's AmountDecimals, read via
// Metadata.AmountScaleDecimals() — the USD-volume paths below consult it
// instead of a hard-coded 8, which mis-valued FX trades 100×.

// USDVolumeFXResolver returns the asset's USD price as of `at` as a decimal
// string for [tradeUSDVolume]'s FX tiers. ("", false, nil) means no usable
// anchor; real I/O errors propagate and the trade still inserts with NULL
// usd_volume. Production: [VWAPUSDFXResolver] over `prices_1m`, wired when
// `[trades].usd_pegged_classic_assets` is non-empty. Implementations MUST be
// safe for concurrent USDPriceAt calls: it runs on the trade-insert hot path.
type USDVolumeFXResolver interface {
	USDPriceAt(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error)
}

// tradeUSDVolume returns the per-trade USD-equivalent volume as a
// NUMERIC-compatible string, or nil (stored as NULL) when the trade can't
// be converted cleanly.
//
// Tiers run in order of EXACTNESS: the peg tiers need no market lookup, so
// they always win over the estimated FX tiers.
//
//  1. Off-chain CEX/FX source, quote fiat:USD or an `aggregate.FiatProxy`
//     stablecoin; scaled by the source's AmountScaleDecimals.
//  2. On-chain DEX source, quote USD-pegged per [USDVolumeQuoteSpec]; x7.
//     2b. The BASE leg is USD-pegged (either source class), for
//     `USDC/TOKEN`-oriented markets; see [tradeUSDVolumeViaUSDBase].
//  3. `fxResolver` returns a USD rate for the quote at the trade's
//     timestamp (also tried for off-chain trades that missed tier 1).
//  4. DEX trade whose BASE is XLM or its SAC: base_amount is stroops, so
//     usd_volume = base_amount/1e7 × XLM/USD; see [usdVolumeViaXLMBaseAnchor].
//
// On a DEX trade with an XLM leg on either side, the XLM anchor runs ahead
// of tier 3 (base side first, then [tradeUSDVolumeViaXLMQuoteAnchorFor]).
// Peg tiers trust the peg at insert time; a depeg does not rewrite stored
// values. Anything else stays NULL rather than over-claim USD-equivalence.
// LOCKSTEP: [ClassifyUSDVolumeTier] mirrors this waterfall (legs, order,
// scale); TestClassifyUSDVolumeTier_TracksTheWaterfall fails on drift.
func tradeUSDVolume(ctx context.Context, t canonical.Trade, quoteSpec *USDVolumeQuoteSpec, fxResolver USDVolumeFXResolver) *string {
	v, _ := tradeUSDVolumeChecked(ctx, t, quoteSpec, fxResolver)
	return v
}

// tradeUSDVolumeChecked is [tradeUSDVolume] with a resolver read error
// returned instead of folded into NULL. On error the value is nil and no
// later tier is tried: a rate that could not be read is not a decline, so
// falling through would store a lower tier's estimate in its place.
func tradeUSDVolumeChecked(ctx context.Context, t canonical.Trade, quoteSpec *USDVolumeQuoteSpec, fxResolver USDVolumeFXResolver) (*string, error) {
	q := t.QuoteAmount.BigInt()
	if q == nil || q.Sign() <= 0 {
		return nil, nil
	}
	md := external.Lookup(t.Source)
	if md.Class != external.ClassExchange {
		// Oracles and aggregators don't emit Trades — defensive nil
		// keeps the function honest if a misregistered source ever
		// sneaks one in.
		return nil, nil
	}
	if decimals, ok := usdVolumeDecimals(t.Pair.Quote, md, quoteSpec); ok {
		denom := scaleDenominator(decimals)
		// FloatString(8) gives a fixed-precision decimal — Postgres
		// NUMERIC accepts the form directly with no precision loss
		// for any value that fit in the original big.Int (NUMERIC is
		// arbitrary-precision; FloatString just chooses a render).
		rendered := new(big.Rat).SetFrac(q, denom).FloatString(8)
		return &rendered, nil
	}
	// Tier 2b — the BASE leg is USD-pegged. Same exactness class as
	// tiers 1/2 (a declared peg, no market lookup), so it belongs here,
	// ahead of the estimated FX tiers.
	if v := tradeUSDVolumeViaUSDBase(t, md, quoteSpec); v != nil {
		return v, nil
	}
	// Phase 2 fallback — only when the operator wired an FX
	// resolver. Skip when nil to keep the no-config path on the
	// existing Phase 1 behaviour exactly.
	if fxResolver == nil {
		return nil, nil
	}
	// With XLM as the BASE leg, value the trade off that leg BEFORE pricing the
	// quote. Tier 3's quote rate for an on-chain token is usually the tier-3b
	// `token/XLM x XLM/USD` bridge read from prices_1m, which the counterparty can
	// author: one self-trade set a rate that stored $8,559,224.82 for a trade
	// worth $0.86 (fleet-wide 24h: $21,958,433 usd_volume against $596,699 of XLM
	// legs). XLM is the bridge's anchor, so its rate is a direct market nobody can
	// author. XLM only: a non-XLM base resolves through the same poisonable bridge.
	if isXLMAsset(t.Pair.Base) {
		if v, err := usdVolumeViaXLMBaseAnchor(ctx, t, md.Subclass, fxResolver); err != nil || v != nil {
			return v, err
		}
	}
	// Same anchor when the pool stored XLM as the QUOTE leg: orientation is
	// the pool's token order, so both sides of one economic swap must value alike.
	if isXLMAsset(t.Pair.Quote) && !isXLMAsset(t.Pair.Base) {
		if v, err := tradeUSDVolumeViaXLMQuoteAnchorFor(ctx, t, fxResolver); err != nil || v != nil {
			return v, err
		}
	}
	if v, err := usdVolumeViaFX(ctx, t, md, fxResolver); err != nil || v != nil {
		return v, err
	}
	// Tier 4 (L7.6) — quote-side resolution declined; try the
	// XLM-base anchor before giving up.
	return usdVolumeViaXLMBaseAnchor(ctx, t, md.Subclass, fxResolver)
}

// tradeUSDVolumeViaUSDBase is tier 2b: value the trade off a USD-pegged BASE
// leg, so `USDC/TOKEN`-oriented markets are priced.
//
//	usd_volume = base_amount / 10^decimals
//
// EXACT, the same "trust the declared peg" assumption as tiers 1/2, so it
// wins over a quote-side FX estimate: across 111,617 USDC/XLM-style trades in
// one day the two agreed to 0.69% on average but diverged by up to 134.92%,
// and the error was the VWAP route's. It does not re-check the quote leg:
// [usdVolumeDecimals] already declined it.
func tradeUSDVolumeViaUSDBase(t canonical.Trade, md external.Metadata, quoteSpec *USDVolumeQuoteSpec) *string {
	decimals, ok := usdVolumeDecimals(t.Pair.Base, md, quoteSpec)
	if !ok {
		return nil
	}
	base := t.BaseAmount.BigInt()
	if base == nil || base.Sign() < 0 {
		return nil
	}
	// A zero pegged leg is exactly $0; declining would hand it to an
	// estimated tier and break usd_volume = base_amount / 10^decimals.
	rendered := new(big.Rat).SetFrac(base, scaleDenominator(decimals)).FloatString(8)
	return &rendered
}

// tradeUSDVolumeViaFX is the FX multiplication tier: quote_amount × usdRate /
// 10^decimals, with the per-source scale (CEX 8, FX pollers 6, DEX 7).
// Resolver errors return nil here; the write path uses [usdVolumeViaFX] and
// decides per derive generation whether to fail or store NULL.
func tradeUSDVolumeViaFX(ctx context.Context, t canonical.Trade, md external.Metadata, r USDVolumeFXResolver) *string {
	v, _ := usdVolumeViaFX(ctx, t, md, r)
	return v
}

// usdVolumeViaFX is [tradeUSDVolumeViaFX] with the resolver's read error
// returned rather than folded into a decline, for the restamp tiers.
func usdVolumeViaFX(ctx context.Context, t canonical.Trade, md external.Metadata, r USDVolumeFXResolver) (*string, error) {
	if r == nil {
		return nil, nil
	}
	usdRateStr, ok, err := r.USDPriceAt(ctx, t.Pair.Quote, t.Timestamp)
	if err != nil {
		return nil, err
	}
	if !ok || usdRateStr == "" {
		return nil, nil
	}
	usdRate, ok := new(big.Rat).SetString(usdRateStr)
	if !ok || usdRate.Sign() <= 0 {
		return nil, nil
	}
	var decimals int
	switch md.Subclass {
	case external.SubclassCEX, external.SubclassFX:
		// Per-source registered scale: CEXes stamp 1e8, the FX
		// pollers stamp 1e6 — assuming 8 valued FX trades 100× low/high.
		decimals = md.AmountScaleDecimals()
	case external.SubclassDEX:
		decimals = stellarClassicDecimals
	default:
		return nil, nil
	}
	q := new(big.Rat).SetFrac(t.QuoteAmount.BigInt(), scaleDenominator(decimals))
	usdAmount := new(big.Rat).Mul(q, usdRate)

	// DEX-only: CEX/FX quote rates are vendor feeds, not poisonable
	// bridges, and their pairs' base legs are often unresolvable anyway.
	if md.Subclass == external.SubclassDEX {
		usdAmount, err = boundUSDVolume(ctx, r, usdAmount, t.Pair.Base, t.BaseAmount, decimals, t.Timestamp)
		if err != nil || usdAmount == nil {
			return nil, err
		}
	}
	rendered := usdAmount.FloatString(8)
	return &rendered, nil
}

// boundUSDVolume is the bound every ESTIMATED on-chain tier shares; it
// returns the value to store or nil (usd_volume NULL). `candidate` rests on a
// resolver rate for one leg; `other`/`otherAmount` name the other leg.
//
// LEG CROSS-CHECK: the tier-3b bridge's token leg is writable for
// bridgeLegMinUSDVolume (a planted INDUSX/XLM rate stamped $182M of fake
// usd_volume, real value <$0.01). Valuing the other leg too and storing the
// SMALLER past usdLegAgreementFactor forces an attacker to pump both markets.
// SINGLE-LEG CEILING: with the other leg unresolvable the cross-check cannot
// fire, so a print above singleLegMaxUSDVolume is refused rather than served.
// One function so no tier can take the rate without the bound; never route an
// XLM-anchored value here (see [usdVolumeViaXLMBaseAnchor]).
func boundUSDVolume(ctx context.Context, r USDVolumeFXResolver, candidate *big.Rat, other canonical.Asset, otherAmount canonical.Amount, decimals int, at time.Time) (*big.Rat, error) {
	otherVal, err := fxLegValue(ctx, r, other, otherAmount, decimals, at)
	if err != nil {
		// The cross-check could not run; serving the candidate unchecked
		// would bypass the bound this function exists to enforce.
		return nil, err
	}
	if otherVal == nil {
		if candidate.Cmp(singleLegMaxUSDVolume) > 0 {
			return nil, nil
		}
		return candidate, nil
	}
	hi, lo := candidate, otherVal
	if hi.Cmp(lo) < 0 {
		hi, lo = lo, hi
	}
	// hi > lo * factor → divergent → conservative leg wins.
	bound := new(big.Rat).Mul(lo, usdLegAgreementFactor)
	if hi.Cmp(bound) > 0 && candidate.Cmp(otherVal) > 0 {
		return otherVal, nil
	}
	return candidate, nil
}

// usdLegAgreementFactor is how far the two independently-valued legs of one
// trade may diverge before the FX tier stores the smaller. 10x is far beyond
// honest spread/rounding on any real market while never firing on ordinary
// thin-market noise.
var usdLegAgreementFactor = big.NewRat(10, 1)

// singleLegMaxUSDVolume bounds a DEX trade whose usd_volume rests on ONE
// resolvable, possibly self-authored leg. No real Stellar swap approaches nine
// figures, so the ceiling removes only the implausible tail an attacker can
// drive through a bridge rate and leaves honest unresolvable-base trades alone.
var singleLegMaxUSDVolume = new(big.Rat).SetInt64(100_000_000)

// fxLegValue values one leg of a trade through the resolver: amount /
// 10^decimals x USDPriceAt(asset). nil when the asset has no resolvable
// rate (which keeps the caller on its single-leg behaviour — no
// cross-check is possible). A resolver read error is returned, not nil.
func fxLegValue(ctx context.Context, r USDVolumeFXResolver, asset canonical.Asset, amount canonical.Amount, decimals int, at time.Time) (*big.Rat, error) {
	rateStr, ok, err := r.USDPriceAt(ctx, asset, at)
	if err != nil {
		return nil, err
	}
	if !ok || rateStr == "" {
		return nil, nil
	}
	rate, ok := new(big.Rat).SetString(rateStr)
	if !ok || rate.Sign() <= 0 {
		return nil, nil
	}
	amt := amount.BigInt()
	if amt == nil || amt.Sign() <= 0 {
		return nil, nil
	}
	v := new(big.Rat).SetFrac(amt, scaleDenominator(decimals))
	return v.Mul(v, rate), nil
}

// usdVolumeViaXLMBaseAnchor values a trade off its BASE leg when
// [tradeUSDVolumeViaFX] declined the quote and the base passes
// [baseAnchorEligible]. For an XLM base, base_amount is stroops regardless of
// the quote token's decimals:
//
//	usd_volume = (base_amount / 1e7) × XLM/USD
//
// the write-time twin of [sorobanVolume24hUSDQuery]'s query-time identity, so
// every consumer summing `usd_volume` picks it up, with no double-count there.
// Any on-chain base is eligible (token/token is the largest unpriced class),
// pure SEP-41 included, since the token's scale cancels in the raw-rate
// product.
func usdVolumeViaXLMBaseAnchor(ctx context.Context, t canonical.Trade, subclass external.Subclass, r USDVolumeFXResolver) (*string, error) {
	if r == nil || subclass != external.SubclassDEX {
		// Off-chain sources don't have this orientation problem —
		// externalAmountDecimals is uniform and tier 1 already
		// covers every USD-pegged quote.
		return nil, nil
	}
	if !baseAnchorEligible(t.Pair.Base) {
		return nil, nil
	}
	base := t.BaseAmount.BigInt()
	if base == nil || base.Sign() <= 0 {
		return nil, nil
	}
	// Resolve XLM through its canonical `native` form whichever wire
	// form the pool used: the XLM/USD markets in prices_1m are stored
	// as `native`, so asking for the SAC contract id would miss (and
	// then decline the bridge, since XLM is the bridge's base case).
	// Non-XLM bases resolve as themselves.
	anchor := t.Pair.Base
	if isXLMAsset(anchor) {
		anchor = canonical.NativeAsset()
	}
	usdRateStr, ok, err := r.USDPriceAt(ctx, anchor, t.Timestamp)
	if err != nil {
		return nil, err
	}
	if !ok || usdRateStr == "" {
		return nil, nil
	}
	usdRate, ok := new(big.Rat).SetString(usdRateStr)
	if !ok || usdRate.Sign() <= 0 {
		return nil, nil
	}
	q := new(big.Rat).SetFrac(base, scaleDenominator(stellarClassicDecimals))
	usdAmount := new(big.Rat).Mul(q, usdRate)
	// A non-XLM anchor's rate is the same poisonable tier-3b bridge, so it takes
	// the same bound, or the $182M fake-print class reopens. XLM is exempt: its
	// rate is a direct market and base_amount is XLM that moved, so a ceiling
	// would NULL a real trade. Applied here so the restamp tiers inherit it.
	if !isXLMAsset(t.Pair.Base) {
		usdAmount, err = boundUSDVolume(ctx, r, usdAmount, t.Pair.Quote, t.QuoteAmount, stellarClassicDecimals, t.Timestamp)
		if err != nil || usdAmount == nil {
			return nil, err
		}
	}
	rendered := usdAmount.FloatString(8)
	return &rendered, nil
}

// isXLMAsset reports whether a is native XLM in either on-chain wire
// form — the classic `native` type, or the Stellar Asset Contract
// that wraps it (the form a Soroban pool holds when one leg of its
// liquidity is XLM). Mirrors [canonOrientSQL]'s
// SQL-side check of the same two forms.
func isXLMAsset(a canonical.Asset) bool {
	return a.Type == canonical.AssetNative ||
		(a.Type == canonical.AssetSoroban && a.ContractID == canonical.NativeSACContractID())
}

// ─── the tier-4 scope, as seen from a STORED row ─────────────────────
//
// The helpers below answer for a stored row what [tradeUSDVolume] answers for
// one being inserted. They live here, not beside their caller, so the tier
// definitions and the waterfall's branch order stay in one file (LOCKSTEP),
// and because [external.Registry] is reachable from storage only through this
// grandfathered file (D8 rule 4, scripts/ci/lint-imports.sh).

// dexSourceNames returns the registered source names whose subclass is
// [external.SubclassDEX], sorted — the set whose trades the DEX branches
// of this waterfall (tier 2's operator allow-list, the FX leg
// cross-check, the tier-4 anchor) apply to. Read from the same
// [external.Registry] the insert path consults, so a newly-registered
// on-chain venue is covered the day it lands rather than the day someone
// remembers to widen a hard-coded list.
func dexSourceNames() []string {
	out := make([]string, 0, len(external.Registry))
	for name, md := range external.Registry {
		if md.Subclass == external.SubclassDEX {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// restampTierVerdict says which usd_volume tier owns a stored trade, seen
// from ONE re-derive tier's gate. Shared by [xlmBaseTierFor],
// [xlmQuoteTierFor] and [cexFiatTierFor] so the three cannot disagree
// about what "the exact tier owns this" means.
type restampTierVerdict int

const (
	// restampTierOwns: this tier is the branch [tradeUSDVolume] takes.
	restampTierOwns restampTierVerdict = iota
	// restampTierOutOfScope: the source's subclass or the tier's own leg
	// is not what the tier needs — the tier's value function declines
	// such a trade outright.
	restampTierOutOfScope
	// restampTierPegged: one leg is a declared USD peg, so tier 1/2/2b
	// values the trade EXACTLY and this tier is never reached.
	restampTierPegged
)

// xlmBaseTierFor mirrors [tradeUSDVolume]'s branch order for one stored
// trade: a DEX source, a quote leg [usdVolumeDecimals] does NOT recognise
// as a USD peg, and an XLM base leg is exactly the combination under
// which the waterfall takes the [usdVolumeViaXLMBaseAnchor] branch
// ahead of the quote side. Every gate is the insert path's own call, not
// a restatement of it.
func xlmBaseTierFor(t canonical.Trade, quoteSpec *USDVolumeQuoteSpec) restampTierVerdict {
	md := external.Lookup(t.Source)
	if md.Subclass != external.SubclassDEX || !isXLMAsset(t.Pair.Base) {
		return restampTierOutOfScope
	}
	if _, pegged := usdVolumeDecimals(t.Pair.Quote, md, quoteSpec); pegged {
		return restampTierPegged
	}
	return restampTierOwns
}

// xlmQuoteTierFor is [xlmBaseTierFor]'s mirror: a DEX trade whose QUOTE leg
// is an XLM form and whose BASE is neither an XLM form nor a USD peg. The
// tiers are DISJOINT: XLM on both legs belongs to the xlm-base tier, a pegged
// base to tier 2b; the pegged-quote check stays for an operator who declared
// an XLM form as a peg.
func xlmQuoteTierFor(t canonical.Trade, quoteSpec *USDVolumeQuoteSpec) restampTierVerdict {
	md := external.Lookup(t.Source)
	if md.Subclass != external.SubclassDEX || !isXLMAsset(t.Pair.Quote) || isXLMAsset(t.Pair.Base) {
		return restampTierOutOfScope
	}
	if _, pegged := usdVolumeDecimals(t.Pair.Quote, md, quoteSpec); pegged {
		return restampTierPegged
	}
	if _, pegged := usdVolumeDecimals(t.Pair.Base, md, quoteSpec); pegged {
		return restampTierPegged
	}
	return restampTierOwns
}

// cexFiatTierFor is the off-chain counterpart: a CEX trade quoted in a
// non-USD fiat whose base is not a USD peg. Its rate comes from `fx_quotes`
// ([VWAPUSDFXResolver.usdPriceForFiat]), a vendor feed no counterparty can
// author.
func cexFiatTierFor(t canonical.Trade, quoteSpec *USDVolumeQuoteSpec) restampTierVerdict {
	md := external.Lookup(t.Source)
	if md.Subclass != external.SubclassCEX || t.Pair.Quote.Type != canonical.AssetFiat {
		return restampTierOutOfScope
	}
	if _, pegged := usdVolumeDecimals(t.Pair.Quote, md, quoteSpec); pegged {
		return restampTierPegged
	}
	if _, pegged := usdVolumeDecimals(t.Pair.Base, md, quoteSpec); pegged {
		return restampTierPegged
	}
	return restampTierOwns
}

// cexSourceNames returns the registered [external.SubclassCEX] source names,
// sorted, read from the same [external.Registry] the insert path consults.
// SubclassFX is excluded: fiat/fiat pairs at a 1e6 scale are a different
// population a re-derive should take on deliberately.
func cexSourceNames() []string {
	out := make([]string, 0, len(external.Registry))
	for name, md := range external.Registry {
		if md.Subclass == external.SubclassCEX {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// tradeUSDVolumeViaXLMBaseAnchorFor is [usdVolumeViaXLMBaseAnchor]
// for a STORED row: it resolves the source's subclass itself rather than
// taking it from a caller that would have to look it up the same way. A
// resolver read error is returned so a restamp aborts instead of filing it
// as a decline.
func tradeUSDVolumeViaXLMBaseAnchorFor(ctx context.Context, t canonical.Trade, r USDVolumeFXResolver) (*string, error) {
	return usdVolumeViaXLMBaseAnchor(ctx, t, external.Lookup(t.Source).Subclass, r)
}

// tradeUSDVolumeViaXLMQuoteAnchorFor values a stored row whose XLM leg is the
// QUOTE: `quote_amount / 1e7 x XLM/USD at ts`, by handing the base-side
// anchor the trade with its legs swapped, so a change moves both tiers.
// [tradeUSDVolume] takes this branch ahead of [tradeUSDVolumeViaFX], so a
// re-derive and an insert agree. Deliberately no leg cross-check: the value
// rests on XLM, which nobody can author, so the token leg could only drag it
// down to a counterparty-written rate.
func tradeUSDVolumeViaXLMQuoteAnchorFor(ctx context.Context, t canonical.Trade, r USDVolumeFXResolver) (*string, error) {
	return tradeUSDVolumeViaXLMBaseAnchorFor(ctx, mirrorTradeLegs(t), r)
}

// mirrorTradeLegs returns t with its two legs swapped — base for quote,
// base_amount for quote_amount. Nothing else about the row changes, so
// the mirrored trade is still the same trade to every gate that reads the
// source, the timestamp or the primary key.
func mirrorTradeLegs(t canonical.Trade) canonical.Trade {
	t.Pair = canonical.Pair{Base: t.Pair.Quote, Quote: t.Pair.Base}
	t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
	return t
}

// tradeUSDVolumeViaFiatQuoteFor values a stored CEX row quoted in a non-USD
// fiat through [tradeUSDVolumeViaFX], as [Store.InsertTrade] does, with the
// source's registered scale and the fx_quotes-backed rate. The CEX and fiat
// gates make the DEX-only guards unreachable; other shapes are declined.
func tradeUSDVolumeViaFiatQuoteFor(ctx context.Context, t canonical.Trade, r USDVolumeFXResolver) (*string, error) {
	md := external.Lookup(t.Source)
	if md.Subclass != external.SubclassCEX || t.Pair.Quote.Type != canonical.AssetFiat {
		return nil, nil
	}
	return usdVolumeViaFX(ctx, t, md, r)
}

// baseAnchorEligible reports whether an asset can be valued from a raw-VWAP
// rate by [usdVolumeViaXLMBaseAnchor].
//
// The 1e7 divisor belongs to the asset the RATE is denominated against, not
// the asset being scaled; looking up per-token decimals would INTRODUCE an
// error. prices_1m stores vwap as a raw quote/base ratio, so for A raw units
// at raw rate R = X/A against an anchor:
//
//	usd = (A / 1e7) x R x anchorUSD = (X / 1e7) x anchorUSD
//
// Every anchor is 7-decimal, so pure SEP-41 tokens are eligible
// (TestUSDVolumeIsIndependentOfTokenDecimals). Per-whole-unit prices (pegs,
// oracle quotes) do not cancel, so the peg tiers stay classic + SAC.
func baseAnchorEligible(a canonical.Asset) bool {
	//exhaustive:ignore — the on-chain asset forms are the point; fiat,
	// crypto-ticker and RWA are external-source shapes that never reach
	// a SubclassDEX trade.
	switch a.Type {
	case canonical.AssetNative, canonical.AssetClassic, canonical.AssetSoroban:
		return true
	default:
		return false
	}
}

// stellarClassicDecimals is the smallest-unit-to-display divisor
// power baked into Stellar classic — XLM, native, every classic
// credit. SEP-41 contracts publish their own value via decimals();
// the FX-fallback path here only fires for on-chain DEX trades
// whose Stellar-side quote assets all share this scale.
const stellarClassicDecimals = 7

// WouldPopulateUSDVolume reports whether [Store.InsertTrade] would stamp a
// non-null `usd_volume` under the installed [USDVolumeQuoteSpec] and
// [USDVolumeFXResolver], running the full waterfall (tiers 1, 2, 2b, 3 incl.
// fiat quotes from fx_quotes, 4). An FX-tier hit calls the resolver
// synchronously, so production resolvers must be hot-path cheap.
func (s *Store) WouldPopulateUSDVolume(ctx context.Context, t canonical.Trade) bool {
	return tradeUSDVolume(ctx, t, s.usdVolumeQuoteSpec, s.usdVolumeFXResolver) != nil
}

// usdVolumeDecimals reports whether an asset is USD-pegged and at what scale,
// given the source's subclass and the operator's quote spec. Side-agnostic:
// used for the quote leg (tiers 1/2) and the base leg (tier 2b).
func usdVolumeDecimals(asset canonical.Asset, md external.Metadata, quoteSpec *USDVolumeQuoteSpec) (int, bool) {
	switch md.Subclass {
	case external.SubclassCEX, external.SubclassFX:
		// Off-chain — the SOURCE's registered amount scale (CEXes stamp
		// 1e8 but the FX pollers stamp 1e6, so a hard-coded 8 is a 100×
		// error for FX trades), peg via
		// the crypto-ticker FiatProxy.
		if !quoteIsUSDOrUSDPegged(asset) {
			return 0, false
		}
		return md.AmountScaleDecimals(), true
	case external.SubclassDEX:
		// On-chain (Stellar SDEX, Soroswap, Aquarius, Phoenix,
		// Comet) — peg + decimals come from the operator's
		// USDVolumeQuoteSpec. Phase 1 covers classic + SAC; pure
		// SEP-41 stablecoins are phase 2.
		return quoteSpec.QuoteUSDPegInfo(asset)
	default:
		return 0, false
	}
}

// ClassifyUSDVolumeTier reports which waterfall tier the insert path would use
// for a (source, base, quote) trade today, and that tier's decimal scale. It
// calls the same functions as [tradeUSDVolume] in the same order. An
// unparseable asset yields [TierEstimated] with an error, a finding in itself.
// A nil spec means on-chain pairs never resolve a peg.
func ClassifyUSDVolumeTier(source, baseID, quoteID string, spec *USDVolumeQuoteSpec) (USDVolumeTier, int, error) {
	md := external.Lookup(source)
	if md.Class != external.ClassExchange {
		return TierUnvaluable, 0, nil
	}
	base, err := canonical.ParseAsset(baseID)
	if err != nil {
		return TierEstimated, 0, fmt.Errorf("base asset %q: %w", baseID, err)
	}
	quote, err := canonical.ParseAsset(quoteID)
	if err != nil {
		return TierEstimated, 0, fmt.Errorf("quote asset %q: %w", quoteID, err)
	}
	// Quote leg first — tradeUSDVolume checks it first, and tier 2b is only
	// reached when the quote declined.
	if decimals, ok := usdVolumeDecimals(quote, md, spec); ok {
		return TierQuotePegged, decimals, nil
	}
	if decimals, ok := usdVolumeDecimals(base, md, spec); ok {
		return TierBasePegged, decimals, nil
	}
	return TierEstimated, 0, nil
}

// scaleDenominator returns 10^decimals as a *big.Int. Decimals are
// always small (≤ 18) so the exponent is cheap.
func scaleDenominator(decimals int) *big.Int {
	out := big.NewInt(1)
	ten := big.NewInt(10)
	for range decimals {
		out.Mul(out, ten)
	}
	return out
}

// quoteIsUSDOrUSDPegged is true when the asset is fiat:USD or a
// stablecoin that aggregate.FiatProxy maps to USD. The peg is
// trusted at insert time — depeg events are observed separately
// via the divergence + anomaly paths and do NOT change the inserted
// usd_volume retroactively (a depegged USDT trade still carries
// its observed quote_amount, which is the right historical record).
func quoteIsUSDOrUSDPegged(a canonical.Asset) bool {
	if a.Type == canonical.AssetFiat && a.Code == "USD" {
		return true
	}
	proxy, ok := aggregate.FiatProxy(a)
	if !ok {
		return false
	}
	return proxy.Type == canonical.AssetFiat && proxy.Code == "USD"
}

// ErrNoFXQuote is returned by [Store.FXQuoteAtOrBefore] when no FX
// observation exists for the requested pair at-or-before the cutoff.
// Callers fall back to the cached VWAP path (degraded but functional)
// and surface the fallback via the AggregatorFXSnapFallbackTotal metric.
var ErrNoFXQuote = errors.New("timescale: no FX quote at or before cutoff")

// isDexUnitRatioTrade reports whether a landed on-chain trade has
// base_amount == quote_amount (both nonzero): the signature of a decoder
// field-mapping bug that completeness checks, which verify presence not
// plausibility, cannot see. Counted in InsertTrade and BatchInsertTrades, the
// one choke point every trade path crosses once; the sink would miss the batch
// path. ledger == 0 (off-chain) is excluded because fixed-scale CEX/FX amounts
// carry no such signal; the nonzero check matters because Validate admits one
// zero leg.
func isDexUnitRatioTrade(ledger uint32, base, quote canonical.Amount) bool {
	if ledger == 0 {
		return false
	}
	return !base.IsZero() && base.Cmp(quote) == 0
}

// recordDexTradeUnitRatio bumps obs.DexTradeUnitRatioTotal when t is a
// landed unit-ratio on-chain trade (see isDexUnitRatioTrade). Split out
// of InsertTrade's body so trades_unit_ratio_test.go can exercise the
// record-or-not decision directly, without a live database connection —
// InsertTrade itself can't be unit-tested that way since it round-trips
// through *sql.DB.
func recordDexTradeUnitRatio(t canonical.Trade) {
	if isDexUnitRatioTrade(t.Ledger, t.BaseAmount, t.QuoteAmount) {
		obs.DexTradeUnitRatioTotal.WithLabelValues(t.Source).Inc()
	}
}

// usdPopulatedLabel maps the resolved-or-not decision to the stable Prometheus
// label values coverage dashboards and alerts filter on. Counted here, the
// choke point every trade path crosses once. "unroutable" (two classic assets
// of one issuer) and "thin" (only candidate rate refused by the substance
// gate) are excluded from the coverage ratio; usd_volume stays NULL either
// way.
func usdPopulatedLabel(p canonical.Pair, populated, thin bool) string {
	switch {
	case populated:
		return "yes"
	case p.Base.Type == canonical.AssetClassic && p.Quote.Type == canonical.AssetClassic &&
		p.Base.Issuer != "" && p.Base.Issuer == p.Quote.Issuer:
		return "unroutable"
	case thin:
		return "thin"
	default:
		return "no"
	}
}

// thinMarketReporter is implemented by a resolver that can say its substance
// gate refused a leg's rate ([VWAPUSDFXResolver.ThinMarketRefused]).
type thinMarketReporter interface {
	ThinMarketRefused(asset canonical.Asset, at time.Time) bool
}

// usdLabel is the trade_inserts_total usd_volume_populated value for t given
// its resolved usd_volume v.
func (s *Store) usdLabel(t canonical.Trade, v *string) string {
	thin := false
	if v == nil {
		if rep, ok := s.usdVolumeFXResolver.(thinMarketReporter); ok {
			thin = rep.ThinMarketRefused(t.Pair.Quote, t.Timestamp) || rep.ThinMarketRefused(t.Pair.Base, t.Timestamp)
		}
	}
	return usdPopulatedLabel(t.Pair, v != nil, thin)
}

// InsertTrade validates and writes one trade, returning nil on insert or
// conflict on (source, ledger, tx_hash, op_index, ts).
//
// The conflict is DO UPDATE guarded by `derive_generation <= EXCLUDED`, so an
// equal-or-higher generation OVERWRITES the value columns, usd_volume
// included. A re-derive without the USD-volume resolvers installed would
// overwrite correct values with NULL, hence [InstallUSDVolumeResolution] and
// [Store.reDeriveNullVolumeGuard]. usd_volume comes from [tradeUSDVolume];
// on-chain pegs need [Store.SetUSDVolumeQuoteSpec], everything else is NULL.
func (s *Store) InsertTrade(ctx context.Context, t canonical.Trade) error {
	if err := t.Validate(); err != nil {
		return err
	}
	countZeroLegAdmitted(t)

	// One atomic statement: upsert the trade (DO UPDATE guarded by
	// derive_generation, so a lower generation never reverts a correction), and
	// only for a fresh INSERT (`xmax = 0`) bump source_entry_counts, so a re-walk
	// never inflates the tally. Data-modifying CTEs run though `bump` is
	// unreferenced. The trailing count is 1 for a new row, 0 otherwise, and gates
	// the registry hook, outcome metric and sentinels.
	const q = `
        WITH ins AS (
            -- routed_via, signer and tx_index are post-insert tagger
            -- columns: never list them here (first-wins, see tx_index.go).
            INSERT INTO trades (
                source, ledger, tx_hash, op_index, ts,
                base_asset, quote_asset,
                base_amount, quote_amount, usd_volume,
                maker, taker, derive_generation
            ) VALUES (
                $1, $2, $3, $4, $5,
                $6, $7,
                $8, $9, $10,
                NULLIF($11, ''), NULLIF($12, ''), $13
            )
            ON CONFLICT (source, ledger, tx_hash, op_index, ts) DO UPDATE SET
                base_asset        = EXCLUDED.base_asset,
                quote_asset       = EXCLUDED.quote_asset,
                base_amount       = EXCLUDED.base_amount,
                quote_amount      = EXCLUDED.quote_amount,
                -- W1-flowtradeingest-1 (generation-aware NULL preservation):
                -- at the LIVE / equal generation a transient FX-resolver miss
                -- must NOT regress a populated usd_volume to NULL (the gen-0
                -- double-write race between BatchInsertTrades and InsertTrade),
                -- so preserve the stored value on a NULL incoming. A strictly
                -- HIGHER-generation re-derive is still allowed to write an
                -- honest NULL (the tier-3b de-poisoning case; store.go:147-155).
                usd_volume        = CASE
                                        WHEN EXCLUDED.derive_generation > trades.derive_generation
                                            THEN EXCLUDED.usd_volume
                                        ELSE COALESCE(EXCLUDED.usd_volume, trades.usd_volume)
                                    END,
                maker             = EXCLUDED.maker,
                taker             = EXCLUDED.taker,
                derive_generation = EXCLUDED.derive_generation
              WHERE trades.derive_generation <= EXCLUDED.derive_generation
            RETURNING (xmax = 0) AS inserted
        ), bump AS (
            INSERT INTO source_entry_counts AS sec (source, entry_count, updated_at)
            SELECT $1, count(*) FILTER (WHERE inserted), now() FROM ins
            HAVING count(*) FILTER (WHERE inserted) > 0
            ON CONFLICT (source) DO UPDATE
              SET entry_count = sec.entry_count + EXCLUDED.entry_count,
                  updated_at  = EXCLUDED.updated_at
        )
        SELECT count(*) FILTER (WHERE inserted) FROM ins
    `
	var usdVolume any // sql NULL when nil; pq accepts the *string form too
	v, err := s.resolveUSDVolume(ctx, t)
	if err != nil {
		return err
	}
	if v != nil {
		usdVolume = *v
	}
	obs.TradeInsertsTotal.WithLabelValues(t.Source, s.usdLabel(t, v)).Inc()
	var rowsInserted int64
	if err := s.db.QueryRowContext(ctx, q,
		t.Source, t.Ledger, t.TxHash, t.OpIndex, t.Timestamp.UTC(),
		t.Pair.Base.String(), t.Pair.Quote.String(),
		t.BaseAmount, t.QuoteAmount, usdVolume,
		t.Maker, t.Taker, s.deriveGeneration,
	).Scan(&rowsInserted); err != nil {
		return fmt.Errorf("timescale: InsertTrade: %w", err)
	}

	// Emit per-source outcome metric (new vs duplicate) so operators
	// can detect a cursor-replay / stuck-tip pattern via
	// `rate(stellarindex_trade_insert_outcome_total{outcome="new"}[5m]) == 0`
	// while attempts (TradeInsertsTotal) keep climbing. See
	// obs.TradeInsertOutcomeTotal.
	outcome := "new"
	if rowsInserted == 0 {
		outcome = "duplicate"
	}
	obs.TradeInsertOutcomeTotal.WithLabelValues(t.Source, outcome).Inc()

	// Skip the registry hook when no row landed (rowsInserted = 0 — the
	// conflicting row lost the derive_generation guard on the
	// `ON CONFLICT ... DO UPDATE`), or backfill replays and process
	// restarts that re-encounter stored trades drift observation_count.
	if rowsInserted == 0 {
		return nil
	}

	// Stamp wall-clock when a fresh row actually lands. Pairs with
	// obs.SourceLastEventUnix to detect the stuck-cursor /
	// duplicate-flood pattern: when last_event_unix keeps climbing
	// but last_insert_unix flat-lines, the cursor is processing
	// events that produce only duplicate inserts. See the metric
	// godoc + stellarindex_ingestion_duplicate_flood alert.
	obs.SourceLastInsertUnix.WithLabelValues(t.Source).Set(float64(time.Now().Unix()))

	// Unit-ratio sentinel — see
	// isDexUnitRatioTrade's godoc for why this is gated on "landed"
	// (not every attempt): a replay/backfill re-encountering an
	// already-stored bad trade must not re-inflate the alert.
	recordDexTradeUnitRatio(t)

	// Phase 4 (per migration 0023's docblock): auto-register the
	// classic-asset registry from observed trades. Errors are
	// soft-failures — the trade row is committed, we just log+skip
	// the registry update so the hot path can't be sunk by a
	// registry-side problem. Dedupe-cached so this is a no-op for
	// every asset/issuer after the first touch in the process.
	for _, side := range [2]canonical.Asset{t.Pair.Base, t.Pair.Quote} {
		if regErr := s.registerClassicAssetSeen(ctx, side, t.Ledger, t.Timestamp); regErr != nil {
			// Soft-fail: the trade row is committed. Dedupe-cached, so this logs at
			// most once per (asset, issuer) per process; Debug keeps steady state quiet.
			slog.Default().Debug("timescale: classic-asset registry upsert failed (soft-skip)",
				"asset", side.String(),
				"ledger", t.Ledger,
				"err", regErr,
			)
		}
	}
	return nil
}

// tradeBatchValues builds the multi-row INSERT VALUES placeholder fragments and
// the flat positional-arg slice for BatchInsertTrades. Each row contributes 13
// params (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset,
// base_amount, quote_amount, usd_volume, maker, taker, derive_generation).
func (s *Store) tradeBatchValues(ctx context.Context, insertRows []canonical.Trade) ([]string, []any, error) {
	const colsPerRow = 13
	args := make([]any, 0, len(insertRows)*colsPerRow)
	valuesParts := make([]string, 0, len(insertRows))
	for i, t := range insertRows {
		base := i*colsPerRow + 1
		valuesParts = append(valuesParts, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, NULLIF($%d, ''), NULLIF($%d, ''), $%d)",
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10, base+11, base+12,
		))
		var usdVolume any
		v, err := s.resolveUSDVolume(ctx, t)
		if err != nil {
			return nil, nil, err
		}
		if v != nil {
			usdVolume = *v
		}
		obs.TradeInsertsTotal.WithLabelValues(t.Source, s.usdLabel(t, v)).Inc()
		args = append(args,
			t.Source, t.Ledger, t.TxHash, t.OpIndex, t.Timestamp.UTC(),
			t.Pair.Base.String(), t.Pair.Quote.String(),
			t.BaseAmount, t.QuoteAmount, usdVolume,
			t.Maker, t.Taker, s.deriveGeneration,
		)
	}
	return valuesParts, args, nil
}

// scanBatchTradeOutcome runs the batch INSERT..RETURNING query and folds the
// per-landed-row (xmax=0) result set into the outcome tallies (new + unit-ratio
// per source) and the distinct landed assets — each kept at its highest-ledger
// observation (registerClassicAssetSeen advances last_seen_* via GREATEST) —
// that drive the classic-asset/issuer registry hook the batch path would otherwise
// skip. Split out of BatchInsertTrades for length/complexity.
// emitBatchTradeOutcomeMetrics records the per-source insert-outcome metrics for
// a batch: landed (new) + duplicate (sent-landed) + unit-ratio counts. Split
// out of BatchInsertTrades to keep it under the length budget.
func emitBatchTradeOutcomeMetrics(trades []canonical.Trade, perSourceNew, perSourceUnitRatio map[string]int) {
	// Per-source totals (we know how many we sent per source from the input
	// slice). Duplicates = sent - landed.
	perSourceSent := make(map[string]int, len(perSourceNew))
	for _, t := range trades {
		perSourceSent[t.Source]++
	}
	now := float64(time.Now().Unix())
	for source, sent := range perSourceSent {
		landed := perSourceNew[source]
		duplicate := sent - landed
		if landed > 0 {
			obs.TradeInsertOutcomeTotal.WithLabelValues(source, "new").Add(float64(landed))
			obs.SourceLastInsertUnix.WithLabelValues(source).Set(now)
		}
		if duplicate > 0 {
			obs.TradeInsertOutcomeTotal.WithLabelValues(source, "duplicate").Add(float64(duplicate))
		}
	}
	for source, n := range perSourceUnitRatio {
		if n > 0 {
			obs.DexTradeUnitRatioTotal.WithLabelValues(source).Add(float64(n))
		}
	}
}

// The statement runs in its own transaction. A re-derive store
// (deriveGeneration > 0) lifts the TimescaleDB decompression cap with SET
// LOCAL: its upserts into compressed chunks decompress whole segments per
// conflict and would otherwise fail with SQLSTATE 53400. Live ingest keeps
// the server cap, so a runaway decompression there surfaces as a 53400
// retry instead of proceeding unbounded across concurrent persist workers.
func (s *Store) scanBatchTradeOutcome(ctx context.Context, query string, args []any) (
	perSourceNew, perSourceUnitRatio map[string]int,
	seenAssets map[string]registryObservation, err error,
) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("timescale: BatchInsertTrades begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if s.deriveGeneration > 0 {
		if _, err := tx.ExecContext(ctx, batchTradeDecompressionCapSQL); err != nil {
			return nil, nil, nil, fmt.Errorf("timescale: BatchInsertTrades: decompression cap: %w", err)
		}
	}
	perSourceNew, perSourceUnitRatio, seenAssets, err = scanBatchTradeRows(ctx, tx, query, args)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("timescale: BatchInsertTrades commit: %w", err)
	}
	committed = true
	return perSourceNew, perSourceUnitRatio, seenAssets, nil
}

// batchTradeDecompressionCapSQL lifts the per-transaction cap on tuples a
// DML statement may decompress (0 = unbounded), for a re-derive batch
// upsert's transaction only. See scanBatchTradeOutcome.
const batchTradeDecompressionCapSQL = "SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0"

// scanBatchTradeRows runs the batch statement on tx and folds its RETURNING
// rows into the outcome tallies. Split from scanBatchTradeOutcome so the
// transaction bookkeeping and the row fold stay readable on their own.
func scanBatchTradeRows(ctx context.Context, tx *sql.Tx, query string, args []any) (
	perSourceNew, perSourceUnitRatio map[string]int,
	seenAssets map[string]registryObservation, err error,
) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("timescale: BatchInsertTrades: %w", err)
	}
	defer func() { _ = rows.Close() }()

	perSourceNew = make(map[string]int, 4)
	perSourceUnitRatio = make(map[string]int, 4)
	seenAssets = make(map[string]registryObservation, 8)
	for rows.Next() {
		var source, baseAsset, quoteAsset string
		var ledger uint32
		var ts time.Time
		var isUnitRatio bool
		if err := rows.Scan(&source, &ledger, &ts, &baseAsset, &quoteAsset, &isUnitRatio); err != nil {
			return nil, nil, nil, fmt.Errorf("timescale: BatchInsertTrades scan: %w", err)
		}
		perSourceNew[source]++
		if isUnitRatio {
			perSourceUnitRatio[source]++
		}
		noteRegistryObservation(seenAssets, baseAsset, newRegistryObservation(ledger, ts))
		noteRegistryObservation(seenAssets, quoteAsset, newRegistryObservation(ledger, ts))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("timescale: BatchInsertTrades rows.Err: %w", err)
	}
	return perSourceNew, perSourceUnitRatio, seenAssets, nil
}

// tradeInsertMaxRows is the largest sub-batch one multi-row INSERT may
// carry: 65,535 bind parameters is the extended-protocol ceiling, 13
// parameters per row, and a round number below the 5,041 that gives.
const tradeInsertMaxRows = 5000

// tradeInsertChunkBounds splits n rows into [start, end) sub-batches of at
// most tradeInsertMaxRows. n <= 0 yields no chunks.
func tradeInsertChunkBounds(n int) [][2]int {
	var out [][2]int
	for start := 0; start < n; start += tradeInsertMaxRows {
		end := start + tradeInsertMaxRows
		if end > n {
			end = n
		}
		out = append(out, [2]int{start, end})
	}
	return out
}

// insertTradeRows sends one parameter-safe sub-batch (see
// BatchInsertTrades) and returns its landed-row outcome.
func (s *Store) insertTradeRows(ctx context.Context, insertRows []canonical.Trade) (
	perSourceNew, perSourceUnitRatio map[string]int,
	seenAssets map[string]registryObservation, err error,
) {
	// Build VALUES placeholders + args slice (13 params/row incl.
	// derive_generation).
	valuesParts, args, err := s.tradeBatchValues(ctx, insertRows)
	if err != nil {
		return nil, nil, nil, err
	}

	// ins: multi-row INSERT RETURNING each landed row; bump: per-source UPSERT
	// into source_entry_counts, the multi-row twin of InsertTrade's. The outer
	// `unit_ratio` FILTER mirrors isDexUnitRatioTrade over the same landed rows;
	// keep the two predicates in sync.
	//nolint:gosec // G201: VALUES placeholders constructed only from compile-time format string.
	query := fmt.Sprintf(`
        WITH ins AS (
            -- routed_via, signer and tx_index are post-insert tagger
            -- columns: never list them here (first-wins, see tx_index.go).
            INSERT INTO trades (
                source, ledger, tx_hash, op_index, ts,
                base_asset, quote_asset,
                base_amount, quote_amount, usd_volume,
                maker, taker, derive_generation
            ) VALUES %s
            ON CONFLICT (source, ledger, tx_hash, op_index, ts) DO UPDATE SET
                base_asset        = EXCLUDED.base_asset,
                quote_asset       = EXCLUDED.quote_asset,
                base_amount       = EXCLUDED.base_amount,
                quote_amount      = EXCLUDED.quote_amount,
                -- W1-flowtradeingest-1 (generation-aware NULL preservation):
                -- at the LIVE / equal generation a transient FX-resolver miss
                -- must NOT regress a populated usd_volume to NULL (the gen-0
                -- double-write race between BatchInsertTrades and InsertTrade),
                -- so preserve the stored value on a NULL incoming. A strictly
                -- HIGHER-generation re-derive is still allowed to write an
                -- honest NULL (the tier-3b de-poisoning case; store.go:147-155).
                usd_volume        = CASE
                                        WHEN EXCLUDED.derive_generation > trades.derive_generation
                                            THEN EXCLUDED.usd_volume
                                        ELSE COALESCE(EXCLUDED.usd_volume, trades.usd_volume)
                                    END,
                maker             = EXCLUDED.maker,
                taker             = EXCLUDED.taker,
                derive_generation = EXCLUDED.derive_generation
              WHERE trades.derive_generation <= EXCLUDED.derive_generation
            RETURNING (xmax = 0) AS inserted, source, ledger, ts,
                      base_asset, quote_asset, base_amount, quote_amount
        ), bump AS (
            INSERT INTO source_entry_counts AS sec (source, entry_count, updated_at)
            -- ORDER BY source: mixed-source batches row-lock one
            -- source_entry_counts row per source; deterministic order
            -- prevents cross-batch AB/BA deadlocks; sorting the trades
            -- rows alone does not cover this second lock resource.
            -- count(*) FILTER (WHERE inserted): a re-derive UPDATE (migration
            -- 0109) must NOT inflate the per-source tally, so
            -- only genuinely-inserted rows (xmax = 0) count — the
            -- multi-source twin of InsertTrade's landed-only bump gate.
            SELECT source, count(*) FILTER (WHERE inserted), now() FROM ins
              GROUP BY source
              HAVING count(*) FILTER (WHERE inserted) > 0
              ORDER BY source
            ON CONFLICT (source) DO UPDATE
              SET entry_count = sec.entry_count + EXCLUDED.entry_count,
                  updated_at  = EXCLUDED.updated_at
        )
        -- Per-LANDED-row projection (xmax = 0 only). The old aggregate
        -- returned per-source counts; we now return one row per genuinely-
        -- inserted trade so the caller can (a) tally new/unit-ratio in Go and
        -- (b) drive the classic-asset/issuer registry hook off the SAME
        -- landed set the single-row InsertTrade path uses. is_unit_ratio
        -- keeps the numeric comparison in SQL (ledger already gated non-zero,
        -- and WHERE inserted already gates landed) to avoid Go-side decimal
        -- equality drift.
        SELECT source, ledger, ts, base_asset, quote_asset,
               (ledger <> 0 AND base_amount = quote_amount AND base_amount <> 0) AS is_unit_ratio
          FROM ins WHERE inserted
    `, strings.Join(valuesParts, ", "))

	return s.scanBatchTradeOutcome(ctx, query, args)
}

// BatchInsertTrades writes trades with one multi-row upsert per sub-batch of
// at most [tradeInsertMaxRows] rows: per-INSERT roundtrip latency caps indexer
// throughput. Conflict semantics match [Store.InsertTrade]; re-runs are
// idempotent in row count but not inert in value.
//
// [Store.filterStorableTrades] drops invalid rows first so one cannot sink an
// all-or-nothing sub-batch. Sub-batches commit separately in conflict-key
// order and stop at the first failure; the committed prefix still gets its
// metrics and registry hook (a replay would see updates). The error is a
// [*TradeSubBatchError] wrapping the cause; callers replay the whole batch.
func (s *Store) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	if len(trades) == 0 {
		return nil
	}

	// Drop invalid rows BEFORE building the all-or-nothing sub-batch
	// INSERTs (see the godoc). When the whole batch is unstorable this
	// returns early — there is nothing to insert, and the skips are already
	// accounted for inside the filter.
	storable := s.filterStorableTrades(trades)
	if len(storable) == 0 {
		return nil
	}

	// Sort by the FULL conflict key, ts included, so every writer takes row locks
	// in one tie-free order: PersistWorkers fan one channel to 8 unsharded
	// goroutines and CEX reconnects redeliver trades, so overlapping batches
	// otherwise deadlock (40P01). Done here so every caller gets it; the per-row
	// fallback in internal/pipeline/trade_sink.go stays as belt-and-braces.
	// Clone first: filterStorableTrades may return the caller's slice, which
	// callers replay in their own order on error.
	storable = slices.Clone(storable)
	sortTradesByConflictKey(storable)

	// Postgres rejects an INSERT..ON CONFLICT DO UPDATE that presents one key
	// twice, and a CEX redelivery can put a key in one batch twice, so collapse
	// adjacent duplicates, keeping the latest. `trades` stays intact so the
	// "sent" tally still counts the duplicate.
	insertRows := dedupeSortedTradesByConflictKey(storable)

	// Postgres' extended protocol caps one statement at 65,535 bind
	// parameters; at 13 per row that is 5,041 rows, so a 100,000-row batch
	// (the bulk backfill's fallback size) fails outright and drops to one
	// INSERT per row. The batch is sent in parameter-safe sub-batches and the
	// outcome of every COMMITTED sub-batch is tallied, so the metrics and the
	// registry hook cover exactly the rows that landed, even when a later
	// sub-batch fails (see the godoc).
	out, insErr := s.insertTradeSubBatches(ctx, insertRows)
	sent := storable
	if insErr != nil {
		sent = insertRows[:out.committedRows]
	}
	emitBatchTradeOutcomeMetrics(sent, out.perSourceNew, out.perSourceUnitRatio)
	s.registerBatchLandedAssets(ctx, out.seenAssets)
	return insErr
}

// batchTradeOutcome is the landed-row outcome of the committed sub-batches
// of one BatchInsertTrades call.
type batchTradeOutcome struct {
	perSourceNew, perSourceUnitRatio map[string]int
	seenAssets                       map[string]registryObservation
	committedRows                    int
}

// TradeSubBatchError is a BatchInsertTrades failure: rows [0, Start) of the
// Total conflict-key-sorted, deduplicated rows committed, sub-batch
// [Start, End) failed with Err, and nothing from Start on was written.
type TradeSubBatchError struct {
	Start, End, Total       int
	FirstLedger, LastLedger uint32
	Err                     error
}

func (e *TradeSubBatchError) Error() string {
	return fmt.Sprintf("timescale: BatchInsertTrades sub-batch rows [%d,%d) of %d (ledgers %d-%d; %d rows before it committed): %v",
		e.Start, e.End, e.Total, e.FirstLedger, e.LastLedger, e.Start, e.Err)
}

func (e *TradeSubBatchError) Unwrap() error { return e.Err }

// insertTradeSubBatches sends insertRows in parameter-safe sub-batches, in
// order, stopping at the first failure. The outcome covers every sub-batch
// that committed, including when the error is non-nil.
func (s *Store) insertTradeSubBatches(ctx context.Context, insertRows []canonical.Trade) (batchTradeOutcome, error) {
	out := batchTradeOutcome{
		perSourceNew:       map[string]int{},
		perSourceUnitRatio: map[string]int{},
		seenAssets:         map[string]registryObservation{},
	}
	for _, b := range tradeInsertChunkBounds(len(insertRows)) {
		chunk := insertRows[b[0]:b[1]]
		n, u, seen, err := s.insertTradeRows(ctx, chunk)
		if err != nil {
			return out, &TradeSubBatchError{
				Start: b[0], End: b[1], Total: len(insertRows),
				FirstLedger: chunk[0].Ledger, LastLedger: chunk[len(chunk)-1].Ledger,
				Err: err,
			}
		}
		for k, v := range n {
			out.perSourceNew[k] += v
		}
		for k, v := range u {
			out.perSourceUnitRatio[k] += v
		}
		for k, v := range seen {
			noteRegistryObservation(out.seenAssets, k, v)
		}
		out.committedRows = b[1]
	}
	return out, nil
}

// noteRegistryObservation folds o into asset's observation, keeping the
// lowest and highest ledger separately — the one rule for folding landed rows
// and merging sub-batches alike.
func noteRegistryObservation(seen map[string]registryObservation, asset string, o registryObservation) {
	prev, ok := seen[asset]
	if !ok {
		seen[asset] = o
		return
	}
	if o.minLedger < prev.minLedger {
		prev.minLedger, prev.minTs = o.minLedger, o.minTs
	}
	if o.maxLedger > prev.maxLedger {
		prev.maxLedger, prev.maxTs = o.maxLedger, o.maxTs
	}
	seen[asset] = prev
}

// registerBatchLandedAssets runs the classic-asset registry hook for the
// distinct assets of a batch's landed rows.
func (s *Store) registerBatchLandedAssets(ctx context.Context, seenAssets map[string]registryObservation) {
	// Register classic assets/issuers from the LANDED rows, as InsertTrade does:
	// the live indexer ingests only through this path, so skipping it would leave
	// the registry under-populated. Soft-fail and dedupe-cached.
	for assetID, obsv := range seenAssets {
		asset, perr := canonical.ParseAsset(assetID)
		if perr != nil {
			// A stored asset string that won't re-parse is a decoder-side
			// invariant break, not a batch failure — the trades already
			// committed. Breadcrumb at debug (same posture as the hook's own
			// soft-fail) and skip this asset.
			slog.Default().Debug("timescale: batch registry asset parse failed (soft-skip)",
				"asset", assetID, "err", perr)
			continue
		}
		if regErr := s.registerClassicAssetRange(ctx, asset, obsv); regErr != nil {
			slog.Default().Debug("timescale: batch classic-asset registry upsert failed (soft-skip)",
				"asset", assetID, "ledger", obsv.maxLedger, "err", regErr)
		}
	}
}

// filterStorableTrades returns the rows passing [canonical.Trade.Validate],
// so one malformed row cannot sink an all-or-nothing sub-batch. Dropped rows
// stay loud (SourceInsertErrorsTotal + ERROR). An SDEX one-side-zero fill
// passes and is counted on [obs.TradesZeroLegAdmittedTotal]. An all-valid
// batch returns the input slice without allocating.
func (s *Store) filterStorableTrades(trades []canonical.Trade) []canonical.Trade {
	firstBad := -1
	for i := range trades {
		if trades[i].Validate() != nil {
			firstBad = i
			break
		}
		countZeroLegAdmitted(trades[i])
	}
	if firstBad == -1 {
		return trades
	}
	storable := make([]canonical.Trade, firstBad, len(trades))
	copy(storable, trades[:firstBad])
	for _, t := range trades[firstBad:] {
		err := t.Validate()
		if err == nil {
			countZeroLegAdmitted(t)
			storable = append(storable, t)
			continue
		}
		obs.SourceInsertErrorsTotal.WithLabelValues(t.Source, obs.InsertErrorKindTradeDropped).Inc()
		slog.Default().Error("timescale: batch dropped invalid trade before insert",
			"source", t.Source, "ledger", t.Ledger, "tx_hash", t.TxHash, "op_index", t.OpIndex, "err", err)
	}
	return storable
}

// IsOneSideZeroFill reports whether t is the SDEX rounding artifact where
// exactly one leg rounded to 0 while the other stayed positive. Such a trade
// is stored but unpriceable. A both-zero atom is dropped in the decoder, and
// a negative leg is never a valid Stellar amount, so neither qualifies.
func IsOneSideZeroFill(t canonical.Trade) bool {
	bs, qs := t.BaseAmount.Sign(), t.QuoteAmount.Sign()
	return bs >= 0 && qs >= 0 && (bs == 0) != (qs == 0)
}

// countZeroLegAdmitted bumps [obs.TradesZeroLegAdmittedTotal] for a trade
// that passed Validate with exactly one zero leg. Called only after Validate,
// at each Go write gate, so the counter reads "stored but unpriceable".
func countZeroLegAdmitted(t canonical.Trade) {
	if IsOneSideZeroFill(t) {
		obs.TradesZeroLegAdmittedTotal.WithLabelValues(t.Source).Inc()
	}
}

// registryObservation is the lowest and highest ledger observation of a
// landed asset within one BatchInsertTrades call — the input to the
// batch-path classic-asset/issuer registry hook. Both ends are carried so a
// backfill batch offers the true first-seen minimum, not just its tip.
type registryObservation struct {
	minLedger, maxLedger uint32
	minTs, maxTs         time.Time
}

func newRegistryObservation(ledger uint32, ts time.Time) registryObservation {
	return registryObservation{minLedger: ledger, maxLedger: ledger, minTs: ts, maxTs: ts}
}

// A market has NO stored direction: the SDEX decoder sets base = soldAsset,
// so one market lands as both (A,B) and (B,A) rows, and a read keyed on one
// direction silently returns nothing for the other. The readers below select
// both directions and re-express flipped rows per ROW ([orientTradeTo]).
//
// TWO LIMITED ARMS, not one OR'd scan, so each keeps its index-ordered scan
// (trades_pair_ts_idx / trades_pair_source_ts_idx) and early stop, at twice
// the single-direction cost. Aggregates fold the same way because each is
// defined on the two integer leg amounts (docs/architecture/aggregation-plan.md
// §"The direction fold"). [Store.TradesInRangeAfter] stays one-directional
// (its caller merges under a keyset cursor) and is the sole [directionExempt].

// maxLatestTradesForPair caps [Store.LatestTradesForPair]: each arm is a
// time-unbounded walk, so an unclamped limit could materialise the market.
const maxLatestTradesForPair = 1000

// LatestTradesForPair returns up to `limit` most-recent trades for the market,
// in either stored direction, oriented as requested; empty slice + nil if
// none. Each arm keeps rows tying its cut on (ts, ledger) so the outer sort
// breaks ties on /v1/history's full key. Unbounded in time for the reasons in
// [Store.LatestTradePerSource]: this backs /v1/price's last-trade arm, and a
// window would stop pricing a quiet market. Zero-leg rows are excluded;
// `limit` is clamped to [maxLatestTradesForPair].
func (s *Store) LatestTradesForPair(ctx context.Context, p canonical.Pair, limit int) ([]canonical.Trade, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maxLatestTradesForPair {
		limit = maxLatestTradesForPair
	}
	const q = `
        (SELECT source, ledger, tx_hash, op_index, ts,
                base_asset, quote_asset,
                base_amount, quote_amount,
                COALESCE(maker, '')      AS maker,
                COALESCE(taker, '')      AS taker,
                COALESCE(routed_via, '') AS routed_via
           FROM trades
          WHERE base_asset  = $1
            AND quote_asset = $2
            AND base_amount > 0 AND quote_amount > 0
          ORDER BY ts DESC, ledger DESC
          FETCH FIRST $3 ROWS WITH TIES)
        UNION ALL
        (SELECT source, ledger, tx_hash, op_index, ts,
                base_asset, quote_asset,
                base_amount, quote_amount,
                COALESCE(maker, '')      AS maker,
                COALESCE(taker, '')      AS taker,
                COALESCE(routed_via, '') AS routed_via
           FROM trades
          WHERE base_asset  = $2
            AND quote_asset = $1
            AND base_amount > 0 AND quote_amount > 0
          ORDER BY ts DESC, ledger DESC
          FETCH FIRST $3 ROWS WITH TIES)
        ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
        LIMIT $3
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestTradesForPair: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []canonical.Trade
	for rows.Next() {
		t, err := scanTradeOriented(rows, p, "LatestTradesForPair")
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestTradesForPair rows: %w", err)
	}
	return out, nil
}

// offChainSourcesArg is the comma-joined set of registered sources that
// are not on-chain (centralised exchanges, FX, aggregators, vendors),
// bound as `string_to_array($n, ',')` by the raw-trade readers behind
// /v1/history and /v1/observations. Those routes serve on-chain trades
// only (exchange redistribution terms), and the filter must sit in the
// query: a post-fetch drop would break the keyset page sizes.
func offChainSourcesArg() string {
	var names []string
	for name := range external.Registry {
		if !external.IsOnChain(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// LatestTradePerSource returns each source's most-recent trade on the market,
// in either stored direction, oriented as requested; sourceFilter "" means all.
//
// Each direction runs DISTINCT ON (source) by ts, ledger DESC (a skip scan
// over trades_pair_source_ts_idx); a LATERAL picks within the ledger by
// (tx_hash, op_index), and a source seen both ways is folded by
// [tradeIsLaterInMarket]. No time bound: each arm is an index seek (worst
// case measured under 300 ms) and quiet networks (futurenet: zero XLM trades
// in 14 days) would otherwise report nothing.
// [TestRawTradeReadsSpanBothStoredDirections] and
// [TestLatestTradeReadsTakeNoRecencyBound] pin this.
// unbounded-latest-ok: "last trade seen" has no window that preserves it; TestLatestTradeReadsTakeNoRecencyBound pins it.
func (s *Store) LatestTradePerSource(ctx context.Context, p canonical.Pair, sourceFilter string) ([]canonical.Trade, error) {
	const q = `
        (SELECT t.source, t.ledger, t.tx_hash, t.op_index, t.ts,
                t.base_asset, t.quote_asset,
                t.base_amount, t.quote_amount,
                COALESCE(t.maker, '')      AS maker,
                COALESCE(t.taker, '')      AS taker,
                COALESCE(t.routed_via, '') AS routed_via
           FROM (SELECT DISTINCT ON (source) source, ts, ledger
                   FROM trades
                  WHERE base_asset  = $1
                    AND quote_asset = $2
                    AND ($3 = '' OR source = $3)
                    AND source <> ALL(string_to_array($4, ','))
                  ORDER BY source, ts DESC, ledger DESC) head
          CROSS JOIN LATERAL
                (SELECT * FROM trades
                  WHERE base_asset  = $1
                    AND quote_asset = $2
                    AND source = head.source
                    AND ts     = head.ts
                    AND ledger = head.ledger
                  ORDER BY tx_hash DESC, op_index DESC
                  LIMIT 1) t)
        UNION ALL
        (SELECT t.source, t.ledger, t.tx_hash, t.op_index, t.ts,
                t.base_asset, t.quote_asset,
                t.base_amount, t.quote_amount,
                COALESCE(t.maker, '')      AS maker,
                COALESCE(t.taker, '')      AS taker,
                COALESCE(t.routed_via, '') AS routed_via
           FROM (SELECT DISTINCT ON (source) source, ts, ledger
                   FROM trades
                  WHERE base_asset  = $2
                    AND quote_asset = $1
                    AND ($3 = '' OR source = $3)
                    AND source <> ALL(string_to_array($4, ','))
                  ORDER BY source, ts DESC, ledger DESC) head
          CROSS JOIN LATERAL
                (SELECT * FROM trades
                  WHERE base_asset  = $2
                    AND quote_asset = $1
                    AND source = head.source
                    AND ts     = head.ts
                    AND ledger = head.ledger
                  ORDER BY tx_hash DESC, op_index DESC
                  LIMIT 1) t)
        ORDER BY source
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(), sourceFilter, offChainSourcesArg(),
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestTradePerSource: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []canonical.Trade
	bySource := map[string]int{}
	for rows.Next() {
		t, err := scanTradeOriented(rows, p, "LatestTradePerSource")
		if err != nil {
			return nil, err
		}
		if i, ok := bySource[t.Source]; ok {
			if tradeIsLaterInMarket(t, out[i]) {
				out[i] = t
			}
			continue
		}
		bySource[t.Source] = len(out)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestTradePerSource rows: %w", err)
	}
	return out, nil
}

// scanTradeOriented scans one row of the latest-trade projection, rebuilds
// its Pair through the canonical parse path and re-expresses it in `want`'s
// orientation; `who` names the caller for the wrapped error.
func scanTradeOriented(rows *sql.Rows, want canonical.Pair, who string) (canonical.Trade, error) {
	var t canonical.Trade
	var baseAsset, quoteAsset string
	if err := rows.Scan(
		&t.Source, &t.Ledger, &t.TxHash, &t.OpIndex, &t.Timestamp,
		&baseAsset, &quoteAsset,
		&t.BaseAmount, &t.QuoteAmount,
		&t.Maker, &t.Taker, &t.RoutedVia,
	); err != nil {
		return canonical.Trade{}, fmt.Errorf("timescale: %s scan: %w", who, err)
	}
	base, err := canonical.ParseAsset(baseAsset)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("timescale: %s base %q: %w", who, baseAsset, err)
	}
	quote, err := canonical.ParseAsset(quoteAsset)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("timescale: %s quote %q: %w", who, quoteAsset, err)
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("timescale: %s pair: %w", who, err)
	}
	t.Pair = pair
	return orientTradeTo(t, want), nil
}

// orientTradeTo re-expresses one stored trade in `want`'s orientation by
// swapping legs and amounts ([canonical.Orient]). The swap divides nothing, so
// price inverts exactly downstream; inverting by division would break
// ADR-0003. A row in neither orientation is returned as it came.
func orientTradeTo(t canonical.Trade, want canonical.Pair) canonical.Trade {
	if !t.Pair.Equal(want.Flip()) {
		return t
	}
	t.Pair = want
	t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
	return t
}

// tradeIsLaterInMarket reports whether `a` is later than `b` on
// (ts, ledger, tx_hash, op_index), /v1/history's order minus `source`, whose
// Go byte order need not match the database collation.
func tradeIsLaterInMarket(a, b canonical.Trade) bool {
	switch {
	case !a.Timestamp.Equal(b.Timestamp):
		return a.Timestamp.After(b.Timestamp)
	case a.Ledger != b.Ledger:
		return a.Ledger > b.Ledger
	case a.TxHash != b.TxHash:
		return a.TxHash > b.TxHash
	default:
		return a.OpIndex > b.OpIndex
	}
}

// MaxTradesInRangeLimit is the hard ceiling [Store.TradesInRange] clamps its
// limit to. Config validation refuses a max_trades_per_window above it,
// because a silent clamp does double damage: the scan does not widen AND the
// orchestrator's truncation detector (len(t) >= cfg.MaxTradesPerWindow) can
// never fire, so a ~48%-of-windows truncation rate would read as 0%.
const MaxTradesInRangeLimit = 10000

// TradesInRange returns the market's trades with ts in [from, to), in either
// stored direction, oriented as requested and ordered (ts, ledger) ASC.
//
// It feeds /v1/vwap, /v1/twap, single-bar /v1/ohlc, /v1/price/tip and the
// orchestrator. One direction alone is biased: one hour of native/USDC held
// 2957 rows one way and 2794 the other, and the served high was 0.65% low.
// The per-row leg swap suffices because every downstream aggregate is defined
// on the integer amounts. Two limited arms keep the NEWEST rows, so
// `len(rows) == limit` still signals truncation. limit <= 0 means 1000,
// clamped to [MaxTradesInRangeLimit].
func (s *Store) TradesInRange(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	if to.Before(from) {
		return nil, fmt.Errorf("timescale: TradesInRange: to %v < from %v", to, from)
	}
	// Order DESC so the LIMIT keeps the NEWEST rows when the window has
	// more than `limit` trades, then reverse to ascending below. The
	// previous `ORDER BY ts ASC LIMIT` kept the OLDEST `limit` rows, so a
	// busy 1h/24h VWAP was computed from a stale slice that began at the
	// window start and stopped ~limit trades later, never reaching the
	// present. Callers still receive ascending order; only which
	// rows survive truncation changed (newest, not oldest).
	const q = `
        (SELECT source, ledger, tx_hash, op_index, ts,
                base_asset, quote_asset,
                base_amount, quote_amount,
                COALESCE(maker, '')      AS maker,
                COALESCE(taker, '')      AS taker,
                COALESCE(routed_via, '') AS routed_via
           FROM trades
          WHERE base_asset  = $1
            AND quote_asset = $2
            AND ts         >= $3
            AND ts          < $4
          ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
          LIMIT $5)
        UNION ALL
        (SELECT source, ledger, tx_hash, op_index, ts,
                base_asset, quote_asset,
                base_amount, quote_amount,
                COALESCE(maker, '')      AS maker,
                COALESCE(taker, '')      AS taker,
                COALESCE(routed_via, '') AS routed_via
           FROM trades
          WHERE base_asset  = $2
            AND quote_asset = $1
            AND ts         >= $3
            AND ts          < $4
          ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
          LIMIT $5)
        ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
        LIMIT $5
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(),
		from.UTC(), to.UTC(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: TradesInRange: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []canonical.Trade
	for rows.Next() {
		t, err := scanTradeOriented(rows, p, "TradesInRange")
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TradesInRange rows: %w", err)
	}
	// Scanned newest-first (see the DESC query above); reverse to the
	// ascending order this method's contract promises its callers.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// TradesInRangeAfter is TradesInRange with a full-PK cursor: rows whose
// (ts, ledger, tx_hash, op_index, source) is strictly greater than `after*`.
// The PK, not (ts, ledger), because several trades can share (ts, ledger).
// A zero afterTs disables the cursor.
func (s *Store) TradesInRangeAfter(
	ctx context.Context,
	p canonical.Pair,
	from, to, afterTs time.Time,
	afterLedger uint32,
	afterTxHash, afterSource string,
	afterOpIndex uint32,
	limit int,
) ([]canonical.Trade, error) {
	return s.tradesInRangeAfter(ctx, p, "", from, to, afterTs,
		afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
}

// TradesInRangeAfterFromSource is [Store.TradesInRangeAfter] restricted to
// rows written by one source. An empty source is the unfiltered read.
func (s *Store) TradesInRangeAfterFromSource(
	ctx context.Context,
	p canonical.Pair,
	source string,
	from, to, afterTs time.Time,
	afterLedger uint32,
	afterTxHash, afterSource string,
	afterOpIndex uint32,
	limit int,
) ([]canonical.Trade, error) {
	return s.tradesInRangeAfter(ctx, p, source, from, to, afterTs,
		afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
}

func (s *Store) tradesInRangeAfter(
	ctx context.Context,
	p canonical.Pair,
	source string,
	from, to, afterTs time.Time,
	afterLedger uint32,
	afterTxHash, afterSource string,
	afterOpIndex uint32,
	limit int,
) ([]canonical.Trade, error) {
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	if to.Before(from) {
		return nil, fmt.Errorf("timescale: TradesInRangeAfter: to %v < from %v", to, from)
	}
	// ORDER BY and WHERE use the same PK column order so the comparison is
	// monotonic with the sort. The signature lists afterSource before
	// afterOpIndex but the binding below follows PK order; reorder both together.
	const q = `
        SELECT source, ledger, tx_hash, op_index, ts,
               base_asset, quote_asset,
               base_amount, quote_amount,
               COALESCE(maker, ''), COALESCE(taker, ''),
               COALESCE(routed_via, '')
          FROM trades
         WHERE base_asset  = $1
           AND quote_asset = $2
           AND ts         >= $3
           AND ts          < $4
           AND source <> ALL(string_to_array($11, ','))
           AND (ts, ledger, tx_hash, op_index, source) > ($5, $6, $7, $8, $9)
         ORDER BY ts ASC, ledger ASC, tx_hash ASC, op_index ASC, source ASC
         LIMIT $10
    `
	// The filtered variant is a separate statement so the unfiltered plan
	// stays exactly as it was.
	const qSource = `
        SELECT source, ledger, tx_hash, op_index, ts,
               base_asset, quote_asset,
               base_amount, quote_amount,
               COALESCE(maker, ''), COALESCE(taker, ''),
               COALESCE(routed_via, '')
          FROM trades
         WHERE base_asset  = $1
           AND quote_asset = $2
           AND source      = $12
           AND source <> ALL(string_to_array($11, ','))
           AND ts         >= $3
           AND ts          < $4
           AND (ts, ledger, tx_hash, op_index, source) > ($5, $6, $7, $8, $9)
         ORDER BY ts ASC, ledger ASC, tx_hash ASC, op_index ASC, source ASC
         LIMIT $10
    `
	args := []any{
		p.Base.String(), p.Quote.String(), // $1, $2
		from.UTC(), to.UTC(), // $3, $4
		// $5..$9 — must match the PK tuple order in the SQL above,
		// NOT the function-signature order.
		afterTs.UTC(), afterLedger, afterTxHash, afterOpIndex, afterSource,
		limit,                // $10
		offChainSourcesArg(), // $11
	}
	query := q
	if source != "" {
		query = qSource
		args = append(args, source) // $12
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: TradesInRangeAfter: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []canonical.Trade
	for rows.Next() {
		var t canonical.Trade
		var baseAsset, quoteAsset string
		if err := rows.Scan(
			&t.Source, &t.Ledger, &t.TxHash, &t.OpIndex, &t.Timestamp,
			&baseAsset, &quoteAsset,
			&t.BaseAmount, &t.QuoteAmount,
			&t.Maker, &t.Taker, &t.RoutedVia,
		); err != nil {
			return nil, fmt.Errorf("timescale: TradesInRangeAfter scan: %w", err)
		}
		base, err := canonical.ParseAsset(baseAsset)
		if err != nil {
			return nil, fmt.Errorf("timescale: TradesInRangeAfter base %q: %w", baseAsset, err)
		}
		quote, err := canonical.ParseAsset(quoteAsset)
		if err != nil {
			return nil, fmt.Errorf("timescale: TradesInRangeAfter quote %q: %w", quoteAsset, err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			return nil, fmt.Errorf("timescale: TradesInRangeAfter pair: %w", err)
		}
		t.Pair = pair
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TradesInRangeAfter rows: %w", err)
	}
	return out, nil
}

// FXQuoteAtOrBefore returns the latest FX observation for `pair` at or before
// `cutoff`, from sources in `fxSources`.
//
//  1. `fx_quotes`, written by the active feed (forex worker), when `fxSources`
//     contains [fxQuotesSourceLabel] and both sides are fiat: the newest daily
//     row per ticker within [fxQuotesSnapLookback], USD legs exact 1 (see
//     [fxSnapFromRows]). Legs fully covered by fx_fixings (bar_end ≤ cutoff −
//     [FXFixingLag], within [fxFixingStoreMaxAge]) price from those first.
//  2. `trades` filtered by `fxSources`, structurally empty today (no FX source
//     writes trades); latest ts wins, ties by source name DESC.
//
// Returns [ErrNoFXQuote] when neither has a quote (and for empty `fxSources`
// without a query). `price` is an exact *big.Rat (ADR-0003): per-trade
// quote/base, where the uniform FX 1e6 scale cancels, or the scale-free
// rate_usd ratio. cutoff is rounded to UTC to match InsertTrade.
func (s *Store) FXQuoteAtOrBefore(
	ctx context.Context,
	pair canonical.Pair,
	cutoff time.Time,
	fxSources []string,
) (price *big.Rat, observedAt time.Time, source string, err error) {
	if len(fxSources) == 0 {
		return nil, time.Time{}, "", ErrNoFXQuote
	}

	if slices.Contains(fxSources, fxQuotesSourceLabel) {
		price, observedAt, source, err = s.fxFixingSnapAtOrBefore(ctx, pair, cutoff)
		switch {
		case err == nil:
			return price, observedAt, source, nil
		case !errors.Is(err, ErrNoFXQuote):
			return nil, time.Time{}, "", err
		}
		price, observedAt, source, err = s.fxQuotesSnapAtOrBefore(ctx, pair, cutoff)
		switch {
		case err == nil:
			return price, observedAt, source, nil
		case !errors.Is(err, ErrNoFXQuote):
			return nil, time.Time{}, "", err
		}
		// ErrNoFXQuote within the lookback → legacy trades fallback.
	}

	// Both stored directions, same rule and same two limited arms as
	// [Store.TradesInRange]. A quote recorded USD/EUR answers a EUR/USD
	// question by swapping its two legs, which is the exact reciprocal rate;
	// the alternative was an entry in [directionExempt] saying a price-serving
	// read may stay blind, which is what that list exists to refuse.
	const q = `
        (SELECT source, ts, base_asset, base_amount, quote_amount
           FROM trades
          WHERE base_asset  = $1
            AND quote_asset = $2
            AND ts         <= $3
            AND source      = ANY($4)
          ORDER BY ts DESC, source DESC
          LIMIT 1)
        UNION ALL
        (SELECT source, ts, base_asset, base_amount, quote_amount
           FROM trades
          WHERE base_asset  = $2
            AND quote_asset = $1
            AND ts         <= $3
            AND source      = ANY($4)
          ORDER BY ts DESC, source DESC
          LIMIT 1)
        ORDER BY ts DESC, source DESC
        LIMIT 1
    `
	var (
		gotSource         string
		gotTS             time.Time
		gotBase           string
		baseAmt, quoteAmt string
	)
	row := s.db.QueryRowContext(ctx, q,
		pair.Base.String(), pair.Quote.String(),
		cutoff.UTC(), fxSources,
	)
	if err := row.Scan(&gotSource, &gotTS, &gotBase, &baseAmt, &quoteAmt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, time.Time{}, "", ErrNoFXQuote
		}
		return nil, time.Time{}, "", fmt.Errorf("timescale: FXQuoteAtOrBefore: %w", err)
	}
	// Re-express on the ROW's own base leg, never on which arm returned
	// it: the swap makes `quoteAmt/baseAmt` the requested orientation's
	// rate at full precision, with no division performed here.
	if gotBase != pair.Base.String() {
		baseAmt, quoteAmt = quoteAmt, baseAmt
	}

	baseInt, ok := new(big.Int).SetString(baseAmt, 10)
	if !ok || baseInt.Sign() == 0 {
		return nil, time.Time{}, "", fmt.Errorf("timescale: FXQuoteAtOrBefore: invalid base_amount %q", baseAmt)
	}
	quoteInt, ok := new(big.Int).SetString(quoteAmt, 10)
	if !ok {
		return nil, time.Time{}, "", fmt.Errorf("timescale: FXQuoteAtOrBefore: invalid quote_amount %q", quoteAmt)
	}
	r := new(big.Rat).SetFrac(quoteInt, baseInt)
	return r, gotTS, gotSource, nil
}

// CountTrades returns the total number of rows in the trades table.
// O(hypertable scan) on TimescaleDB; use sparingly (diagnostics + tests).
func (s *Store) CountTrades(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("timescale: CountTrades: %w", err)
	}
	return n, nil
}

// sortTradesByConflictKey orders a batch by the FULL trades PK — every
// column in `ON CONFLICT (source, ledger, tx_hash, op_index, ts)` — so
// every writer acquires row locks in one global, tie-free order (see
// BatchInsertTrades).
func sortTradesByConflictKey(trades []canonical.Trade) {
	sort.Slice(trades, func(i, j int) bool {
		a, b := &trades[i], &trades[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Ledger != b.Ledger {
			return a.Ledger < b.Ledger
		}
		if a.TxHash != b.TxHash {
			return a.TxHash < b.TxHash
		}
		if a.OpIndex != b.OpIndex {
			return a.OpIndex < b.OpIndex
		}
		return a.Timestamp.Before(b.Timestamp)
	})
}

// sameTradeConflictKey reports whether two trades collide on the full
// trades PK (source, ledger, tx_hash, op_index, ts) — i.e. they map to
// the same row. Timestamp is compared with time.Equal so two instants
// that differ only in monotonic-clock reading / location still count as
// equal (they land in the same timestamptz).
func sameTradeConflictKey(a, b *canonical.Trade) bool {
	return a.Source == b.Source &&
		a.Ledger == b.Ledger &&
		a.TxHash == b.TxHash &&
		a.OpIndex == b.OpIndex &&
		a.Timestamp.Equal(b.Timestamp)
}

// dedupeSortedTradesByConflictKey collapses adjacent PK-duplicate rows,
// keeping the LAST (latest redelivery), because Postgres rejects one ON
// CONFLICT DO UPDATE presenting a key twice. Input must be sorted by
// sortTradesByConflictKey. With no duplicate it returns the input untouched,
// so the caller's "sent" tally still counts collapsed duplicates.
func dedupeSortedTradesByConflictKey(sorted []canonical.Trade) []canonical.Trade {
	firstDup := -1
	for i := 1; i < len(sorted); i++ {
		if sameTradeConflictKey(&sorted[i-1], &sorted[i]) {
			firstDup = i
			break
		}
	}
	if firstDup < 0 {
		return sorted // already unique — hot path, no allocation
	}
	out := make([]canonical.Trade, firstDup, len(sorted))
	copy(out, sorted[:firstDup])
	for i := firstDup; i < len(sorted); i++ {
		last := &out[len(out)-1]
		if sameTradeConflictKey(last, &sorted[i]) {
			*last = sorted[i] // keep the latest redelivered copy
			continue
		}
		out = append(out, sorted[i])
	}
	return out
}

// EarliestTradeInWindow returns the earliest stored ts for (source, pair)
// in [from, to), and false when the window holds no row. Served by
// trades_pair_source_ts_idx (migration 0037).
func (s *Store) EarliestTradeInWindow(ctx context.Context, source string, pair canonical.Pair, from, to time.Time) (time.Time, bool, error) {
	const q = `
        SELECT ts
          FROM trades
         WHERE base_asset  = $1::text
           AND quote_asset = $2::text
           AND source      = $3::text
           AND ts         >= $4::timestamptz
           AND ts          < $5::timestamptz
         ORDER BY ts
         LIMIT 1`
	var ts time.Time
	err := s.db.QueryRowContext(ctx, q,
		pair.Base.String(), pair.Quote.String(), source, from.UTC(), to.UTC(),
	).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("timescale: EarliestTradeInWindow (%s %s): %w", source, pair.String(), err)
	}
	return ts.UTC(), true, nil
}

// TradesInWindowOutside counts the stored (source, pair) rows in
// [from, to) whose (tx_hash, ts) is not one of keep's, and returns the
// earliest of them. Those are rows an upsert of keep would leave beside
// its own, so writing keep over the window would count their volume twice.
func (s *Store) TradesInWindowOutside(ctx context.Context, source string, pair canonical.Pair, from, to time.Time, keep []canonical.Trade) (int64, time.Time, error) {
	const q = `
        SELECT count(*), min(t.ts)
          FROM trades t
         WHERE t.base_asset  = $1::text
           AND t.quote_asset = $2::text
           AND t.source      = $3::text
           AND t.ts         >= $4::timestamptz
           AND t.ts          < $5::timestamptz
           AND NOT EXISTS (
                 SELECT 1
                   FROM unnest($6::text[], $7::timestamptz[]) AS k(tx_hash, ts)
                  WHERE k.tx_hash = t.tx_hash
                    AND k.ts      = t.ts)`
	hashes := make([]string, len(keep))
	stamps := make([]time.Time, len(keep))
	for i, tr := range keep {
		hashes[i], stamps[i] = tr.TxHash, tr.Timestamp.UTC()
	}
	var (
		n     int64
		first sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, q,
		pair.Base.String(), pair.Quote.String(), source, from.UTC(), to.UTC(), hashes, stamps,
	).Scan(&n, &first)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("timescale: TradesInWindowOutside (%s %s): %w", source, pair.String(), err)
	}
	return n, first.Time.UTC(), nil
}
