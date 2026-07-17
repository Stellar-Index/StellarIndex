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

	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// externalUSDVolumeDecimals is the off-chain quote-amount scale.
// Every CEX/FX source stamps amounts at the uniform 10^8 decimal
// convention per
// `internal/sources/external/<venue>::externalAmountDecimals`, so a
// quote amount of 4_250_000_000 means $42.50.
const externalUSDVolumeDecimals = 8

// USDVolumeFXResolver returns the asset's USD price as of `at` as
// a decimal string ("0.07127", "1.00", …) for use by
// [tradeUSDVolume]'s Phase 2 fallback path.
//
// Returns ("", false, nil) when no USD anchor is available for
// the asset (the resolver has no rate, the rate is too stale, the
// asset isn't on the operator's covered set). Real I/O errors
// propagate so the caller can surface them in metrics; the trade
// still inserts, just with `usd_volume` left NULL.
//
// Production wiring (F-1268 audit-2026-05-12 — landed):
// [VWAPUSDFXResolver] queries `prices_1m` for `<asset>/<peg>`
// per configured peg, caches per-(asset, 1-minute bucket), and
// expires entries past a freshness ceiling. Wired in
// `cmd/stellarindex-indexer/main.go` whenever the operator's
// `[trades].usd_pegged_classic_assets` list is non-empty.
//
// Concurrency: the resolver is invoked from the trade-insert
// hot path, possibly across many goroutines for high-fanout
// indexers. Implementations MUST be safe for concurrent
// USDPriceAt calls.
type USDVolumeFXResolver interface {
	USDPriceAt(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error)
}

// tradeUSDVolume returns the per-trade USD-equivalent volume as a
// NUMERIC-compatible string, or nil when the trade can't be
// converted cleanly. Returning a *string lets the caller pass the
// value (or sql NULL) straight into the trades.usd_volume column.
//
// Four resolution tiers, tried in order:
//
//  1. Off-chain CEX/FX source AND quote is fiat:USD or a
//     USD-pegged stablecoin per `aggregate.FiatProxy`
//     (USDC/USDT/DAI/PYUSD/USDP). Decimals: 8 (uniform external
//     scale).
//
//  2. On-chain DEX source AND `quoteSpec` recognises the quote
//     asset as USD-pegged (operator-declared classic credits +
//     their SAC wrappers per [USDVolumeQuoteSpec]). Decimals: 7
//     (Stellar classic invariant).
//
//  3. **L2.2 Phase 2** — On-chain DEX source AND `fxResolver !=
//     nil` returns a USD rate for the quote asset at the trade's
//     timestamp. quote_amount × USDPrice / 10^classicDecimals
//     produces a non-NULL `usd_volume` for any operator-watched
//     quote with a recent VWAP. Off-chain trades that fell
//     through tier 1 also get a tier-3 attempt — covers a CEX
//     pair quoted in a non-stablecoin (e.g. binance:XLM/BTC).
//
//  4. **L7.6** — On-chain DEX source, tier 3 declined (the quote
//     asset has no direct USD-pegged market — the common case for a
//     pure-Soroban SEP-41 token whose only liquidity route is
//     against XLM), AND the trade's BASE asset is native XLM (or
//     its SAC wrapper). Trades keep the pool's observed orientation
//     (see [canonical.Trade]'s docstring — we do not re-orient to
//     canonical.Orient's base/quote choice), so a pool that quotes
//     TOKEN-in-XLM stores base=XLM, quote=TOKEN — the mirror image
//     of tier 3's base=TOKEN, quote=XLM case. Valuing the XLM leg
//     needs no knowledge of TOKEN's decimals: base_amount is
//     already XLM stroops (decimals=7, the Stellar classic
//     invariant XLM keeps even wrapped in a Soroban pool), so
//     usd_volume = base_amount/1e7 × XLM/USD. See
//     [tradeUSDVolumeViaXLMBaseAnchor].
//
// Tiers 1 + 2 trust their pegs at insert time — depeg events
// are observed separately via the divergence + anomaly paths and
// do NOT change the inserted usd_volume retroactively. Tiers 3 + 4
// are time-anchored to the trade's timestamp; the resolver MAY
// return (false) on stale data, in which case the column stays
// NULL.
//
// Everything else — including a pure SEP-41/SEP-41 pair where
// neither leg is XLM nor USD-pegged — returns nil and the column
// stays NULL — neither over-claiming USD-equivalence on unknown
// quotes (would mislead downstream sums) nor silently dropping the
// trade itself (the row still inserts; only the USD column goes
// NULL). Valuing a pure SEP-41/SEP-41 leg needs a per-token oracle —
// separate work, matching the boundary
// [Store.SorobanVolume24hUSDForAsset] documents for its query-time
// equivalent.
func tradeUSDVolume(ctx context.Context, t canonical.Trade, quoteSpec *USDVolumeQuoteSpec, fxResolver USDVolumeFXResolver) *string {
	q := t.QuoteAmount.BigInt()
	if q == nil || q.Sign() <= 0 {
		return nil
	}
	md := external.Lookup(t.Source)
	if md.Class != external.ClassExchange {
		// Oracles and aggregators don't emit Trades — defensive nil
		// keeps the function honest if a misregistered source ever
		// sneaks one in.
		return nil
	}
	if decimals, ok := usdVolumeDecimals(t.Pair.Quote, md.Subclass, quoteSpec); ok {
		denom := scaleDenominator(decimals)
		// FloatString(8) gives a fixed-precision decimal — Postgres
		// NUMERIC accepts the form directly with no precision loss
		// for any value that fit in the original big.Int (NUMERIC is
		// arbitrary-precision; FloatString just chooses a render).
		rendered := new(big.Rat).SetFrac(q, denom).FloatString(8)
		return &rendered
	}
	// Phase 2 fallback — only when the operator wired an FX
	// resolver. Skip when nil to keep the no-config path on the
	// existing Phase 1 behaviour exactly.
	if fxResolver == nil {
		return nil
	}
	if v := tradeUSDVolumeViaFX(ctx, t, md.Subclass, fxResolver); v != nil {
		return v
	}
	// Tier 4 (L7.6) — quote-side resolution declined; try the
	// XLM-base anchor before giving up.
	return tradeUSDVolumeViaXLMBaseAnchor(ctx, t, md.Subclass, fxResolver)
}

