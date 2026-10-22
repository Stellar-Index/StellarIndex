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

// Off-chain amount scaling is NOT uniform: CEX and aggregator sources stamp 10^8, the FX pollers
// 10^6. The USD-volume paths below read external.Registry's AmountDecimals via
// Metadata.AmountScaleDecimals() instead of a hard-coded 8, which would mis-value FX trades 100x.

// USDVolumeFXResolver returns the asset's USD price as of `at` as a decimal string for the FX tiers; ("", false, nil) means no usable anchor, and it MUST be safe for concurrent calls.
type USDVolumeFXResolver interface {
	USDPriceAt(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error)
}

// tradeUSDVolume returns the USD-equivalent volume as a NUMERIC string, or nil (NULL) if it cannot be converted cleanly. Tiers run by
// EXACTNESS: pegs (trusted at insert, a depeg rewrites nothing) before estimated FX; anything else stays NULL, never over-claimed.
// LOCKSTEP: [ClassifyUSDVolumeTier] mirrors this waterfall (legs, order, scale);
// TestExactTierUSDVolume_TracksTheWaterfall and TestClassifyUSDVolumeTier fail on drift.
func tradeUSDVolume(ctx context.Context, t canonical.Trade, quoteSpec *USDVolumeQuoteSpec, fxResolver USDVolumeFXResolver) *string {
	v, _ := tradeUSDVolumeChecked(ctx, t, quoteSpec, fxResolver)
	return v
}

// tradeUSDVolumeChecked is [tradeUSDVolume] returning a resolver read error instead of folding it into NULL; no later tier is tried, since
// an unreadable rate is not a decline and falling through would store a lower tier's estimate.
func tradeUSDVolumeChecked(ctx context.Context, t canonical.Trade, quoteSpec *USDVolumeQuoteSpec, fxResolver USDVolumeFXResolver) (*string, error) {
	v, _, _, err := tradeUSDVolumeCredited(ctx, t, quoteSpec, fxResolver, nil, false)
	return v, err
}

// USDBasis is the leg and rate a stored usd_volume rests on (trades.usd_basis); empty exactly when the value is NULL.
type USDBasis string

const (
	// BasisQuotePegged and BasisBasePegged: that leg is a declared USD peg, valued at face (the exact tiers).
	BasisQuotePegged = USDBasis(TierQuotePegged)
	BasisBasePegged  = USDBasis(TierBasePegged)
	// BasisXLMBase and BasisXLMQuote: that leg is XLM, valued at XLM/USD.
	BasisXLMBase  USDBasis = "xlm_base"
	BasisXLMQuote USDBasis = "xlm_quote"
	// BasisFiatPeg: a leg declared 1:1 to a fiat currency, valued at that currency's USD rate.
	BasisFiatPeg USDBasis = "fiat_peg"
	// BasisFXQuote and BasisFXBase: that leg valued at its own USD rate; a vendor feed off-chain, a market on a DEX.
	BasisFXQuote USDBasis = "fx_quote"
	BasisFXBase  USDBasis = "fx_base"
)

// tradeUSDVolumeCredited is [tradeUSDVolumeChecked] under a credibility budget (nil: none), with the value's basis. refused reports a
// token/token print the charged asset's budget declined: stored NULL, counted "refused". commit records the charge; a dry run leaves the
// budget alone.
func tradeUSDVolumeCredited(
	ctx context.Context, t canonical.Trade, quoteSpec *USDVolumeQuoteSpec, fxResolver USDVolumeFXResolver,
	budget *usdCreditBudget, commit bool,
) (*string, USDBasis, bool, error) {
	q := t.QuoteAmount.BigInt()
	if q == nil || q.Sign() <= 0 {
		return nil, "", false, nil
	}
	md := external.Lookup(t.Source)
	if md.Class != external.ClassExchange {
		// Oracles and aggregators don't emit Trades; stay nil if a misregistered one does.
		return nil, "", false, nil
	}
	if decimals, ok := usdVolumeDecimals(t.Pair.Quote, md, quoteSpec); ok {
		denom := scaleDenominator(decimals)
		// FloatString(8) is a fixed-precision decimal that NUMERIC (arbitrary-precision) accepts
		// directly, with no precision loss for any value that fit the big.Int.
		rendered := new(big.Rat).SetFrac(q, denom).FloatString(8)
		return &rendered, BasisQuotePegged, false, nil
	}
	// Tier 2b: a USD-pegged BASE is as exact as tiers 1/2 (declared peg, no market lookup), so it
	// goes ahead of the estimated FX tiers.
	if v := tradeUSDVolumeViaUSDBase(t, md, quoteSpec); v != nil {
		return v, BasisBasePegged, false, nil
	}
	// The FX tiers need an operator-wired resolver.
	if fxResolver == nil {
		return nil, "", false, nil
	}
	if v, basis, handled, err := xlmLegUSDVolume(ctx, t, md, fxResolver); handled {
		return v, basis, false, err
	}
	if v, basis, err := usdVolumeViaFiatPeg(ctx, t, md, quoteSpec, fxResolver); err != nil || v != nil {
		return v, basis, false, err
	}
	if v, basis, refused, err := usdVolumeViaFXCredited(ctx, t, md, fxResolver, budget, commit); err != nil || v != nil || refused {
		return v, basis, refused, err
	}
	// Tier 4: quote-side resolution declined, so try the XLM-base anchor.
	return usdVolumeViaXLMBaseAnchor(ctx, t, md.Subclass, fxResolver, budget, commit)
}

// keptLegBasis names the leg a two-leg bound stored: [boundUSDVolume] and [creditBoundUSDVolume] return one of their inputs, so
// identity with the rated leg's value tells which.
func keptLegBasis(kept, rated *big.Rat, ratedBasis, otherBasis USDBasis) USDBasis {
	if kept == rated {
		return ratedBasis
	}
	return otherBasis
}

