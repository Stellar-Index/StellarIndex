// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── the ESTIMATED tiers' shared row-list re-derive ────────────────────
//
// [usd_volume_restamp.go] repairs the EXACT tiers, whose value is a pure
// decimal rescaling of an amount already on the row and is therefore a
// SQL identity. The estimated tiers cannot be: their value is a function
// of a RATE at the row's timestamp, so each candidate row is rebuilt into
// the [canonical.Trade] the decoder produced and handed to the very
// function [Store.InsertTrade] calls for it. This file is what those
// tiers share — one scan, one decision skeleton, one write set — so that
// what varies between them is only:
//
//	tier       source scope   scanned leg        value function
//	xlm-base   DEX            base_asset = XLM   tradeUSDVolumeViaXLMBaseAnchorFor
//	xlm-quote  DEX            quote_asset = XLM  tradeUSDVolumeViaXLMQuoteAnchorFor
//	cex-fx     CEX            quote_asset = fiat tradeUSDVolumeViaFiatQuoteFor
//
// # The substance gate
//
// Every tier here prices a row off a leg whose USD rate nobody trading
// the pair can author: native XLM (the bridge's own anchor) or a vendor
// fiat feed (`fx_quotes`). A pair with neither — the ~54M token/token
// rows on production — has NO tier in this file and must stay unpriced.
// Valuing it would mean reading the tier-3b `<token>/XLM` bridge, which
// is whatever the last counterparty wrote: the 2026-08-04 incident stored
// $8,559,224.82 for a trade worth $0.86 that way, and the 2026-08-11
// fake-XMR plant stamped $182M off two dust trades. Both scans below are
// bounded by an asset ALLOW-LIST on the tier's own leg, and the Go gate
// ([restampScope]) re-asserts the tier from the insert path's own
// primitives, so a widened scan fails CLOSED rather than silently valuing
// a pair off a poisonable rate.
//
// # Naming
//
// The row/plan/stats vocabulary below was written for the xlm-base tier
// (issue #372) and keeps its exported spelling — renaming a money-path
// type across the tree would bury this change in churn. The aliases give
// the shared vocabulary a tier-neutral name for the code that is not
// about XLM at all.

type (
	// RestampScanParams scopes one window of an estimated-tier re-derive.
	RestampScanParams = XLMBaseRestampParams
	// RestampPlan is one window's decisions: the write set plus the
	// disposition of every row that is not in it.
	RestampPlan = XLMBaseRestampPlan
	// RestampRow is one row the re-derive would rewrite.
	RestampRow = XLMBaseRestampRow
	// RestampStats is the per-window accounting.
	RestampStats = XLMBaseRestampStats
	// restampScanRow is one candidate straight off the SELECT.
	restampScanRow = xlmBaseRestampScan
)

// restampLeg names the trade leg a tier's scan filters on. A closed set
// whose [restampLeg.column] is the only place a column name reaches the
// statement text — nothing operator-supplied ever does.
type restampLeg int

const (
	restampLegBase restampLeg = iota
	restampLegQuote
)

func (l restampLeg) column() string {
	if l == restampLegQuote {
		return "quote_asset"
	}
	return "base_asset"
}

// The candidate scan, in two halves so the two leg variants cannot drift.
//
// The SQL filters only on things whose SQL spelling is exactly their Go
// spelling — the source name, the leg's admissible asset ids, the time
// window and the generation guard. Everything that requires the
// operator's peg configuration (whether a leg is USD-pegged, which is
// keyed on code+issuer plus the SAC-wrapper map) is decided in Go by
// [usdVolumeDecimals], through each tier's gate. `base_asset = ANY(...)`
// rides `trades_pair_source_ts_idx` / `trades_base_ts_idx` and
// `quote_asset = ANY(...)` rides `trades_quote_ts_idx`, so the scan is an
// index walk over one chunk rather than a chunk seq-scan.
const (
	restampScanSelectHead = `
	SELECT source, ledger, tx_hash, op_index, ts,
	       base_asset, quote_asset,
	       base_amount::text, quote_amount::text,
	       usd_volume::text, derive_generation
	  FROM trades
	 WHERE ts >= $1 AND ts < $2
	   AND source = ANY($3)
	   AND `
	restampScanSelectTail = ` = ANY($4)
	   AND derive_generation <= $5
	 ORDER BY ts, source, ledger, tx_hash, op_index
`
)