// tradeUSDVolumeViaFX is the L2.2 Phase 2 multiplication path.
// Picks the right scale per source class (CEX/FX = 8, DEX = 7),
// asks the resolver for the quote asset's USD price at the trade
// time, and renders quote_amount × usdRate / 10^decimals as a
// fixed-precision NUMERIC string.
//
// Errors in the resolver are silent here — the function returns
// nil so the trade still inserts with NULL `usd_volume`, and the
// caller (InsertTrade) doesn't fail the row. Operators read the
// fall-through rate via [TradeInsertsTotal]'s no-label series.
func tradeUSDVolumeViaFX(ctx context.Context, t canonical.Trade, subclass external.Subclass, r USDVolumeFXResolver) *string {
	if r == nil {
		return nil
	}
	usdRateStr, ok, err := r.USDPriceAt(ctx, t.Pair.Quote, t.Timestamp)
	if err != nil || !ok || usdRateStr == "" {
		return nil
	}
	usdRate, ok := new(big.Rat).SetString(usdRateStr)
	if !ok || usdRate.Sign() <= 0 {
		return nil
	}
	var decimals int
	switch subclass {
	case external.SubclassCEX, external.SubclassFX:
		decimals = externalUSDVolumeDecimals
	case external.SubclassDEX:
		decimals = stellarClassicDecimals
	default:
		return nil
	}
	q := new(big.Rat).SetFrac(t.QuoteAmount.BigInt(), scaleDenominator(decimals))
	usdAmount := new(big.Rat).Mul(q, usdRate)
	rendered := usdAmount.FloatString(8)
	return &rendered
}

// tradeUSDVolumeViaXLMBaseAnchor is L7.6 (ROADMAP #37): the
// write-time counterpart of [Store.SorobanVolume24hUSDForAsset]'s
// query-time `base_asset IN ('native', SAC)` CASE. It fires only
// when [tradeUSDVolumeViaFX] declined the quote asset — i.e. the
// trade's quote is a pure-Soroban SEP-41 token with no direct
// USD-pegged market (or no market against XLM either) — AND the
// trade's BASE asset is native XLM or its Stellar Asset Contract
// wrapper.
//
// A pool that quotes a pure SEP-41 token in XLM (its primary
// liquidity route) can store the trade either way round depending
// on the pool's own token ordering — see [tradeUSDVolume]'s tier 3
// vs tier 4 split. When the quote leg carries no resolvable USD
// price but the BASE leg is XLM, this values the trade off the XLM
// leg instead: base_amount is already XLM stroops at the Stellar
// classic 10^7 scale regardless of the quote token's own decimals
// (which we may not even know), so
//
//	usd_volume = (base_amount / 1e7) × XLM/USD
//
// mirrors exactly the NUMERIC identity [sorobanVolume24hUSDQuery]
// applies query-time via `(volume / 1e7::numeric) * xlm_usd`. Once
// this lands at insert time, new pure-SEP41/XLM trades carry a
// non-NULL `usd_volume` in the `trades` row itself, so every
// consumer that sums `usd_volume`/`volume_usd` from `trades` or the
// `prices_*` CAGGs (chart buckets, `/v1/markets`, source stats,
// routed-via, protocol KPIs, …) picks it up automatically — not
// just the one asset-detail field the query-time fallback covers.
// `SorobanVolume24hUSDForAsset`'s `volume_usd > 0` discriminator
// means a tier-4 hit here is picked up there too, with no
// double-count.
//
// Pure SEP-41/SEP-41 pairs (neither leg XLM nor USD-pegged) still
// return nil — matching the documented scope boundary (needs a
// per-token oracle, separate work).
func tradeUSDVolumeViaXLMBaseAnchor(ctx context.Context, t canonical.Trade, subclass external.Subclass, r USDVolumeFXResolver) *string {
	if r == nil || subclass != external.SubclassDEX {
		// Off-chain sources don't have this orientation problem —
		// externalAmountDecimals is uniform and tier 1 already
		// covers every USD-pegged quote.
		return nil
	}
	if !isXLMAsset(t.Pair.Base) {
		return nil
	}
	base := t.BaseAmount.BigInt()
	if base == nil || base.Sign() <= 0 {
		return nil
	}
	usdRateStr, ok, err := r.USDPriceAt(ctx, canonical.NativeAsset(), t.Timestamp)
	if err != nil || !ok || usdRateStr == "" {
		return nil
	}
	usdRate, ok := new(big.Rat).SetString(usdRateStr)
	if !ok || usdRate.Sign() <= 0 {
		return nil
	}
	q := new(big.Rat).SetFrac(base, scaleDenominator(stellarClassicDecimals))
	usdAmount := new(big.Rat).Mul(q, usdRate)
	rendered := usdAmount.FloatString(8)
	return &rendered
}