// xlmLegUSDVolume values a trade off its XLM leg, whichever side the pool stored it on; handled is true when the caller returns
// (v, err) as is: the anchor answered or failed, or a DEX trade with an XLM leg declined and stays unpriced.
func xlmLegUSDVolume(ctx context.Context, t canonical.Trade, md external.Metadata, fxResolver USDVolumeFXResolver) (v *string, basis USDBasis, handled bool, err error) {
	xlmBase, xlmQuote := isXLMAsset(t.Pair.Base), isXLMAsset(t.Pair.Quote)
	// With XLM as the BASE leg, value the trade off that leg BEFORE pricing the quote: tier 3's
	// token/XLM x XLM/USD bridge from prices_1m is counterparty-authorable, while XLM is a direct
	// market nobody can author. XLM only: any other base resolves through the same poisonable bridge.
	if xlmBase {
		if v, _, _, err := usdVolumeViaXLMBaseAnchor(ctx, t, md.Subclass, fxResolver, nil, false); err != nil || v != nil {
			return v, BasisXLMBase, true, err
		}
	}
	// Same anchor when the pool stored XLM as the QUOTE leg: orientation is
	// the pool's token order, so both sides of one economic swap must value alike.
	if xlmQuote && !xlmBase {
		if v, err := tradeUSDVolumeViaXLMQuoteAnchorFor(ctx, t, fxResolver); err != nil || v != nil {
			return v, BasisXLMQuote, true, err
		}
	}
	// A DEX trade with an XLM leg is priced off XLM or not at all: once that anchor declines (frozen or
	// stale XLM/USD), the tiers below value it through the other asset's own market, which the
	// counterparty can author, with no XLM cross-check left to bound it. The restamp fills it later.
	if md.Subclass == external.SubclassDEX && (xlmBase || xlmQuote) {
		return nil, "", true, nil
	}
	return nil, "", false, nil
}

// tradeUSDVolumeViaUSDBase is tier 2b: value off a USD-pegged BASE leg (usd = base_amount / 10^decimals); trusted like tiers 1/2, so it wins
// over a quote-side FX estimate. It does not re-check the quote leg: [usdVolumeDecimals] already declined it.
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

// tradeUSDVolumeViaFX is the FX tier (quote_amount x usdRate / 10^decimals, per-source scale). Resolver errors return nil here; the write
// path uses [usdVolumeViaFX] and decides per derive generation whether to fail or store NULL.
func tradeUSDVolumeViaFX(ctx context.Context, t canonical.Trade, md external.Metadata, r USDVolumeFXResolver) *string {
	v, _ := usdVolumeViaFX(ctx, t, md, r)
	return v
}

// usdVolumeViaFX is [tradeUSDVolumeViaFX] with the resolver's read error
// returned rather than folded into a decline, for the restamp tiers.
func usdVolumeViaFX(ctx context.Context, t canonical.Trade, md external.Metadata, r USDVolumeFXResolver) (*string, error) {
	v, _, _, err := usdVolumeViaFXCredited(ctx, t, md, r, nil, false)
	return v, err
}

// usdVolumeViaFXCredited is [usdVolumeViaFX] with a DEX token/token print bounded by [creditBoundUSDVolume]; refused is that
// bound's decline.
func usdVolumeViaFXCredited(
	ctx context.Context, t canonical.Trade, md external.Metadata, r USDVolumeFXResolver, budget *usdCreditBudget, commit bool,
) (*string, USDBasis, bool, error) {
	if r == nil {
		return nil, "", false, nil
	}
	usdRateStr, ok, err := r.USDPriceAt(ctx, t.Pair.Quote, t.Timestamp)
	if err != nil {
		return nil, "", false, err
	}
	if !ok || usdRateStr == "" {
		return nil, "", false, nil
	}
	usdRate, ok := new(big.Rat).SetString(usdRateStr)
	if !ok || usdRate.Sign() <= 0 {
		return nil, "", false, nil
	}
	var decimals int
	switch md.Subclass {
	case external.SubclassCEX, external.SubclassFX:
		// Per-source scale: CEXes stamp 1e8, FX pollers 1e6; assuming 8 mis-values FX trades 100x.
		decimals = md.AmountScaleDecimals()
	case external.SubclassDEX:
		decimals = stellarClassicDecimals
	default:
		return nil, "", false, nil
	}
	q := new(big.Rat).SetFrac(t.QuoteAmount.BigInt(), scaleDenominator(decimals))
	usdAmount := new(big.Rat).Mul(q, usdRate)
	basis := BasisFXQuote

	// DEX-only: CEX/FX quote rates are vendor feeds, not poisonable bridges, and their base legs
	// are often unresolvable anyway.
	if md.Subclass == external.SubclassDEX {
		rated := usdAmount
		var refused bool
		usdAmount, refused, err = creditBoundUSDVolume(ctx, r, budget, creditLegs{
			rated: t.Pair.Quote, ratedVal: rated, other: t.Pair.Base, otherAmount: t.BaseAmount, decimals: decimals, at: t.Timestamp, id: t.ID(),
		}, commit)
		if err != nil || usdAmount == nil {
			return nil, "", refused, err
		}
		basis = keptLegBasis(usdAmount, rated, BasisFXQuote, BasisFXBase)
	}
	rendered := usdAmount.FloatString(8)
	return &rendered, basis, false, nil
}

// fiatPegToleranceBps is how far a declared leg's on-chain USD price may sit from the fiat rate before
// usdVolumeViaFiatPeg declines: a 2% band holds a pegged asset's normal spread (EURC ran -96..+55 bps over 30 days
// on the reference deployment) and rejects a depeg, which is a multi-percent move.
const fiatPegToleranceBps = 200

// usdVolumeViaFiatPeg values a DEX trade off a leg the operator declared 1:1-pegged to a fiat currency
// ([USDVolumeQuoteSpec.DeclareFiatPegs]): amount/10^7 x the fiat->USD rate at trade time. It replaces only the
// counterparty-authorable FX bridge for those legs, so it runs after the XLM anchors. The peg is honoured only while
// the declared CLASSIC asset's on-chain USD price sits within fiatPegToleranceBps of the fiat rate: the classic form,
// not the SAC the leg may carry, because the resolver links a SAC to its markets only through sac_wrappers. A depeg,
// a missing on-chain price or a missing fiat rate declines, so the leg falls through to the estimated FX tier, never
// above it. The value takes the FX tier's leg cross-check and single-leg cap ([boundUSDVolume]).
func usdVolumeViaFiatPeg(ctx context.Context, t canonical.Trade, md external.Metadata, spec *USDVolumeQuoteSpec, r USDVolumeFXResolver) (*string, USDBasis, error) {
	if r == nil || md.Subclass != external.SubclassDEX {
		return nil, "", nil
	}
	quote, base := pegLeg{t.Pair.Quote, t.QuoteAmount, BasisFXQuote}, pegLeg{t.Pair.Base, t.BaseAmount, BasisFXBase}
	for _, pair := range [2][2]pegLeg{{quote, base}, {base, quote}} {
		usd, basis, err := fiatPegLegValue(ctx, spec, r, pair[0], pair[1], t.Timestamp)
		if err != nil || usd != nil {
			return usd, basis, err
		}
	}
	return nil, "", nil
}

// pegLeg is one side of a DEX trade as usdVolumeViaFiatPeg sees it.
type pegLeg struct {
	asset  canonical.Asset
	amount canonical.Amount
	// fxBasis is this leg's basis when the bound stores it at its own market rate.
	fxBasis USDBasis
}