// restampScanSelect assembles one tier's candidate scan. The only
// variable part is the leg's column name, which comes from the closed
// [restampLeg] set above.
//
//nolint:gosec // no caller-supplied text reaches the statement: the column comes from a two-valued enum, every value is a placeholder
func restampScanSelect(leg restampLeg) string {
	return restampScanSelectHead + leg.column() + restampScanSelectTail
}

// restampGate answers which usd_volume tier owns one rebuilt trade, from
// one tier's point of view: [xlmBaseTierFor], [xlmQuoteTierFor],
// [cexFiatTierFor]. Every one of them is spelled beside [tradeUSDVolume]
// so the tier's definition and the waterfall's branch order stay in one
// file (the LOCKSTEP discipline in that function's header).
type restampGate func(canonical.Trade, *USDVolumeQuoteSpec) restampTierVerdict

// restampValuer is a tier's value for one rebuilt trade — always the
// function the live insert path calls for the same row, never a second
// spelling of it. A nil value means the tier DECLINED to price the row,
// which is reported and never guessed at.
//
// The error is for the OTHER kind of failure: a rate source that could
// not be read at all. The live insert path swallows that (the trade still
// lands, with usd_volume NULL); a backfill must not, or a broken read
// would report a whole population as unpriceable and the operator would
// read a configuration failure as a finding.
type restampValuer func(canonical.Trade) (*string, error)

// restampTierScan is one tier's half of the shared walk: what to scan and
// how to judge what comes back.
type restampTierScan struct {
	// Tier is the operator-facing name, used in errors only.
	Tier string
	// Sources is the resolved source allow-list for the scan.
	Sources []string
	// Leg + Assets bound the scan to the tier's own leg.
	Leg    restampLeg
	Assets []string
	// Gate and Value are the tier's Go-side decision and arithmetic.
	Gate  restampGate
	Value restampValuer
}

// restampScanSources filters a tier's registered source list by the
// caller's allow-list; an empty allow-list means every source the tier
// owns. Names outside the tier's own registry slice are dropped — the
// tier's value function declines them anyway, and scanning them would
// only widen the read.
func restampScanSources(all []string, allow map[string]bool) []string {
	if len(allow) == 0 {
		return all
	}
	out := make([]string, 0, len(all))
	for _, s := range all {
		if allow[s] {
			out = append(out, s)
		}
	}
	return out
}