// isXLMAsset reports whether a is native XLM in either on-chain wire
// form — the classic `native` type, or the Stellar Asset Contract
// that wraps it (the form a Soroban pool holds when one leg of its
// liquidity is XLM). Mirrors [nativeXLMSAC] / [canonOrientSQL]'s
// SQL-side check of the same two forms.
func isXLMAsset(a canonical.Asset) bool {
	return a.Type == canonical.AssetNative ||
		(a.Type == canonical.AssetSoroban && a.ContractID == nativeXLMSAC)
}

// stellarClassicDecimals is the smallest-unit-to-display divisor
// power baked into Stellar classic — XLM, native, every classic
// credit. SEP-41 contracts publish their own value via decimals();
// the FX-fallback path here only fires for on-chain DEX trades
// whose Stellar-side quote assets all share this scale.
const stellarClassicDecimals = 7

// WouldPopulateUSDVolume reports whether [Store.InsertTrade] would
// stamp a non-null `usd_volume` for this trade given the store's
// currently-configured [USDVolumeQuoteSpec] AND
// [USDVolumeFXResolver]. Safe for callers (e.g. the pipeline sink
// emitting coverage metrics) to invoke before InsertTrade.
//
// The predicate runs the full four-tier resolution: Phase 1
// (off-chain CEX/FX with USD-pegged quote → tier 1; on-chain DEX
// with operator-allow-listed quote → tier 2), Phase 2 (any
// remaining trade with a quote-side FX-resolver hit → tier 3), and
// L7.6 (a remaining pure-Soroban SEP-41 quote whose trade's BASE
// asset is XLM and resolves via the same FX resolver → tier 4).
//
// Note: a Phase 2 hit makes a synchronous call into the configured
// resolver. Production resolvers MUST be cheap enough for the
// trade-insert hot path (typically an in-memory cache lookup —
// see the package doc).
func (s *Store) WouldPopulateUSDVolume(ctx context.Context, t canonical.Trade) bool {
	return tradeUSDVolume(ctx, t, s.usdVolumeQuoteSpec, s.usdVolumeFXResolver) != nil
}