// fiatPegLegValue is one leg of usdVolumeViaFiatPeg: nil when the leg is undeclared or the gate declines.
func fiatPegLegValue(ctx context.Context, spec *USDVolumeQuoteSpec, r USDVolumeFXResolver, declared, other pegLeg, at time.Time) (*string, USDBasis, error) {
	classic, ticker, ok := spec.FiatPegInfo(declared.asset)
	amount := declared.amount.BigInt()
	if !ok || amount == nil || amount.Sign() < 0 {
		return nil, "", nil
	}
	fiatRate, ok, err := resolverRateAt(ctx, r, canonical.Asset{Type: canonical.AssetFiat, Code: ticker}, at)
	if err != nil || !ok {
		return nil, "", err
	}
	onChain, ok, err := resolverRateAt(ctx, r, classic, at)
	if err != nil || !ok || !withinBps(onChain, fiatRate, fiatPegToleranceBps) {
		return nil, "", err
	}
	pegged := new(big.Rat).Mul(new(big.Rat).SetFrac(amount, scaleDenominator(stellarClassicDecimals)), fiatRate)
	usd, err := boundUSDVolume(ctx, r, pegged, other.asset, other.amount, stellarClassicDecimals, at)
	if err != nil || usd == nil {
		return nil, "", err
	}
	rendered := usd.FloatString(8)
	return &rendered, keptLegBasis(usd, pegged, BasisFiatPeg, other.fxBasis), nil
}

// resolverRateAt parses one USDPriceAt answer into a positive rate; ok=false when the resolver declines or the
// string is unusable, so callers treat both as "no rate".
func resolverRateAt(ctx context.Context, r USDVolumeFXResolver, asset canonical.Asset, at time.Time) (*big.Rat, bool, error) {
	s, ok, err := r.USDPriceAt(ctx, asset, at)
	if err != nil {
		return nil, false, err
	}
	if !ok || s == "" {
		return nil, false, nil
	}
	rate, ok := new(big.Rat).SetString(s)
	if !ok || rate.Sign() <= 0 {
		return nil, false, nil
	}
	return rate, true, nil
}

// withinBps reports |a - b| * 10000 <= bps * b, exact in big.Rat.
func withinBps(a, b *big.Rat, bps int64) bool {
	diff := new(big.Rat).Sub(a, b)
	diff.Abs(diff)
	diff.Mul(diff, big.NewRat(10_000, 1))
	bound := new(big.Rat).Mul(b, big.NewRat(bps, 1))
	return diff.Cmp(bound) <= 0
}

// boundUSDVolume bounds the fiat-peg tier's declared leg, and a token/token print only where no credibility budget is installed: the
// other leg is valued too and the SMALLER stored; with it unresolvable a print above singleLegMaxUSDVolume is refused. Never route
// XLM-anchored values here.
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

// usdLegAgreementFactor is how far the two independently valued legs of one trade may diverge
// before the FX tier stores the smaller. 10x is beyond honest spread and thin-market noise.
var usdLegAgreementFactor = big.NewRat(10, 1)

// singleLegMaxUSDVolume caps a trade resting on ONE possibly self-authored leg; no real swap nears nine figures, so only the attacker tail
// is cut.
var singleLegMaxUSDVolume = new(big.Rat).SetInt64(100_000_000)

// fxLegValue values one leg as amount / 10^decimals x USDPriceAt(asset); nil means no resolvable rate, a resolver read error is returned.
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

// usdVolumeViaXLMBaseAnchor values a trade off its BASE leg when the quote FX tier declined and the base passes [baseAnchorEligible]. For
// XLM, base_amount is stroops whatever the quote decimals: usd = (base_amount / 1e7) x XLM/USD, twin of [sorobanVolume24hUSDQuery].
func usdVolumeViaXLMBaseAnchor(
	ctx context.Context, t canonical.Trade, subclass external.Subclass, r USDVolumeFXResolver, budget *usdCreditBudget, commit bool,
) (*string, USDBasis, bool, error) {
	if r == nil || subclass != external.SubclassDEX {
		// Off-chain sources have no orientation problem; tier 1 covers every USD-pegged quote.
		return nil, "", false, nil
	}
	if !baseAnchorEligible(t.Pair.Base) {
		return nil, "", false, nil
	}
	base := t.BaseAmount.BigInt()
	if base == nil || base.Sign() <= 0 {
		return nil, "", false, nil
	}
	// Resolve XLM through its canonical `native` form whichever wire form the pool used: prices_1m
	// stores XLM/USD as `native`, so asking for the SAC contract id would miss. Non-XLM bases
	// resolve as themselves.
	anchor := t.Pair.Base
	if isXLMAsset(anchor) {
		anchor = canonical.NativeAsset()
	}
	usdRateStr, ok, err := r.USDPriceAt(ctx, anchor, t.Timestamp)
	if err != nil {
		return nil, "", false, err
	}
	if !ok || usdRateStr == "" {
		return nil, "", false, nil
	}
	usdRate, ok := new(big.Rat).SetString(usdRateStr)
	if !ok || usdRate.Sign() <= 0 {
		return nil, "", false, nil
	}
	q := new(big.Rat).SetFrac(base, scaleDenominator(stellarClassicDecimals))
	usdAmount := new(big.Rat).Mul(q, usdRate)
	basis := BasisXLMBase
	// A non-XLM base is the one rated leg of a token/token print, so it is charged to the base's credibility budget. XLM is exempt:
	// its rate is a direct market and base_amount is XLM that moved. Applied here so the restamp tiers inherit it.
	if !isXLMAsset(t.Pair.Base) {
		rated := usdAmount
		var refused bool
		usdAmount, refused, err = creditBoundUSDVolume(ctx, r, budget, creditLegs{
			rated: t.Pair.Base, ratedVal: rated, other: t.Pair.Quote, otherAmount: t.QuoteAmount, decimals: stellarClassicDecimals, at: t.Timestamp, id: t.ID(),
		}, commit)
		if err != nil || usdAmount == nil {
			return nil, "", refused, err
		}
		basis = keptLegBasis(usdAmount, rated, BasisFXBase, BasisFXQuote)
	}
	rendered := usdAmount.FloatString(8)
	return &rendered, basis, false, nil
}

// isXLMAsset reports whether a is XLM in either on-chain form (classic native or its SAC); mirrors [canonOrientSQL]'s SQL-side check.
func isXLMAsset(a canonical.Asset) bool {
	return a.Type == canonical.AssetNative ||
		(a.Type == canonical.AssetSoroban && a.ContractID == canonical.NativeSACContractID())
}

// ─── the tier-4 scope, as seen from a STORED row ─────────────────────
//
// The helpers below answer for a stored row what [tradeUSDVolume] answers for one being
// inserted. They live here so the tier definitions and branch order stay in one file (LOCKSTEP),
// and because [external.Registry] is reachable from storage only through this file
// (scripts/ci/lint-imports.sh).