// planRestampTier scans one bounded window for one tier and returns the
// rows it would rewrite, plus the disposition of every row it would not.
//
// READ-ONLY. This is the whole of a dry run;
// [Store.ApplyUSDVolumeRestampPlan] consumes the plan verbatim, which is
// what makes "-write writes exactly the rows the dry run reported" true
// by construction rather than by two predicates happening to agree.
//
// Requires the store's USD-volume resolution to be installed
// ([InstallUSDVolumeResolution]) — without a resolver every tier here
// declines every row and the run would report a fleet-wide "cannot
// price", which is a configuration error dressed as a finding. Refused up
// front, in the same spirit as [Store.reDeriveNullVolumeGuard].
func (s *Store) planRestampTier(ctx context.Context, p RestampScanParams, t restampTierScan) (*RestampPlan, error) {
	if !p.To.After(p.From) {
		return nil, fmt.Errorf("timescale: %s restamp: empty window [%s, %s)", t.Tier, p.From, p.To)
	}
	if s.usdVolumeFXResolver == nil {
		return nil, fmt.Errorf("timescale: %s restamp: no USD-volume FX resolver installed — "+
			"the tier cannot resolve its rate and every row would report as unpriceable; "+
			"call InstallUSDVolumeResolution with the operator's usd_pegged_classic_assets first", t.Tier)
	}
	if len(t.Sources) == 0 || len(t.Assets) == 0 {
		return &RestampPlan{Stats: NewXLMBaseRestampStats()}, nil
	}

	rows, err := s.db.QueryContext(ctx, restampScanSelect(t.Leg),
		p.From.UTC(), p.To.UTC(), t.Sources, t.Assets, p.MaxGeneration)
	if err != nil {
		return nil, fmt.Errorf("timescale: %s restamp scan [%s, %s): %w",
			t.Tier, p.From.Format(time.RFC3339), p.To.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	plan := &RestampPlan{Stats: NewXLMBaseRestampStats()}
	for rows.Next() {
		var (
			r      restampScanRow
			ledger int64
			opIdx  int64
			stored sql.NullString
		)
		if err := rows.Scan(&r.Source, &ledger, &r.TxHash, &opIdx, &r.TS,
			&r.BaseAsset, &r.QuoteAsset, &r.BaseAmount, &r.QuoteAmount,
			&stored, &r.Generation); err != nil {
			return nil, fmt.Errorf("timescale: %s restamp scan row: %w", t.Tier, err)
		}
		//nolint:gosec // ledger/op_index are non-negative `integer` columns (CHECK-constrained in migration 0001)
		r.Ledger, r.OpIndex = uint32(ledger), uint32(opIdx)
		if stored.Valid {
			v := stored.String
			r.Stored = &v
		}
		r.TS = r.TS.UTC()
		decision, disp, derr := restampDecide(r, s.usdVolumeQuoteSpec, t.Gate, t.Value, p.FillNull, p.MinRelDelta)
		if derr != nil {
			return nil, fmt.Errorf("timescale: %s restamp value %s L%d %s: %w",
				t.Tier, r.Source, r.Ledger, r.TS.Format(time.RFC3339), derr)
		}
		plan.Record(decision, disp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: %s restamp scan rows [%s, %s): %w",
			t.Tier, p.From.Format(time.RFC3339), p.To.Format(time.RFC3339), err)
	}
	return plan, nil
}

// restampDecide judges ONE scanned row for ONE tier.
//
// `value` is injected rather than called directly so the decision rules
// are testable without a live prices_1m / fx_quotes; production passes a
// closure over the live insert path's own function with the store's
// installed [VWAPUSDFXResolver] and [USDVolumeQuoteSpec], so the number
// that reaches the column is the number the insert path computes for the
// same row.
//
// The three rules that make this a re-derive rather than a guess hold for
// every tier that uses it:
//
//  1. THE VALUE COMES FROM THE LIVE FUNCTION (`value`).
//  2. ONLY THAT FUNCTION. A row it declines is REPORTED, never valued
//     through a second route — writing a fallback estimate at a HIGH
//     derive_generation is the one state a later correction cannot claw
//     back.
//  3. NEVER WRITE NULL OVER A VALUE. A declined row that already carries
//     a number keeps it, and is counted in
//     [XLMBaseRestampStats.AnchorDeclinedStored].
//
// The disposition vocabulary is the xlm-base tier's, read generically:
// `xlmBaseQuotePegged` is "a leg is a declared USD peg, so an EXACT tier
// owns this row" and `xlmBaseNotDEX` is "outside this tier's source/leg
// scope".
func restampDecide(
	row restampScanRow,
	spec *USDVolumeQuoteSpec,
	gate restampGate,
	value restampValuer,
	fillNull bool,
	minRelDelta *big.Rat,
) (RestampRow, xlmBaseDisposition, error) {
	out := RestampRow{
		Source: row.Source, Ledger: row.Ledger, TxHash: row.TxHash,
		OpIndex: row.OpIndex, TS: row.TS,
		BaseAsset: row.BaseAsset, QuoteAsset: row.QuoteAsset,
		Stored: row.Stored,
	}
	trade, disp, inTier := restampScope(row, spec, gate)
	if !inTier {
		return out, disp, nil
	}
	want, err := value(trade)
	if err != nil {
		return out, disp, err
	}
	if want == nil {
		return out, xlmBaseAnchorDeclined, nil
	}
	out.Want = *want
	wantRat, wok := new(big.Rat).SetString(out.Want)
	if !wok {
		return out, xlmBaseUnparseable, nil
	}
	if row.Stored == nil {
		out.NullFill = true
		out.AbsDelta = new(big.Rat).Abs(wantRat)
		if !fillNull {
			return out, xlmBaseSkipNull, nil
		}
		return out, xlmBaseWrite, nil
	}
	storedRat, sok := new(big.Rat).SetString(*row.Stored)
	if !sok {
		// A NUMERIC that does not render as a decimal is reportable, not
		// silently rewritable — same posture as the exact-tier tool.
		return out, xlmBaseUnparseable, nil
	}
	if storedRat.Cmp(wantRat) == 0 {
		return out, xlmBaseUnchanged, nil
	}
	out.AbsDelta = new(big.Rat).Abs(new(big.Rat).Sub(wantRat, storedRat))
	if storedRat.Sign() != 0 {
		out.RelDelta = new(big.Rat).Quo(out.AbsDelta, new(big.Rat).Abs(storedRat))
		out.RelOK = true
	}
	if minRelDelta != nil && minRelDelta.Sign() > 0 && out.RelOK && out.RelDelta.Cmp(minRelDelta) < 0 {
		return out, xlmBaseBelowMinRelDelta, nil
	}
	return out, xlmBaseWrite, nil
}

// restampScope decides whether one scanned row is IN the tier `gate`
// describes, and rebuilds it into the [canonical.Trade] the decoder
// produced so the tier's value function can be asked about the same
// object the insert path was.
//
// The tier decision itself lives beside [tradeUSDVolume] so the branch
// order and the population it selects cannot drift apart. What is left
// here is the row-shaped part: parsing the two asset ids and the two
// amounts, with [tradeUSDVolume]'s own positive-amount bail-outs.
func restampScope(row restampScanRow, spec *USDVolumeQuoteSpec, gate restampGate) (canonical.Trade, xlmBaseDisposition, bool) {
	base, err := canonical.ParseAsset(row.BaseAsset)
	if err != nil {
		return canonical.Trade{}, xlmBaseUnparseable, false
	}
	quote, qerr := canonical.ParseAsset(row.QuoteAsset)
	if qerr != nil {
		return canonical.Trade{}, xlmBaseUnparseable, false
	}
	baseAmt, bok := new(big.Int).SetString(row.BaseAmount, 10)
	quoteAmt, qok := new(big.Int).SetString(row.QuoteAmount, 10)
	if !bok || !qok || baseAmt.Sign() <= 0 || quoteAmt.Sign() <= 0 {
		// [tradeUSDVolume] bails on a non-positive quote before any tier,
		// and every value function below bails on a non-positive leg.
		return canonical.Trade{}, xlmBaseUnparseable, false
	}
	trade := canonical.Trade{
		Source:      row.Source,
		Ledger:      row.Ledger,
		TxHash:      row.TxHash,
		OpIndex:     row.OpIndex,
		Timestamp:   row.TS,
		Pair:        canonical.Pair{Base: base, Quote: quote},
		BaseAmount:  canonical.NewAmount(baseAmt),
		QuoteAmount: canonical.NewAmount(quoteAmt),
	}
	// The SQL scan already bounds source and the tier's leg, but
	// re-asserting the tier in Go keeps its definition in ONE place and
	// makes a widened scan fail CLOSED rather than silently valuing, say,
	// a token/token pair off a rate its own counterparties authored.
	switch gate(trade, spec) {
	case restampTierPegged:
		return canonical.Trade{}, xlmBaseQuotePegged, false
	case restampTierOutOfScope:
		return canonical.Trade{}, xlmBaseNotDEX, false
	case restampTierOwns:
		return trade, xlmBaseWrite, true
	default:
		return canonical.Trade{}, xlmBaseNotDEX, false
	}
}

// ApplyUSDVolumeRestampPlan writes an estimated-tier plan's rows and
// returns how many rows the database actually changed. The write set is
// the PLAN — no second predicate is evaluated against the table — and
// each row is written only while `trades.derive_generation <=
// generation` and is stamped with it (INV-3). Shared by every tier: the
// statement is keyed on the primary key and carries the batch's own `ts`
// bounds, neither of which knows which tier decided the value.
func (s *Store) ApplyUSDVolumeRestampPlan(ctx context.Context, plan *RestampPlan, generation int64, batch int) (int64, error) {
	return s.applyXLMBaseRestampBatches(ctx, plan, generation, batch, nil)
}

// ApplyUSDVolumeRestampPlanInChunk is [Store.ApplyUSDVolumeRestampPlan]
// for a plan whose rows all lie in chunk c, with the
// is-the-chunk-still-decompressed guard ahead of every batch.
func (s *Store) ApplyUSDVolumeRestampPlanInChunk(ctx context.Context, c TradeChunk, plan *RestampPlan, generation int64, batch int) (int64, error) {
	return s.applyXLMBaseRestampBatches(ctx, plan, generation, batch, s.stillDecompressed(c))
}

// CEXSourceNames is the ops layer's view of [cexSourceNames] — the
// off-chain exchanges whose fiat-quoted trades the cex-fx tier values.
// Exported so `usd-volume-restamp` can print the scope it is about to
// walk, exactly as [DEXSourceNames] is.
func CEXSourceNames() []string { return cexSourceNames() }
