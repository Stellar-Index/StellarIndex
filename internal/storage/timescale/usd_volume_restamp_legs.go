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

// The ESTIMATED tiers' shared row-list re-derive. [usd_volume_restamp.go] repairs the EXACT tiers
// (a SQL identity); an estimated tier depends on a RATE at the row's timestamp, so each candidate
// row is rebuilt into the [canonical.Trade] the decoder produced and handed to the function
// [Store.InsertTrade] calls. Only these vary per tier:
//
//	tier       source scope   scanned leg        value function
//	xlm-base   DEX            base_asset = XLM   tradeUSDVolumeViaXLMBaseAnchorFor
//	xlm-quote  DEX            quote_asset = XLM  tradeUSDVolumeViaXLMQuoteAnchorFor
//	cex-fx     CEX            quote_asset = fiat tradeUSDVolumeViaFiatQuoteFor
//
// # The substance gate
//
// Every tier prices off a leg whose USD rate nobody trading the pair can author: native XLM or a
// vendor fiat feed (`fx_quotes`). A token/token pair has NO tier here and must stay unpriced, since
// valuing it would read a `<token>/XLM` bridge that is whatever the last counterparty wrote. Scans
// are an ALLOW-LIST on the tier's own leg and the Go gate ([restampScope]) re-asserts the tier, so
// a widened scan fails CLOSED.

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

// restampLeg names the trade leg a tier's scan filters on. [restampLeg.column] is a closed set and
// the only place a column name reaches the statement text; nothing operator-supplied does.
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

// The candidate scan, in two halves so the two leg variants cannot drift. The SQL filters only on
// things whose SQL spelling is exactly their Go spelling: source name, the leg's admissible asset
// ids, the time window and the generation guard. Anything needing the operator's peg configuration
// (keyed on code+issuer plus the SAC-wrapper map) is decided in Go by [usdVolumeDecimals] through
// each tier's gate. `base_asset = ANY(...)` rides `trades_pair_source_ts_idx`/`trades_base_ts_idx`
// and `quote_asset = ANY(...)` rides `trades_quote_ts_idx`, so the scan is an index walk over one
// chunk.
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

// restampScanSelect assembles one tier's candidate scan; only the leg column varies, from the closed [restampLeg] set.
//
//nolint:gosec // no caller-supplied text reaches the statement: the column comes from a two-valued enum, every value is a placeholder
func restampScanSelect(leg restampLeg) string {
	return restampScanSelectHead + leg.column() + restampScanSelectTail
}

// restampGate answers which usd_volume tier owns one rebuilt trade ([xlmBaseTierFor], [xlmQuoteTierFor], [cexFiatTierFor]); each is spelled
// beside [tradeUSDVolume] so tier definitions and waterfall branch order stay in one file (LOCKSTEP).
type restampGate func(canonical.Trade, *USDVolumeQuoteSpec) restampTierVerdict

// restampValuer is a tier's value for one rebuilt trade: the live insert path's own function; nil means the tier DECLINED (reported,
// never guessed). An error means the rate source was unreadable; the live path swallows that (usd_volume NULL) but a backfill must not.
type restampValuer func(canonical.Trade) (*string, error)

// restampTierScan is one tier's half of the shared walk: what to scan and
// how to judge what comes back.
type restampTierScan struct {
	// Tier is the operator-facing name, used in errors only.
	Tier string
	// Basis is what every row this tier writes rests on.
	Basis USDBasis
	// Sources is the resolved source allow-list for the scan.
	Sources []string
	// Leg + Assets bound the scan to the tier's own leg.
	Leg    restampLeg
	Assets []string
	// Gate and Value are the tier's Go-side decision and arithmetic.
	Gate  restampGate
	Value restampValuer
}

// restampScanSources filters a tier's source list by the allow-list (empty = all); names outside the tier's registry are dropped (its
// valuer declines them).
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

// planRestampTier scans one bounded window for one tier: rows it would rewrite plus each skipped row's disposition. READ-ONLY dry run
// that [Store.ApplyUSDVolumeRestampPlan] consumes verbatim; refused without [InstallUSDVolumeResolution].
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
		decision.Basis = t.Basis
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

// restampDecide judges ONE scanned row for ONE tier; `value` is the insert path's own function (injected for DB-free tests). A declined
// row is REPORTED, never valued another way (a high-generation fallback is unrecoverable), and never overwrites a value with NULL.
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
		BaseAmount: row.BaseAmount, QuoteAmount: row.QuoteAmount, Generation: row.Generation,
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

// restampScope decides whether a scanned row is IN the tier `gate` describes and rebuilds the decoder's [canonical.Trade].
// The tier decision lives beside [tradeUSDVolume] so branch order cannot drift; this is the row-shaped part.
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
		// and every estimated tier below bails on a non-positive leg.
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
	// The SQL scan already bounds source and leg; re-asserting the tier in Go keeps its definition
	// in ONE place and makes a widened scan fail CLOSED rather than value a token/token pair off
	// a rate its own counterparties authored.
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

// ApplyUSDVolumeRestampPlan writes an estimated-tier plan's rows and returns how many the database changed; the write set is the plan (no second predicate), each row written only while trades.derive_generation <= generation.
func (s *Store) ApplyUSDVolumeRestampPlan(ctx context.Context, plan *RestampPlan, generation int64, batch int) (int64, error) {
	return s.applyXLMBaseRestampBatches(ctx, plan, generation, batch, nil)
}

// ApplyUSDVolumeRestampPlanInChunk is Store.ApplyUSDVolumeRestampPlan for a plan whose rows all lie in chunk c, with a still-decompressed guard ahead of every batch.
func (s *Store) ApplyUSDVolumeRestampPlanInChunk(ctx context.Context, c TradeChunk, plan *RestampPlan, generation int64, batch int) (int64, error) {
	return s.applyXLMBaseRestampBatches(ctx, plan, generation, batch, s.stillDecompressed(c))
}

// CEXSourceNames exports cexSourceNames, the off-chain exchanges whose fiat-quoted trades the cex-fx tier values, so usd-volume-restamp can print its scope.
func CEXSourceNames() []string { return cexSourceNames() }