// dexSourceNames returns the sorted [external.SubclassDEX] source names from the same [external.Registry] the insert path reads.
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

// restampTierVerdict says which usd_volume tier owns a stored trade from one re-derive tier's gate, shared so the tiers cannot disagree.
type restampTierVerdict int

const (
	// restampTierOwns: this tier is the branch [tradeUSDVolume] takes.
	restampTierOwns restampTierVerdict = iota
	// restampTierOutOfScope: the source subclass or the tier's own leg is not what the tier needs.
	restampTierOutOfScope
	// restampTierPegged: one leg is a declared USD peg, so tier 1/2/2b values the trade EXACTLY.
	restampTierPegged
)

// xlmBaseTierFor mirrors [tradeUSDVolume]'s branch order: DEX source, quote not a USD peg per [usdVolumeDecimals], XLM base takes the
// [usdVolumeViaXLMBaseAnchor] branch.
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

// xlmQuoteTierFor mirrors [xlmBaseTierFor] for a DEX trade with an XLM QUOTE and a base that is neither XLM nor a USD peg; the tiers are
// DISJOINT. The pegged-quote check stays for an operator who declared an XLM form as a peg.
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

// cexFiatTierFor covers a CEX trade quoted in a non-USD fiat with a non-pegged base; its rate comes from `fx_quotes`
// ([VWAPUSDFXResolver.usdPriceForFiat]), a vendor feed no counterparty can author.
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

// cexSourceNames returns the sorted [external.SubclassCEX] source names; SubclassFX is excluded (fiat/fiat pairs at 1e6 are a different
// population).
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

// tradeUSDVolumeViaXLMBaseAnchorFor is [usdVolumeViaXLMBaseAnchor] for a STORED row; a resolver read error is returned so a restamp aborts.
func tradeUSDVolumeViaXLMBaseAnchorFor(ctx context.Context, t canonical.Trade, r USDVolumeFXResolver) (*string, error) {
	v, _, _, err := usdVolumeViaXLMBaseAnchor(ctx, t, external.Lookup(t.Source).Subclass, r, nil, false)
	return v, err
}

// tradeUSDVolumeViaXLMQuoteAnchorFor values a stored row with an XLM QUOTE (quote_amount / 1e7 x XLM/USD) by mirroring its legs into the
// base anchor, so insert and re-derive agree. No leg cross-check: the value rests on XLM, which nobody can author.
func tradeUSDVolumeViaXLMQuoteAnchorFor(ctx context.Context, t canonical.Trade, r USDVolumeFXResolver) (*string, error) {
	return tradeUSDVolumeViaXLMBaseAnchorFor(ctx, mirrorTradeLegs(t), r)
}

// mirrorTradeLegs swaps base for quote and base_amount for quote_amount; everything else the gates read is unchanged.
func mirrorTradeLegs(t canonical.Trade) canonical.Trade {
	t.Pair = canonical.Pair{Base: t.Pair.Quote, Quote: t.Pair.Base}
	t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
	return t
}

// tradeUSDVolumeViaFiatQuoteFor values a stored non-USD-fiat CEX row through [tradeUSDVolumeViaFX] with the source's scale and fx_quotes
// rate; other shapes are declined.
func tradeUSDVolumeViaFiatQuoteFor(ctx context.Context, t canonical.Trade, r USDVolumeFXResolver) (*string, error) {
	md := external.Lookup(t.Source)
	if md.Subclass != external.SubclassCEX || t.Pair.Quote.Type != canonical.AssetFiat {
		return nil, nil
	}
	return usdVolumeViaFX(ctx, t, md, r)
}

// baseAnchorEligible reports whether an asset can be valued from a raw-VWAP rate by [usdVolumeViaXLMBaseAnchor]: the 1e7 divisor belongs to
// the asset the RATE is against, so SEP-41 tokens qualify; per-whole-unit prices (pegs, oracles) do not, so peg tiers stay classic + SAC.
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

// stellarClassicDecimals is the display-divisor power of XLM and classic credits; the FX fallback only fires for DEX trades sharing this
// scale.
const stellarClassicDecimals = 7

// WouldPopulateUSDVolume reports whether InsertTrade would stamp a non-null usd_volume under the installed quote spec, FX resolver and
// credibility budget (a dry run). An FX-tier hit calls the resolver synchronously, and a first-seen asset runs the budget's seed query.
func (s *Store) WouldPopulateUSDVolume(ctx context.Context, t canonical.Trade) bool {
	v, _, _, err := tradeUSDVolumeCredited(ctx, t, s.usdVolumeQuoteSpec, s.usdVolumeFXResolver, s.creditBudget, false)
	return err == nil && v != nil
}

// usdVolumeDecimals reports whether an asset is USD-pegged and at what scale; side-agnostic (quote tiers 1/2, base tier 2b).
func usdVolumeDecimals(asset canonical.Asset, md external.Metadata, quoteSpec *USDVolumeQuoteSpec) (int, bool) {
	switch md.Subclass {
	case external.SubclassCEX, external.SubclassFX:
		// Off-chain: the SOURCE's registered amount scale (CEXes stamp 1e8 but the FX pollers stamp 1e6, so a hard-coded 8 is a
		// 100x error for FX trades), peg via the crypto-ticker FiatProxy.
		if !quoteIsUSDOrUSDPegged(asset) {
			return 0, false
		}
		return md.AmountScaleDecimals(), true
	case external.SubclassDEX:
		// On-chain (Stellar SDEX, Soroswap, Aquarius, Phoenix, Comet): peg + decimals come from the operator's USDVolumeQuoteSpec.
		// Phase 1 covers classic + SAC; pure SEP-41 stablecoins are phase 2.
		return quoteSpec.QuoteUSDPegInfo(asset)
	default:
		return 0, false
	}
}

// ClassifyUSDVolumeTier reports which waterfall tier the insert path would use for a (source, base, quote) trade and that tier's decimal scale, calling the same functions as tradeUSDVolume in the same order.
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

// quoteIsUSDOrUSDPegged is true for fiat:USD or an aggregate.FiatProxy stablecoin; the peg is trusted at insert and a depeg never rewrites
// usd_volume.
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

// ErrNoFXQuote is returned by [Store.FXQuoteAtOrBefore] when no FX observation exists for the requested pair at-or-before the cutoff.
var ErrNoFXQuote = errors.New("timescale: no FX quote at or before cutoff")