// usdVolumeDecimals picks the correct decimal scale for the trade's
// quote asset given the source's subclass + the operator's quote
// spec. Returns (0, false) when the trade isn't a USD-volume
// candidate.
func usdVolumeDecimals(quote canonical.Asset, subclass external.Subclass, quoteSpec *USDVolumeQuoteSpec) (int, bool) {
	switch subclass {
	case external.SubclassCEX, external.SubclassFX:
		// Off-chain — uniform externalAmountDecimals, peg via the
		// crypto-ticker FiatProxy.
		if !quoteIsUSDOrUSDPegged(quote) {
			return 0, false
		}
		return externalUSDVolumeDecimals, true
	case external.SubclassDEX:
		// On-chain (Stellar SDEX, Soroswap, Aquarius, Phoenix,
		// Comet) — peg + decimals come from the operator's
		// USDVolumeQuoteSpec. Phase 1 covers classic + SAC; pure
		// SEP-41 stablecoins are phase 2.
		return quoteSpec.QuoteUSDPegInfo(quote)
	default:
		return 0, false
	}
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

// isDexUnitRatioTrade reports whether a LANDED trade is the signature
// of the 2026-07-07 Phoenix decoder incident: an on-chain DEX trade
// whose base_amount exactly equals its quote_amount (both nonzero) —
// i.e. the decoder is reporting a 1:1 price, which a field-mapping
// bug can produce silently while ADR-0033 completeness checks (which
// verify presence, not plausibility) stay green.
//
// Chosen as the instrumentation choke point over the pipeline sink
// (internal/pipeline/sink.go) deliberately: trades reach `trades`
// through TWO different call shapes there — persistTrade (single-row,
// used by HandleEvent for the projector's per-event sink and as the
// dispatcher batch path's per-row fallback) and flushTradeBatch's
// bulk w.BatchInsertTrades call (the dispatcher's PRIMARY live path,
// which never touches persistTrade on the success case). A metric
// wired into only persistTrade would silently miss the majority of
// on-chain trades, since the batch path is the common case. InsertTrade
// and BatchInsertTrades below are the only two functions every trade
// — dispatcher live batch, projector single-event (via HandleEvent),
// and stellarindex-ops backfill/ch-rebuild re-derives — funnels
// through exactly once per landed row. This is the same choke point
// obs.TradeInsertOutcomeTotal already uses for the analogous
// new-vs-duplicate signal.
//
// ledger == 0 is the off-chain (CEX/FX) marker every external
// connector deliberately stamps (migration
// 0004_relax_trades_ledger_for_offchain); those trades are excluded
// because their amounts are normalised onto a fixed integer scale
// (CLAUDE.md "External-source amount scaling is NOT uniform") where
// an equal-value reading doesn't carry the same "decoder is broken"
// signal an on-chain 1:1 does. canonical.Trade.Validate() already
// requires both amounts to be strictly positive before a trade can
// reach either insert path, so the nonzero check here is
// defence-in-depth, not load-bearing.
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

// InsertTrade writes one trade. Returns nil for a successful insert
// OR a duplicate-key clash (idempotent by storage identity — the
// current conflict target is source+ledger+tx_hash+op_index+ts).
// Other errors propagate.
//
// The trade is validated via [canonical.Trade.Validate] before
// touching the DB; a Validate failure returns [canonical.ErrInvalidTrade].
//
// `usd_volume` is computed via [tradeUSDVolume] for both off-chain
// (CEX/FX) and on-chain (DEX) sources whose quote asset is recognised
// as USD-pegged. Off-chain coverage is built-in via the crypto-ticker
// `aggregate.FiatProxy` map; on-chain coverage requires the operator
// to install a [USDVolumeQuoteSpec] via [Store.SetUSDVolumeQuoteSpec]
// declaring which classic credits (and their SAC wrappers, transitive)
// they trust as USD-pegged. Everything else stores NULL — see the
// L2.2 caveat documented on `Volume24hUSDForAsset` and
// `internal/api/v1.VolumeReader`.
func (s *Store) InsertTrade(ctx context.Context, t canonical.Trade) error {
	if err := t.Validate(); err != nil {
		return err
	}

	// One statement, two effects, fully atomic:
	//   1. Upsert the trade (idempotent-corrective on its PK). On
	//      conflict we DO UPDATE every value column plus derive_generation,
	//      guarded by `trades.derive_generation <= EXCLUDED.derive_generation`
	//      (migration 0109 / INV-3): a re-derive with a higher-or-equal
	//      generation lands its correction in place, while a lower
	//      generation (e.g. a live gen-0 replay) can never revert a
	//      correction. This replaces the old `DO NOTHING`, which silently
	//      discarded corrected re-derives (the re-backfill treadmill).
	//   2. If — and only if — a row was actually INSERTED (not updated),
	//      bump the per-source entry tally (migration 0035). `xmax = 0`
	//      distinguishes a fresh insert from an on-conflict update; the
	//      `HAVING count(*) FILTER (WHERE inserted) > 0` makes the counter
	//      upsert produce zero rows on a duplicate or an update, so a
	//      backfill re-walk / re-derive over already-stored ledgers never
	//      inflates the tally. Data-modifying CTEs always execute even
	//      though `bump` is unreferenced.
	// The trailing `SELECT count(*) FILTER (WHERE inserted)` returns 1
	// (new row) or 0 (duplicate, update, or guard-skipped) — an explicit
	// count, sturdier than the old RowsAffected() path (no driver-quirk
	// fail-open ambiguity), and it keeps the landed-only gating below
	// (registry hook, outcome metric, sentinels) exactly as before.
	const q = `
        WITH ins AS (
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
                usd_volume        = EXCLUDED.usd_volume,
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
	if v := tradeUSDVolume(ctx, t, s.usdVolumeQuoteSpec, s.usdVolumeFXResolver); v != nil {
		usdVolume = *v
	}
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

	// F-1243 (codex audit-2026-05-13) second half: skip the
	// registry hook when the trade was a duplicate (rowsInserted
	// = 0 from the `ON CONFLICT DO NOTHING`). The wave-47 TTL fix
	// already addressed the freeze; this guard fixes the
	// observation_count drift on backfill replays / process
	// restarts that re-encounter already-stored trades.
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

	// Unit-ratio sentinel (2026-07-07 Phoenix incident) — see
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
			// Soft-fail: the trade row is committed; a registry-side
			// problem must not sink the hot path. Stay quiet at info
			// level, but DON'T swallow silently (audit-2026-06-11
			// G11-08). registerClassicAssetSeen is dedupe-cached, so
			// this naturally fires at most once per (asset,issuer) per
			// process — already rate-limited. Debug level keeps the
			// steady state silent while leaving a breadcrumb when an
			// operator turns up logging to chase registry drift.
			slog.Default().Debug("timescale: classic-asset registry upsert failed (soft-skip)",
				"asset", side.String(),
				"ledger", t.Ledger,
				"err", regErr,
			)
		}
	}
	return nil
}

// BatchInsertTrades writes up to a few hundred trades in a single
// SQL roundtrip. Live-r1 incident 2026-06-01: per-INSERT roundtrip
// latency capped indexer throughput at ~5 inserts/sec despite
// postgres-side capacity > 9000/sec (verified by raw psql loop).
// Batching collapses N roundtrips into 1, lifting throughput by
// roughly the batch factor.
//
// Same idempotency semantics as [Store.InsertTrade]:
// `ON CONFLICT DO NOTHING` on the trade PK, and the
// `source_entry_counts` UPSERT only bumps the per-source tally by
// the number of rows actually inserted (so re-runs over already-
// stored ledgers don't inflate the count).
//
// Caller-side filtering: rows are NOT pre-validated by this
// function — callers MUST `Validate` each trade before queueing it
// into a batch (the sink layer already does). USD-volume is computed
// per row from the store's USD-volume resolver, same as the single
// row path.
//
// Returns nil on success; on any DB error the whole batch fails and
// the caller's outcome metric should reflect that. The error is
// best-effort wrapped with `timescale: BatchInsertTrades: %w`. There
// is no partial-success semantic — either every row is attempted (and
// individual rows may be duplicate-absorbed), or the whole batch
// fails.
// tradeBatchValues builds the multi-row INSERT VALUES placeholder fragments and
// the flat positional-arg slice for BatchInsertTrades. Each row contributes 13
// params (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset,
// base_amount, quote_amount, usd_volume, maker, taker, derive_generation).
func (s *Store) tradeBatchValues(ctx context.Context, insertRows []canonical.Trade) ([]string, []any) {
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
		if v := tradeUSDVolume(ctx, t, s.usdVolumeQuoteSpec, s.usdVolumeFXResolver); v != nil {
			usdVolume = *v
		}
		args = append(args,
			t.Source, t.Ledger, t.TxHash, t.OpIndex, t.Timestamp.UTC(),
			t.Pair.Base.String(), t.Pair.Quote.String(),
			t.BaseAmount, t.QuoteAmount, usdVolume,
			t.Maker, t.Taker, s.deriveGeneration,
		)
	}
	return valuesParts, args
}

// scanBatchTradeOutcome runs the batch INSERT..RETURNING query and folds the
// per-landed-row (xmax=0) result set into the outcome tallies (new + unit-ratio
// per source) and the distinct landed assets — each kept at its highest-ledger
// observation (registerClassicAssetSeen advances last_seen_* via GREATEST) —
// that drive the classic-asset/issuer registry hook the batch path previously
// skipped (C2-13b). Split out of BatchInsertTrades for length/complexity.
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

func (s *Store) scanBatchTradeOutcome(ctx context.Context, query string, args []any) (
	perSourceNew, perSourceUnitRatio map[string]int,
	seenAssets map[string]registryObservation, err error,
) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("timescale: BatchInsertTrades: %w", err)
	}
	defer func() { _ = rows.Close() }()

	perSourceNew = make(map[string]int, 4)
	perSourceUnitRatio = make(map[string]int, 4)
	seenAssets = make(map[string]registryObservation, 8)
	noteAsset := func(asset string, ledger uint32, ts time.Time) {
		if prev, ok := seenAssets[asset]; !ok || ledger > prev.ledger {
			seenAssets[asset] = registryObservation{ledger: ledger, ts: ts}
		}
	}
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
		noteAsset(baseAsset, ledger, ts)
		noteAsset(quoteAsset, ledger, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("timescale: BatchInsertTrades rows.Err: %w", err)
	}
	return perSourceNew, perSourceUnitRatio, seenAssets, nil
}

func (s *Store) BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error {
	if len(trades) == 0 {
		return nil
	}

	// Deterministic PK order WITHIN the batch (2026-07-05 deadlock
	// storm, 918 in one afternoon; recurred 2026-07-08 as a CEX-specific
	// storm — ~15 deadlocks/5min, 40P01 2-/3-way ShareLock cycles between
	// concurrent batch inserts). PersistWorkers fans a single event
	// channel out to 8 goroutines with NO sharding by source/symbol (see
	// persistWorker in internal/pipeline/sink.go), so any two workers can
	// end up holding batches with overlapping trades.PK rows — most
	// visibly for CEX sources, whose WS reconnect handling can redeliver
	// the same exchange trade into the shared channel and have it picked
	// up by two different workers at once. Two multi-row INSERT..ON
	// CONFLICT statements that touch the same keys in different orders
	// take row locks in different orders — a textbook AB/BA deadlock.
	//
	// The 2026-07-05 fix sorted by (source, ledger, tx_hash, op_index) —
	// FOUR of the FIVE columns in the actual `ON CONFLICT (source,
	// ledger, tx_hash, op_index, ts)` target. `ts` was left out, so rows
	// that tie on those four columns fall back on `sort.Slice`'s
	// unspecified (non-stable) tie order, which is a function of each
	// batch's original element order — not guaranteed equal across two
	// different workers' batches. That reopens exactly the AB/BA window
	// the sort was meant to close. Sorting by the FULL conflict key
	// (adding `ts` as the final tiebreaker) gives every writer, in every
	// caller of BatchInsertTrades, one total, tie-free lock-acquisition
	// order — implemented here, inside the batch builder, so ALL
	// callers (the indexer's persistWorker drain, the external retry
	// buffer, `stellarindex-ops ch-rebuild`) get the fix automatically
	// rather than each having to remember to pre-sort. The per-row
	// isolate-on-non-infra-error fallback in
	// internal/pipeline/trade_sink.go::flushTradeBatch stays as
	// belt-and-braces for whatever this doesn't catch.
	sortTradesByConflictKey(trades)

	// Collapse intra-batch PK duplicates BEFORE building the statement.
	// The INV-3 fix (migration 0109) turns the batch `ON CONFLICT` into a
	// DO UPDATE, and Postgres rejects a single INSERT..ON CONFLICT DO
	// UPDATE that presents the same conflict key twice ("cannot affect
	// row a second time") — which the old DO NOTHING silently absorbed. A
	// CEX WS reconnect can redeliver the same exchange trade into one
	// worker's batch (see the deadlock note above), so dedupe adjacent
	// equal keys here (input is already conflict-key sorted), keeping the
	// latest copy. The original `trades` slice is left intact so the
	// per-source "sent" tally below still counts the collapsed duplicate
	// as a duplicate — the outcome metric is unchanged.
	insertRows := dedupeSortedTradesByConflictKey(trades)

	// Build VALUES placeholders + args slice (13 params/row incl.
	// derive_generation) — extracted to keep this function under the length
	// budget without splitting the deadlock/dedup narrative above.
	valuesParts, args := s.tradeBatchValues(ctx, insertRows)

	// CTE shape:
	//   ins → multi-row INSERT, RETURNING source (+ ledger/amounts for
	//         the unit-ratio sentinel below) for each row that actually
	//         landed (i.e. wasn't a duplicate).
	//   bump → aggregate landed rows per source, UPSERT into
	//          source_entry_counts.
	// The bump's SELECT … GROUP BY pattern is the multi-source twin
	// of the single-row variant in InsertTrade.
	//
	// The outer SELECT's `unit_ratio` column mirrors isDexUnitRatioTrade
	// (2026-07-07 Phoenix incident sentinel) as a FILTER over the SAME
	// `ins` rows the outcome-metric count uses — cheap (no extra I/O,
	// the RETURNING set is already materialized) and exact: it counts
	// only rows that actually landed, matching InsertTrade's landed-only
	// gating. Keep the predicate in sync with isDexUnitRatioTrade's Go
	// logic if either changes.
	//nolint:gosec // G201: VALUES placeholders constructed only from compile-time format string.
	query := fmt.Sprintf(`
        WITH ins AS (
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
                usd_volume        = EXCLUDED.usd_volume,
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
            -- prevents cross-batch AB/BA deadlocks (2026-07-09 — the
            -- second lock resource the trades-row batch sort missed).
            -- count(*) FILTER (WHERE inserted): a re-derive UPDATE (INV-3,
            -- migration 0109) must NOT inflate the per-source tally, so
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
        -- landed set the single-row InsertTrade path uses (C2-13b). is_unit_ratio
        -- keeps the numeric comparison in SQL (ledger already gated non-zero,
        -- and WHERE inserted already gates landed) to avoid Go-side decimal
        -- equality drift.
        SELECT source, ledger, ts, base_asset, quote_asset,
               (ledger <> 0 AND base_amount = quote_amount AND base_amount <> 0) AS is_unit_ratio
          FROM ins WHERE inserted
    `, strings.Join(valuesParts, ", "))

	perSourceNew, perSourceUnitRatio, seenAssets, err := s.scanBatchTradeOutcome(ctx, query, args)
	if err != nil {
		return err
	}

	emitBatchTradeOutcomeMetrics(trades, perSourceNew, perSourceUnitRatio)

	// C2-13b: auto-register the classic-asset / issuer registry from the
	// LANDED trades — the same Phase-4 hook InsertTrade runs. The batch path
	// used to skip this, reasoning a later single-row InsertTrade would pick
	// the asset up; but the LIVE indexer ingests trades EXCLUSIVELY through
	// this batch path (persistWorker → BatchInsertTrades), so classic_assets
	// / issuers permanently under-populated for batch-ingested assets. We
	// register only genuinely-inserted rows (matching InsertTrade's F-1243
	// duplicate-replay guard), deduped to the distinct assets in this batch,
	// each at the highest ledger we saw. Soft-fail + dedupe-cached, exactly
	// like the single-row path — a registry write can't sink the committed
	// batch, and steady state is a no-op after the first touch per asset.
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
		if regErr := s.registerClassicAssetSeen(ctx, asset, obsv.ledger, obsv.ts); regErr != nil {
			slog.Default().Debug("timescale: batch classic-asset registry upsert failed (soft-skip)",
				"asset", assetID, "ledger", obsv.ledger, "err", regErr)
		}
	}
	return nil
}

// registryObservation is the highest-ledger observation of a landed asset
// within one BatchInsertTrades call — the input to the C2-13b batch-path
// classic-asset/issuer registry hook.
type registryObservation struct {
	ledger uint32
	ts     time.Time
}

// LatestTradesForPair returns up to `limit` most-recent trades for
// the given ordered pair. Returns an empty slice + nil error if the
// pair has no trades.
func (s *Store) LatestTradesForPair(ctx context.Context, p canonical.Pair, limit int) ([]canonical.Trade, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
        SELECT source, ledger, tx_hash, op_index, ts,
               base_asset, quote_asset,
               base_amount, quote_amount,
               COALESCE(maker, ''), COALESCE(taker, ''),
               COALESCE(routed_via, '')
          FROM trades
         WHERE base_asset  = $1
           AND quote_asset = $2
         ORDER BY ts DESC, ledger DESC
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
		var t canonical.Trade
		var baseAsset, quoteAsset string
		if err := rows.Scan(
			&t.Source, &t.Ledger, &t.TxHash, &t.OpIndex, &t.Timestamp,
			&baseAsset, &quoteAsset,
			&t.BaseAmount, &t.QuoteAmount,
			&t.Maker, &t.Taker, &t.RoutedVia,
		); err != nil {
			return nil, fmt.Errorf("timescale: LatestTradesForPair scan: %w", err)
		}
		// Reconstruct Pair via the canonical parse path — this also
		// enforces shape invariants on read.
		base, err := canonical.ParseAsset(baseAsset)
		if err != nil {
			return nil, fmt.Errorf("timescale: LatestTradesForPair base %q: %w", baseAsset, err)
		}
		quote, err := canonical.ParseAsset(quoteAsset)
		if err != nil {
			return nil, fmt.Errorf("timescale: LatestTradesForPair quote %q: %w", quoteAsset, err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			return nil, fmt.Errorf("timescale: LatestTradesForPair pair: %w", err)
		}
		t.Pair = pair
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestTradesForPair rows: %w", err)
	}
	return out, nil
}

// LatestTradePerSource returns the most-recent trade from each source
// that has ever traded `pair`. Empty slice + nil error when the pair
// has no trades.
//
// sourceFilter "" returns all sources; a non-empty value restricts to
// that single source (0- or 1-element slice). Filtering at the SQL
// layer means a single-source query is just an index point lookup.
//
// Implementation: DISTINCT ON (source) ordered by ts DESC, ledger DESC
// — cheap because trades_pair_source_ts_idx (migration 0037) covers
// the (base_asset, quote_asset, source, ts DESC, ledger DESC) order
// exactly. The cost is ~O(num_sources) per pair rather than
// O(rows_in_pair).
func (s *Store) LatestTradePerSource(ctx context.Context, p canonical.Pair, sourceFilter string) ([]canonical.Trade, error) {
	const q = `
        SELECT DISTINCT ON (source)
               source, ledger, tx_hash, op_index, ts,
               base_asset, quote_asset,
               base_amount, quote_amount,
               COALESCE(maker, ''), COALESCE(taker, ''),
               COALESCE(routed_via, '')
          FROM trades
         WHERE base_asset  = $1
           AND quote_asset = $2
           AND ($3 = '' OR source = $3)
         ORDER BY source, ts DESC, ledger DESC
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(), sourceFilter,
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestTradePerSource: %w", err)
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
			return nil, fmt.Errorf("timescale: LatestTradePerSource scan: %w", err)
		}
		base, err := canonical.ParseAsset(baseAsset)
		if err != nil {
			return nil, fmt.Errorf("timescale: LatestTradePerSource base %q: %w", baseAsset, err)
		}
		quote, err := canonical.ParseAsset(quoteAsset)
		if err != nil {
			return nil, fmt.Errorf("timescale: LatestTradePerSource quote %q: %w", quoteAsset, err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			return nil, fmt.Errorf("timescale: LatestTradePerSource pair: %w", err)
		}
		t.Pair = pair
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestTradePerSource rows: %w", err)
	}
	return out, nil
}

// TradesInRange returns trades for the given pair whose close-time
// falls in [from, to). Ordered by (ts ASC, ledger ASC) — chronological,
// which is what OHLC / VWAP callers want.
//
// limit clamps the returned count to avoid runaway queries; pass 0
// or negative for the default of 1000. The hard ceiling is 10000.
//
// An empty slice + nil error means the pair has no trades in the
// window — not an error. Callers distinguish "empty" from "error"
// by testing len(rows).
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
	// present (F-1319). Callers still receive ascending order; only which
	// rows survive truncation changed (newest, not oldest).
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
         ORDER BY ts DESC, ledger DESC
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
		var t canonical.Trade
		var baseAsset, quoteAsset string
		if err := rows.Scan(
			&t.Source, &t.Ledger, &t.TxHash, &t.OpIndex, &t.Timestamp,
			&baseAsset, &quoteAsset,
			&t.BaseAmount, &t.QuoteAmount,
			&t.Maker, &t.Taker, &t.RoutedVia,
		); err != nil {
			return nil, fmt.Errorf("timescale: TradesInRange scan: %w", err)
		}
		base, err := canonical.ParseAsset(baseAsset)
		if err != nil {
			return nil, fmt.Errorf("timescale: TradesInRange base %q: %w", baseAsset, err)
		}
		quote, err := canonical.ParseAsset(quoteAsset)
		if err != nil {
			return nil, fmt.Errorf("timescale: TradesInRange quote %q: %w", quoteAsset, err)
		}
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			return nil, fmt.Errorf("timescale: TradesInRange pair: %w", err)
		}
		t.Pair = pair
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

// TradesInRangeAfter is TradesInRange with a full-PK cursor. Rows
// are returned iff their (ts, ledger, tx_hash, op_index, source)
// tuple is strictly greater than the corresponding `after*` values.
//
// Widening from (ts, ledger) to the full PK closes a pagination
// edge case: multiple trades can share (ts, ledger), and the naive
// tuple `(ts, ledger) > (X, Y)` would skip any same-(ts, ledger)
// row that didn't happen to be the last one on the previous page.
// The primary key is unique so the full-PK tuple gives total order.
//
// afterTs = zero time disables the cursor; use TradesInRange for
// that case (shorter form).
func (s *Store) TradesInRangeAfter(
	ctx context.Context,
	p canonical.Pair,
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
	// Full-PK tuple comparison. ORDER BY + WHERE must agree on the
	// column order so the comparison is monotonic with the sort.
	// Source sorts last so the common case (single-source trades)
	// doesn't pay an unnecessary string compare cost on the index.
	//
	// NOTE: the Go function signature declares afterSource BEFORE
	// afterOpIndex (by-type grouping of the two strings), but the
	// SQL tuple expects them in PK order (tx_hash, op_index, source).
	// The parameter BINDING below — not the signature — is what
	// matters; it hands values to the placeholders in PK order.
	// If you reorder the signature, reorder the binding too.
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
           AND (ts, ledger, tx_hash, op_index, source) > ($5, $6, $7, $8, $9)
         ORDER BY ts ASC, ledger ASC, tx_hash ASC, op_index ASC, source ASC
         LIMIT $10
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(), // $1, $2
		from.UTC(), to.UTC(), // $3, $4
		// $5..$9 — must match the PK tuple order in the SQL above,
		// NOT the function-signature order.
		afterTs.UTC(), afterLedger, afterTxHash, afterOpIndex, afterSource,
		limit, // $10
	)
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

// FXQuoteAtOrBefore returns the most recent FX observation for `pair`
// at-or-before `cutoff`, restricted to sources passed in `fxSources`
// (typically the result of external.FXSources()).
//
// Read order (BACKLOG #42 — the unified FX read path):
//
//  1. `fx_quotes` — the table the ACTIVE FX feed (`massive`, the
//     internal/sources/external/forex worker) writes. Consulted only when
//     `fxSources` admits that feed (contains [fxQuotesSourceLabel]) and
//     both pair sides are fiat. The most recent daily row per needed
//     ticker within [fxQuotesSnapLookback] wins; USD legs are exact 1
//     (rate_usd is USD-anchored). See [fxSnapFromRows] for the exact-
//     Rat cross/inversion math.
//  2. `trades` — the legacy connector-path fallback (polygon-forex /
//     exchangeratesapi rows; disabled in production but kept for
//     compatibility if re-enabled). Fires only when fx_quotes has no
//     row in the lookback. When multiple FX sources have a quote
//     at-or-before cutoff, the one with the largest ts wins; ties are
//     broken by source-name DESC ordering (deterministic across
//     regions because every region's source registry is identical).
//
// Returns (price, observedAt, source, nil) on hit;
// (nil, time.Time{}, "", [ErrNoFXQuote]) when neither table has an FX
// quote at or before cutoff. Other DB errors propagate.
//
// `price` is quote-units-per-base-unit as a *big.Rat (no precision
// loss — every input is a NUMERIC column read as text; floats never
// touch the money path per ADR-0003). On the trades path that is the
// per-trade ratio QuoteAmount/BaseAmount — FX-source trades use a
// uniform 1e8 scale on each side so the ratio is dimensionally clean
// (the scale cancels). On the fx_quotes path it is the rate_usd ratio,
// which is already scale-free. Empty `fxSources` returns ErrNoFXQuote
// without touching the DB.
//
// Implementation notes:
//   - The trades hypertable index `(base_asset, quote_asset, ts DESC)`
//     makes the fallback a constant-cost descending range scan. Pushing
//     the source filter to SQL keeps the scan bounded to FX rows.
//   - cutoff is rounded to UTC to match the InsertTrade convention.
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
		price, observedAt, source, err = s.fxQuotesSnapAtOrBefore(ctx, pair, cutoff)
		switch {
		case err == nil:
			return price, observedAt, source, nil
		case !errors.Is(err, ErrNoFXQuote):
			return nil, time.Time{}, "", err
		}
		// ErrNoFXQuote within the lookback → legacy trades fallback.
	}

	const q = `
        SELECT source, ts, base_amount, quote_amount
          FROM trades
         WHERE base_asset  = $1
           AND quote_asset = $2
           AND ts         <= $3
           AND source      = ANY($4)
         ORDER BY ts DESC, source DESC
         LIMIT 1
    `
	var (
		gotSource         string
		gotTS             time.Time
		baseAmt, quoteAmt string
	)
	row := s.db.QueryRowContext(ctx, q,
		pair.Base.String(), pair.Quote.String(),
		cutoff.UTC(), pq.Array(fxSources),
	)
	if err := row.Scan(&gotSource, &gotTS, &baseAmt, &quoteAmt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, time.Time{}, "", ErrNoFXQuote
		}
		return nil, time.Time{}, "", fmt.Errorf("timescale: FXQuoteAtOrBefore: %w", err)
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
// every writer acquires row locks in one global, tie-free order (the
// 2026-07-05 deadlock-storm fix, extended 2026-07-08 to include `ts`
// after that four-column version left ties to `sort.Slice`'s
// unspecified order; see BatchInsertTrades).
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

// dedupeSortedTradesByConflictKey collapses adjacent PK-duplicate rows in
// a conflict-key-sorted batch, keeping the LAST copy of each run (the
// latest redelivery). It exists because the INV-3 batch upsert (migration
// 0109) uses ON CONFLICT DO UPDATE, which Postgres rejects when one
// statement presents the same conflict key twice; the old DO NOTHING
// tolerated it. Input MUST be sorted by sortTradesByConflictKey so equal
// keys are adjacent.
//
// Copy-on-write: the common case (no intra-batch duplicate) returns the
// input slice untouched and allocates nothing; only when a duplicate is
// found does it build a fresh slice, leaving the caller's slice intact so
// the per-source "sent" tally can still count the collapsed duplicate.
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