// isDexUnitRatioTrade reports a landed on-chain trade with base_amount == quote_amount (both nonzero), the signature of a decoder
// field-mapping bug. Off-chain (ledger == 0) is excluded: fixed-scale CEX/FX amounts carry no such signal.
func isDexUnitRatioTrade(ledger uint32, base, quote canonical.Amount) bool {
	if ledger == 0 {
		return false
	}
	return !base.IsZero() && base.Cmp(quote) == 0
}

// recordDexTradeUnitRatio bumps obs.DexTradeUnitRatioTotal for a landed unit-ratio trade; split out to unit-test without a database.
func recordDexTradeUnitRatio(t canonical.Trade) {
	if isDexUnitRatioTrade(t.Ledger, t.BaseAmount, t.QuoteAmount) {
		obs.DexTradeUnitRatioTotal.WithLabelValues(t.Source).Inc()
	}
}

// usdLabelResolverError marks a live trade left NULL because a resolver read failed; unlike "thin" or "unroutable" it counts against
// coverage, since a re-derive can still price it.
const usdLabelResolverError = "error"

// usdPopulatedLabel maps resolved-or-not to the stable Prometheus labels dashboards filter on; "refused", "unroutable" and "thin" are
// excluded from the coverage ratio (usd_volume stays NULL).
func usdPopulatedLabel(p canonical.Pair, populated, refused, thin bool) string {
	switch {
	case populated:
		return "yes"
	case refused:
		return "refused"
	case sameClassicIssuer(p.Base, p.Quote):
		return "unroutable"
	case thin:
		return "thin"
	default:
		return "no"
	}
}

// sameClassicIssuer reports whether both legs are credits of one issuer, a declared SAC wrapper counting as its classic asset: an
// issuer's wash trades between its own tokens have no independent market whichever wire form the pool holds.
func sameClassicIssuer(a, b canonical.Asset) bool {
	ca, cb := canonical.CanonicalAsset(a), canonical.CanonicalAsset(b)
	return ca.Type == canonical.AssetClassic && cb.Type == canonical.AssetClassic && ca.Issuer != "" && ca.Issuer == cb.Issuer
}

// thinMarketReporter is implemented by a resolver that can say its substance
// gate refused a leg's rate ([VWAPUSDFXResolver.ThinMarketRefused]).
type thinMarketReporter interface {
	ThinMarketRefused(asset canonical.Asset, at time.Time) bool
}

// noMarketReporter is implemented by a resolver that can say an asset had no market to price it from at all
// ([VWAPUSDFXResolver.NoMarket]).
type noMarketReporter interface {
	NoMarket(asset canonical.Asset, at time.Time) bool
}

// usdLabel is the trade_inserts_total usd_volume_populated value for t given
// its resolved usd_volume v.
func (s *Store) usdLabel(t canonical.Trade, v *string, refused bool) string {
	thin := false
	if v == nil {
		if rep, ok := s.usdVolumeFXResolver.(thinMarketReporter); ok {
			thin = rep.ThinMarketRefused(t.Pair.Quote, t.Timestamp) || rep.ThinMarketRefused(t.Pair.Base, t.Timestamp)
		}
	}
	label := usdPopulatedLabel(t.Pair, v != nil, refused, thin)
	// Neither leg has any market to price from, so there is no route to USD; XLM and the pegs are never marked, so an outage stays "no".
	if rep, ok := s.usdVolumeFXResolver.(noMarketReporter); ok && label == "no" &&
		rep.NoMarket(t.Pair.Base, t.Timestamp) && rep.NoMarket(t.Pair.Quote, t.Timestamp) {
		return "unroutable"
	}
	return label
}

// InsertTrade validates and writes one trade, returning nil on insert or conflict on (source, ledger, tx_hash, op_index, ts).
func (s *Store) InsertTrade(ctx context.Context, t canonical.Trade) error {
	if err := t.Validate(); err != nil {
		return err
	}
	countZeroLegAdmitted(t)

	// One atomic statement: upsert the trade (a lower generation never reverts a correction) and,
	// only for a fresh INSERT (`xmax = 0`), bump source_entry_counts so a re-walk never inflates
	// the tally. Data-modifying CTEs run though `bump` is unreferenced. The trailing count (1 for
	// a new row, else 0) gates the registry hook, outcome metric and sentinels.
	//nolint:gosec // G201: SQL built only from compile-time fragments; values are bind params.
	q := fmt.Sprintf(`
        WITH v (%[1]s) AS (
            VALUES (%[4]s)
        ), ins AS (
            -- routed_via, signer and tx_index are post-insert tagger
            -- columns: never list them here (first-wins, see tx_index.go).
            INSERT INTO trades (%[1]s)
            %[3]s
            ON CONFLICT (source, ledger, tx_hash, op_index, ts) DO UPDATE SET
                base_asset        = EXCLUDED.base_asset,
                quote_asset       = EXCLUDED.quote_asset,
                base_amount       = EXCLUDED.base_amount,
                quote_amount      = EXCLUDED.quote_amount,
                -- At an equal generation a transient FX miss must not NULL a stored value
                -- (the BatchInsertTrades/InsertTrade double write); a higher generation may.
                usd_volume        = CASE
                                        WHEN EXCLUDED.derive_generation > trades.derive_generation
                                            THEN EXCLUDED.usd_volume
                                        ELSE COALESCE(EXCLUDED.usd_volume, trades.usd_volume)
                                    END,
                -- usd_basis follows whichever usd_volume the CASE above kept.
                usd_basis         = CASE
                                        WHEN EXCLUDED.derive_generation > trades.derive_generation
                                          OR EXCLUDED.usd_volume IS NOT NULL
                                            THEN EXCLUDED.usd_basis
                                        ELSE trades.usd_basis
                                    END,
                maker             = EXCLUDED.maker,
                taker             = EXCLUDED.taker,
                derive_generation = EXCLUDED.derive_generation
              WHERE trades.derive_generation <= EXCLUDED.derive_generation AND %[2]s
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
    `, tradeUpsertColumns, tradeUpsertWrites("trades", "EXCLUDED"), tradeUpsertSkipNoOp(5, 5),
		fmt.Sprintf(tradeValuesRow, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14))
	var usdVolume any // sql NULL when nil; pq accepts the *string form too
	v, basis, label, err := s.resolveUSDVolume(ctx, t)
	if err != nil {
		return err
	}
	if v != nil {
		usdVolume = *v
	}
	obs.TradeInsertsTotal.WithLabelValues(t.Source, label).Inc()
	var rowsInserted int64
	if err := s.db.QueryRowContext(ctx, q,
		t.Source, t.Ledger, t.TxHash, t.OpIndex, t.Timestamp.UTC(),
		t.Pair.Base.String(), t.Pair.Quote.String(),
		t.BaseAmount, t.QuoteAmount, usdVolume,
		t.Maker, t.Taker, s.deriveGeneration, string(basis),
	).Scan(&rowsInserted); err != nil {
		return fmt.Errorf("timescale: InsertTrade: %w", err)
	}

	// Per-source new-vs-duplicate outcome lets operators spot a cursor replay or stuck tip:
	// outcome="new" flat while attempts (TradeInsertsTotal) keep climbing.
	outcome := "new"
	if rowsInserted == 0 {
		outcome = "duplicate"
	}
	obs.TradeInsertOutcomeTotal.WithLabelValues(t.Source, outcome).Inc()

	// Skip the registry hook when no row landed (rowsInserted = 0: the conflicting row lost the derive_generation guard on the
	// `ON CONFLICT ... DO UPDATE`), or backfill replays and process restarts that re-encounter stored trades drift
	// observation_count.
	if rowsInserted == 0 {
		return nil
	}

	// Stamp wall-clock only when a fresh row lands. Paired with obs.SourceLastEventUnix: events
	// climbing while inserts flat-line means the cursor is replaying duplicates.
	obs.SourceLastInsertUnix.WithLabelValues(t.Source).Set(float64(time.Now().Unix()))

	// Unit-ratio sentinel: see isDexUnitRatioTrade's godoc for why this is gated on "landed" (not every attempt); a
	// replay/backfill re-encountering an already-stored bad trade must not re-inflate the alert.
	recordDexTradeUnitRatio(t)

	// Auto-register the classic-asset registry from observed trades. Errors are soft: the trade
	// is committed, so log and skip rather than let a registry problem sink the hot path.
	// Dedupe-cached, so a no-op after the first touch per asset/issuer in the process.
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

// tradeColsPerRow is the column count every trade writer sends per row (row INSERT, batch INSERT, COPY).
const tradeColsPerRow = 14

// tradeBatchValues builds the multi-row INSERT placeholders and flat args for BatchInsertTrades: tradeColsPerRow params per row, in
// column order.
func (s *Store) tradeBatchValues(ctx context.Context, insertRows []canonical.Trade) ([]string, []any, error) {
	args := make([]any, 0, len(insertRows)*tradeColsPerRow)
	valuesParts := make([]string, 0, len(insertRows))
	for i, t := range insertRows {
		base := i*tradeColsPerRow + 1
		valuesParts = append(valuesParts, "("+fmt.Sprintf(tradeValuesRow,
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10, base+11, base+12, base+13,
		)+")")
		var usdVolume any
		v, basis, label, err := s.resolveUSDVolume(ctx, t)
		if err != nil {
			return nil, nil, err
		}
		if v != nil {
			usdVolume = *v
		}
		obs.TradeInsertsTotal.WithLabelValues(t.Source, label).Inc()
		args = append(args,
			t.Source, t.Ledger, t.TxHash, t.OpIndex, t.Timestamp.UTC(),
			t.Pair.Base.String(), t.Pair.Quote.String(),
			t.BaseAmount, t.QuoteAmount, usdVolume,
			t.Maker, t.Taker, s.deriveGeneration, string(basis),
		)
	}
	return valuesParts, args, nil
}

// emitBatchTradeOutcomeMetrics records per-source insert outcomes for a batch: landed (new),
// duplicate (sent minus landed) and unit-ratio counts.
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

// scanBatchTradeOutcome runs the batch INSERT..RETURNING in its own transaction, folding landed rows (xmax=0) into tallies. A re-derive
// store lifts the decompression cap via SET LOCAL (else SQLSTATE 53400); live ingest keeps the server cap.
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

// batchTradeDecompressionCapSQL lifts the per-transaction decompressed-tuple cap (0 = unbounded) for a re-derive batch upsert only.
const batchTradeDecompressionCapSQL = "SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0"

// scanBatchTradeRows runs the batch on tx and folds its RETURNING rows into the outcome tallies.
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

// tradeInsertMaxRows is the largest sub-batch per INSERT that keeps tradeColsPerRow params per row under the 65,535 bind ceiling.
const tradeInsertMaxRows = (65535 - 2) / tradeColsPerRow // 2: tradeUpsertSkipNoOp's ts bounds

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
	// Build VALUES placeholders + args slice (tradeColsPerRow params/row).
	valuesParts, args, err := s.tradeBatchValues(ctx, insertRows)
	if err != nil {
		return nil, nil, nil, err
	}

	// ins: multi-row INSERT RETURNING each landed row; bump: per-source UPSERT into
	// source_entry_counts, the multi-row twin of InsertTrade's. The outer `unit_ratio` predicate
	// mirrors isDexUnitRatioTrade; keep the two in sync.
	//nolint:gosec // G201: VALUES placeholders constructed only from compile-time format string.
	query := fmt.Sprintf(`
        WITH v (%[1]s) AS (
            VALUES %[4]s
        ), ins AS (
            -- routed_via, signer and tx_index are post-insert tagger
            -- columns: never list them here (first-wins, see tx_index.go).
            INSERT INTO trades (%[1]s)
            %[3]s
            ON CONFLICT (source, ledger, tx_hash, op_index, ts) DO UPDATE SET
                base_asset        = EXCLUDED.base_asset,
                quote_asset       = EXCLUDED.quote_asset,
                base_amount       = EXCLUDED.base_amount,
                quote_amount      = EXCLUDED.quote_amount,
                -- At an equal generation a transient FX miss must not NULL a stored value
                -- (the BatchInsertTrades/InsertTrade double write); a higher generation may.
                usd_volume        = CASE
                                        WHEN EXCLUDED.derive_generation > trades.derive_generation
                                            THEN EXCLUDED.usd_volume
                                        ELSE COALESCE(EXCLUDED.usd_volume, trades.usd_volume)
                                    END,
                -- usd_basis follows whichever usd_volume the CASE above kept.
                usd_basis         = CASE
                                        WHEN EXCLUDED.derive_generation > trades.derive_generation
                                          OR EXCLUDED.usd_volume IS NOT NULL
                                            THEN EXCLUDED.usd_basis
                                        ELSE trades.usd_basis
                                    END,
                maker             = EXCLUDED.maker,
                taker             = EXCLUDED.taker,
                derive_generation = EXCLUDED.derive_generation
              WHERE trades.derive_generation <= EXCLUDED.derive_generation AND %[2]s
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
    `, tradeUpsertColumns, tradeUpsertWrites("trades", "EXCLUDED"), tradeUpsertSkipNoOp(len(args)+1, len(args)+2), strings.Join(valuesParts, ", "))
	minTS, maxTS := tradeTSBounds(insertRows)
	args = append(args, minTS, maxTS)

	return s.scanBatchTradeOutcome(ctx, query, args)
}

// BatchInsertTrades writes trades with one multi-row upsert per sub-batch of at most tradeInsertMaxRows rows, with InsertTrade's conflict semantics; re-runs are idempotent in row count but not inert in value.
func (s *Store) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	if len(trades) == 0 {
		return nil
	}

	// Drop invalid rows BEFORE building the all-or-nothing sub-batch INSERTs (see the godoc). A wholly unstorable batch returns
	// early; the skips are already accounted for inside the filter.
	storable := s.filterStorableTrades(trades)
	if len(storable) == 0 {
		return nil
	}

	// Sort by the FULL conflict key, ts included, so every writer takes row locks in one tie-free
	// order: persist workers share a channel and CEX reconnects redeliver trades, so overlapping
	// batches otherwise deadlock (40P01). Done here so every caller gets it. Clone first:
	// filterStorableTrades may return the caller's slice, which callers replay in their own order.
	storable = slices.Clone(storable)
	sortTradesByConflictKey(storable)

	// Postgres rejects an INSERT..ON CONFLICT DO UPDATE that presents one key twice, and a CEX
	// redelivery can do that, so collapse adjacent duplicates keeping the latest. `trades` stays
	// intact so the "sent" tally still counts the duplicate.
	insertRows := dedupeSortedTradesByConflictKey(storable)

	// Postgres' extended protocol caps one statement at 65,535 bind parameters (5,041 rows at 13
	// each), so a large batch would fail outright. Send parameter-safe sub-batches and tally every
	// COMMITTED one, so metrics and the registry hook cover exactly the rows that landed even if a
	// later sub-batch fails.
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

// TradeSubBatchError is a BatchInsertTrades failure: rows [0, Start) committed, sub-batch [Start, End) failed with Err, and nothing from Start on was written.
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

// insertTradeSubBatches sends insertRows in parameter-safe sub-batches, stopping at the first failure; the outcome covers every committed
// sub-batch even when err is non-nil.
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

// noteRegistryObservation folds o into asset's observation, keeping the lowest and highest ledger separately.
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
			// A stored asset string that won't re-parse is a decoder-side invariant break, not a batch failure (the trades already
			// committed). Breadcrumb at debug, as with the hook's own soft-fail, and skip this asset.
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

// filterStorableTrades returns the rows passing [canonical.Trade.Validate] so one bad row cannot sink an all-or-nothing sub-batch; drops
// stay loud (SourceInsertErrorsTotal + ERROR). An SDEX one-side-zero fill passes, counted on [obs.TradesZeroLegAdmittedTotal].
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

// IsOneSideZeroFill reports whether t is the SDEX rounding artifact where exactly one leg rounded to 0 and the other stayed positive; such a trade is stored but unpriceable.
func IsOneSideZeroFill(t canonical.Trade) bool {
	bs, qs := t.BaseAmount.Sign(), t.QuoteAmount.Sign()
	return bs >= 0 && qs >= 0 && (bs == 0) != (qs == 0)
}

// countZeroLegAdmitted bumps [obs.TradesZeroLegAdmittedTotal] for a Validate-passing trade with one zero leg ("stored but unpriceable").
func countZeroLegAdmitted(t canonical.Trade) {
	if IsOneSideZeroFill(t) {
		obs.TradesZeroLegAdmittedTotal.WithLabelValues(t.Source).Inc()
	}
}

// registryObservation is an asset's lowest and highest landed ledger in one BatchInsertTrades call; both ends so a backfill offers the true
// first-seen.
type registryObservation struct {
	minLedger, maxLedger uint32
	minTs, maxTs         time.Time
}

func newRegistryObservation(ledger uint32, ts time.Time) registryObservation {
	return registryObservation{minLedger: ledger, maxLedger: ledger, minTs: ts, maxTs: ts}
}

// A market has NO stored direction: the SDEX decoder sets base = soldAsset, so one market lands as
// both (A,B) and (B,A) rows, and a read keyed on one direction silently misses the other. The
// readers below select both directions and re-express flipped rows per ROW ([orientTradeTo]).
//
// TWO LIMITED ARMS, not one OR'd scan, so each keeps its index-ordered scan (trades_pair_ts_idx /
// trades_pair_source_ts_idx) and early stop. Aggregates fold the same way (docs/architecture/
// aggregation-plan.md "The direction fold"). [Store.TradesInRangeAfter] stays one-directional (its
// caller merges under a keyset cursor) and is the sole [directionExempt].

// maxLatestTradesForPair caps [Store.LatestTradesForPair]: each arm is a
// time-unbounded walk, so an unclamped limit could materialise the market.
// Each arm orders by the outer key and takes LIMIT rather than FETCH FIRST ... WITH TIES on
// (ts, ledger): under ChunkAppend a WITH TIES arm trips the planner bounded sort
// ("retrieved too many tuples in a bounded sort") on a pair whose newest ledger holds many
// trades, and the full key keeps the union top-N equal to the union of each arm top-N.
const maxLatestTradesForPair = 1000

// recentTradesWindow is LatestTradesForPair's first try: ~2 trades chunks instead of the ~58 per arm a 400-day bound plans, which costs
// ~1s on each fresh connection and stalled every /v1/price call in a burst that opened several.
const recentTradesWindow = 7 * 24 * time.Hour

// LatestTradesForPair returns up to `limit` most-recent trades for the market at or after `since` (zero: unbounded), in either stored
// direction, oriented as requested; empty slice + nil if none.
func (s *Store) LatestTradesForPair(ctx context.Context, p canonical.Pair, limit int, since time.Time) ([]canonical.Trade, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maxLatestTradesForPair {
		limit = maxLatestTradesForPair
	}
	// Floored to a UTC midnight so the statement text, and its cached plan, holds for a day. `limit` rows inside the window are the
	// newest `limit` overall; fewer means older rows may rank, so the caller's bound decides.
	recent := time.Now().Add(-recentTradesWindow).UTC().Truncate(24 * time.Hour)
	if !since.Before(recent) {
		return s.latestTradesForPair(ctx, s.db, p, limit, since)
	}
	out, err := s.latestTradesForPair(ctx, s.db, p, limit, recent)
	if err != nil || len(out) >= limit {
		return out, err
	}
	key := fmt.Sprintf("%s|%s|%d|%d", p.Base, p.Quote, since.Unix(), recent.Unix())
	if s.olderTradesKnownEmpty(key) {
		return out, nil
	}
	all, err := s.latestTradesForPair(ctx, s.tradeWalkDB(), p, limit, since)
	if err == nil && !slices.ContainsFunc(all, func(t canonical.Trade) bool { return t.Timestamp.Before(recent) }) {
		s.markOlderTradesEmpty(key)
	}
	return all, err
}

func (s *Store) latestTradesForPair(ctx context.Context, db tradeQuerier, p canonical.Pair, limit int, since time.Time) ([]canonical.Trade, error) {
	// A LITERAL lower bound, as in latestClosedVWAP1m: TimescaleDB excludes chunks at plan time only for a constant, and planning
	// every trades chunk is most of an unbounded miss (~325ms vs ~24ms per arm on r1 at a 400-day bound).
	// The LIMIT is a literal too: a bound LIMIT makes the generic plan cost out worse than a custom one, so
	// Postgres re-plans every chunk on every call (55-220ms on r1); with a literal it caches the generic plan.
	lower := ""
	if !since.IsZero() {
		lower = fmt.Sprintf("AND ts >= TIMESTAMPTZ '%s'", since.UTC().Format("2006-01-02 15:04:05-07"))
	}
	//nolint:gosec // G201: lower is a formatted time.Time and limit a clamped int, never caller text
	q := fmt.Sprintf(`
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
            %[1]s
          ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
          LIMIT %[2]d)
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
            %[1]s
          ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
          LIMIT %[2]d)
        ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
        LIMIT %[2]d
    `, lower, limit)
	rows, err := db.QueryContext(ctx, q, p.Base.String(), p.Quote.String())
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

// offChainSourcesArg is the comma-joined non-on-chain source set, bound as `string_to_array($n, ',')`: /v1/history and /v1/observations
// serve on-chain trades only (redistribution terms), filtered in the query so keyset page sizes hold.
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

// nonVWAPSourcesArg is the comma-joined registry names with IncludeInVWAP=false, bound as `string_to_array($n, ',')`. A CAGG
// bucket whose `sources` all sit in this set holds no fill-derived VWAP (a vendor daily close, a derived series); readers that
// pick a first or latest bucket skip it. An unregistered source is outside the set, so such a bucket still counts.
func nonVWAPSourcesArg() string {
	var names []string
	for name, md := range external.Registry {
		if !md.IncludeInVWAP {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// LatestTradePerSource returns each source's most-recent trade on the market, in either stored direction, oriented as requested; sourceFilter "" means all; unbounded-latest-ok: "last trade seen" has no window that preserves it;
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

// scanTradeOriented scans one latest-trade row, rebuilds its Pair via the canonical parse path and re-expresses it in `want`'s orientation.
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

// orientTradeTo re-expresses a stored trade in `want`'s orientation by swapping legs and amounts ([canonical.Orient]); the swap divides
// nothing, so price inverts exactly downstream (ADR-0003). A row in neither orientation is returned as is.
func orientTradeTo(t canonical.Trade, want canonical.Pair) canonical.Trade {
	if !t.Pair.Equal(want.Flip()) {
		return t
	}
	t.Pair = want
	t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
	return t
}

// tradeIsLaterInMarket compares on (ts, ledger, tx_hash, op_index), /v1/history's order minus `source`, whose Go byte order may differ from
// the DB collation.
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

// MaxTradesInRangeLimit is the hard ceiling [Store.TradesInRange] clamps its limit to.
const MaxTradesInRangeLimit = 10000

// TradesInRange returns the market's trades with ts in [from, to) in either stored direction, oriented as requested and ordered (ts, ledger) ASC; one direction alone is biased, so both are read.
// Rows from non-VWAP sources (a vendor daily close, a derived series) are price points, not fills, and are left out.
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
	// Order DESC so the LIMIT keeps the NEWEST rows when the window exceeds `limit`, then reverse
	// to ascending below. Keeping the oldest would compute a busy VWAP from a stale slice that
	// never reaches the present.
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
            AND NOT (source = ANY(string_to_array($6, ',')))
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
            AND NOT (source = ANY(string_to_array($6, ',')))
          ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
          LIMIT $5)
        ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC
        LIMIT $5
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(),
		from.UTC(), to.UTC(), limit, nonVWAPSourcesArg(),
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

// TradesInRangeAfter is TradesInRange with a full-PK cursor (ts, ledger, tx_hash, op_index, source) strictly greater than the after* values, since several trades can share (ts, ledger); a zero afterTs disables it.
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

// TradesInRangeAfterFromSource is [Store.TradesInRangeAfter] restricted to rows written by one source.
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

// FXQuoteAtOrBefore returns the latest FX observation for `pair` at or before `cutoff` from sources in `fxSources`, preferring the daily fx_quotes snapshot when both sides are fiat.
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

	// Both stored directions, with the same two limited arms as [Store.TradesInRange]. A quote
	// recorded USD/EUR answers a EUR/USD question by swapping its legs (the exact reciprocal), so
	// this price-serving read needs no [directionExempt] entry.
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

// sortTradesByConflictKey orders a batch by the FULL trades PK so every writer takes row locks in one global, tie-free order.
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

// sameTradeConflictKey reports a full-PK collision; timestamps use time.Equal since instants differing only in location or monotonic reading
// collide.
func sameTradeConflictKey(a, b *canonical.Trade) bool {
	return a.Source == b.Source &&
		a.Ledger == b.Ledger &&
		a.TxHash == b.TxHash &&
		a.OpIndex == b.OpIndex &&
		a.Timestamp.Equal(b.Timestamp)
}

// dedupeSortedTradesByConflictKey collapses adjacent PK duplicates keeping the LAST, since Postgres rejects one ON CONFLICT DO UPDATE
// presenting a key twice. Input must be sorted by [sortTradesByConflictKey].
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

// EarliestTradeInWindow returns the earliest stored ts for (source, pair) in [from, to), and false when the window holds no row.
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

// TradesInWindowOutside counts the stored (source, pair) rows in [from, to) whose (tx_hash, ts) is not one of keep's, and returns the earliest of them.
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

// TradeSourcesInWindow lists which of `sources` hold a stored row for pair in [from, to), sorted. The
// candidate list is bound so the lookup walks trades_pair_source_ts_idx per source instead of every row
// of the pair in the window.
func (s *Store) TradeSourcesInWindow(ctx context.Context, pair canonical.Pair, from, to time.Time, sources []string) ([]string, error) {
	const q = `
        SELECT DISTINCT source
          FROM trades
         WHERE base_asset = $1::text AND quote_asset = $2::text
           AND ts >= $3::timestamptz AND ts < $4::timestamptz
           AND source = ANY(string_to_array($5, ','))
         ORDER BY source`
	rows, err := s.db.QueryContext(ctx, q, pair.Base.String(), pair.Quote.String(), from.UTC(), to.UTC(), strings.Join(sources, ","))
	if err != nil {
		return nil, fmt.Errorf("timescale: TradeSourcesInWindow (%s): %w", pair.String(), err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return nil, fmt.Errorf("timescale: TradeSourcesInWindow (%s): %w", pair.String(), err)
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TradeSourcesInWindow (%s): %w", pair.String(), err)
	}
	return out, nil
}
