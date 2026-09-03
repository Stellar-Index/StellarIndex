// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── #372: the XLM-BASE usd_volume RE-DERIVE ──────────────────────────
//
// [usd_volume_restamp.go] repairs the EXACT tiers, whose value is a pure
// decimal rescaling of an amount already on the row and can therefore be
// evaluated in SQL. This file is the ESTIMATED-tier counterpart for the
// one estimated tier whose input is not authorable by a counterparty: the
// XLM-base anchor (`usd_volume = base_amount/1e7 x XLM/USD at ts`,
// [tradeUSDVolumeViaXLMBaseAnchor]).
//
// The population it exists for (issue #372, triage G9 2026-09-02): every
// on-chain DEX trade with an XLM BASE leg written before `fd1860bd`
// (2026-08-04, v0.25.0). Until that commit the waterfall reached the
// QUOTE side first, so an `XLM/<token>` trade was valued through the
// token's own thin `<token>/USDC` prices_1m bucket — a rate the token's
// own counterparties author — instead of off the XLM leg, which is the
// measured side of the trade and whose rate is a direct market. Measured
// on r1: a 5-XLM sdex trade worth $0.72 stored as $0.0037 (43x under),
// and ~31% of pre-2026-07-09 XLM-base rows stored NULL because the quote
// leg resolved to nothing at all ($32k of $134k unpriced on 2026-05-19).
// The insert path is correct at HEAD; nothing has ever re-derived the
// rows behind it (they all sit at `derive_generation = 0`).
//
// Three rules make this a re-derive rather than a guess, and they are the
// reason the arithmetic is NOT done in SQL:
//
//  1. THE VALUE COMES FROM THE LIVE FUNCTION. Each candidate row is
//     rebuilt into the [canonical.Trade] the decoder produced and handed
//     to [tradeUSDVolumeViaXLMBaseAnchor] — the same function
//     [Store.InsertTrade] calls, with the store's installed
//     [VWAPUSDFXResolver] and [USDVolumeQuoteSpec]. There is no second
//     spelling of the waterfall to drift against (the reimplementation
//     trap the verifier's header warns about). The resolver is
//     time-anchored to the ROW's `ts`, not to now(), so a historical
//     re-derive is deterministic given prices_1m.
//
//  2. ONLY THE ANCHOR. The live insert path, when the anchor declines,
//     falls through to [tradeUSDVolumeViaFX] — the quote-side route that
//     wrote the defect. This tool deliberately stops at the anchor: a row
//     the anchor cannot price is REPORTED, not valued. Writing the
//     quote-side estimate here would re-commit the exact error #372 is
//     about, and it would do so at a HIGH `derive_generation`, which is
//     the one state a later correction cannot claw back. An unpriced row
//     stays recoverable; a confidently-wrong high-generation row does not.
//
//  3. NEVER WRITE NULL OVER A VALUE. When the anchor declines on a row
//     that already carries a (quote-side, probably wrong) number, the row
//     is left exactly as it is and counted in
//     [XLMBaseRestampStats.AnchorDeclinedStored]. Blanking it would
//     destroy a figure every volume surface already sums, on the strength
//     of an inference this tool is not entitled to make.
//
// Scope, decided in Go from the SAME primitives the insert path uses —
// never from a SQL predicate that could mean something subtly different:
//
//   - the source's registered subclass is DEX. The anchor returns nil for
//     anything else, so a CEX/FX row is not in this tier at all.
//   - base leg is an XLM form ([isXLMAsset]: `native` or the SAC wrapper).
//     That is the exact condition under which [tradeUSDVolume] takes the
//     anchor branch AHEAD of the quote side.
//   - quote leg is NOT USD-pegged ([usdVolumeDecimals]). A pegged quote is
//     tier 1/2 — exact, and `usd-volume-restamp -tier exact`'s job. Rows
//     of the two tiers never overlap.

// xlmBaseRestampSelect is the candidate scan for one bounded window.
//
// The SQL half filters only on things whose SQL spelling is exactly their
// Go spelling — the source name, the two XLM wire forms, the time window
// and the generation guard. Everything that requires the operator's peg
// configuration (i.e. whether the QUOTE leg is USD-pegged, which is keyed
// on code+issuer plus the SAC-wrapper map) is decided in Go by
// [usdVolumeDecimals]. `base_asset = ANY(...)` rides
// `trades_pair_source_ts_idx` / `trades_base_ts_idx`, so the scan is an
// index walk over one chunk rather than a chunk seq-scan.
const xlmBaseRestampSelect = `
	SELECT source, ledger, tx_hash, op_index, ts,
	       base_asset, quote_asset,
	       base_amount::text, quote_amount::text,
	       usd_volume::text, derive_generation
	  FROM trades
	 WHERE ts >= $1 AND ts < $2
	   AND source     = ANY($3)
	   AND base_asset = ANY($4)
	   AND derive_generation <= $5
	 ORDER BY ts, source, ledger, tx_hash, op_index
`

// XLMBaseRestampParams scopes one window of the XLM-base re-derive.
type XLMBaseRestampParams struct {
	// [From, To) bounds the rows by ts. Callers slice a day into bounded
	// windows so neither the candidate scan nor any single UPDATE has to
	// decompress a whole chunk in one transaction.
	From, To time.Time

	// Sources, when non-empty, restricts the scan to these source names.
	// Names outside the DEX registry are dropped — the anchor declines
	// them anyway, and scanning them would only widen the read.
	Sources map[string]bool

	// FillNull admits rows whose stored usd_volume is NULL. Off by
	// default: filling an unpriced row is a COVERAGE change (~31% of this
	// population on the measured days), which the operator opts into
	// rather than a value-repair tool doing it silently. The planner
	// counts the NULL population either way, so a dry run always shows
	// what -fill-null would add.
	FillNull bool

	// MaxGeneration is the INV-3 read guard: rows already stamped by a
	// LATER re-derive are not candidates. Callers pass the run's own
	// generation (the same value the UPDATE writes), or a lower value to
	// target a specific vintage — e.g. 0 for the never-re-derived
	// population #372 is about.
	MaxGeneration int64

	// MinRelDelta, when non-nil and positive, suppresses rows whose
	// relative move |new-old|/|old| is below it from the WRITE set. Nil
	// (the default) writes every row whose value differs at all. It never
	// suppresses a NULL->value fill, which has no relative move to
	// measure.
	MinRelDelta *big.Rat
}

// XLMBaseRestampRow is one row the re-derive would rewrite: its primary
// key, what is stored now, and the value [tradeUSDVolumeViaXLMBaseAnchor]
// produces for it today.
type XLMBaseRestampRow struct {
	Source  string
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	TS      time.Time

	BaseAsset  string
	QuoteAsset string

	// Stored is the current column value; nil for SQL NULL.
	Stored *string
	// Want is the anchor's value, rendered exactly as the insert path
	// renders it (`big.Rat.FloatString(8)`).
	Want string

	// AbsDelta is |Want - Stored| in USD; equal to Want for a NULL fill.
	AbsDelta *big.Rat
	// RelDelta is |Want - Stored| / |Stored|. RelOK is false when the
	// stored value is NULL or zero, i.e. there is no ratio to state.
	RelDelta *big.Rat
	RelOK    bool
	// NullFill marks a row that goes from SQL NULL to a value.
	NullFill bool
}

// XLMBaseRestampStats is the accounting for one planned window. Every
// scanned row lands in exactly one of the disposition counters, so a
// report can be checked for self-consistency — the tool prints the
// reconciliation line, and a gate that silently dropped rows would show
// up as a residual rather than as a quietly smaller number.
type XLMBaseRestampStats struct {
	// Scanned is every row the SQL predicate returned.
	Scanned int64
	// QuotePegged rows have a USD-pegged quote leg: exact tier, owned by
	// `-tier exact`, never touched here.
	QuotePegged int64
	// NotDEX rows came from a source whose subclass is not DEX, or whose
	// base leg is not an XLM form. Should be zero given the scan's
	// filters; counted so a registry change that re-classes a source is
	// visible rather than silently shrinking the population.
	NotDEX int64
	// Unparseable rows carry a base/quote amount or a stored usd_volume
	// that is not a positive decimal — reportable, never rewritten.
	Unparseable int64
	// AnchorDeclinedNull / AnchorDeclinedStored are the rows the XLM
	// anchor cannot price, split by what they hold today. The first is
	// coverage the re-derive cannot recover; the second is the population
	// that stays WRONG after a clean run, and is the number that decides
	// whether a second remedy is needed.
	AnchorDeclinedNull   int64
	AnchorDeclinedStored int64
	// Unchanged rows already hold exactly the anchor's value.
	Unchanged int64
	// Changed is every row in the write set; NullFilled is the subset
	// going NULL -> value.
	Changed    int64
	NullFilled int64
	// NullCandidates counts rows the anchor CAN price that are stored
	// NULL, whether or not FillNull admitted them — so a dry run without
	// -fill-null still reports the coverage on offer.
	NullCandidates int64
	// BelowMinRelDelta counts rows suppressed from the write set by
	// MinRelDelta.
	BelowMinRelDelta int64

	// SumStored / SumWant are the USD sums over the CHANGED rows, so the
	// operator can read the aggregate effect of the run before running
	// it, and RelBucket counts changed rows by relative-move magnitude.
	SumStored *big.Rat
	SumWant   *big.Rat
	RelBucket map[string]int64
}

// XLMBaseRelBuckets are the relative-move magnitudes the report counts
// changed rows into. Reporting a DISTRIBUTION rather than a single
// "materially changed" number is deliberate: that count depends entirely
// on where the threshold is put (the measured population moves ~0.1-1% on
// almost every priced row and >30% on a few hundred thousand of them), and
// landing a guessed threshold on a money surface is exactly the mistake
// verify-usd-volume's own footer warns about. The operator reads the split
// and picks.
var XLMBaseRelBuckets = []struct {
	Label string
	Min   *big.Rat
}{
	{">=0.1%", big.NewRat(1, 1000)},
	{">=1%", big.NewRat(1, 100)},
	{">=10%", big.NewRat(1, 10)},
	{">=100%", big.NewRat(1, 1)},
	{">=10x", big.NewRat(9, 1)},
}

// XLMBaseRestampPlan is one window's decisions: the rows to write, and
// the accounting for every row that was scanned and not written. A dry
// run produces the plan and stops; a write run hands the SAME plan to
// [Store.ApplyXLMBaseUSDVolumeRestamp], which is what makes "-write
// writes exactly the rows the dry run reported" true by construction
// rather than by two predicates happening to agree.
type XLMBaseRestampPlan struct {
	Rows  []XLMBaseRestampRow
	Stats XLMBaseRestampStats
}

// DEXSourceNames is the ops layer's view of [dexSourceNames] — the set of
// on-chain venues whose trades the XLM-base anchor will value. Exported
// so `usd-volume-restamp` can print the scope it is about to walk.
func DEXSourceNames() []string { return dexSourceNames() }

// xlmAssetForms are the two on-chain wire forms of XLM a trade's base leg
// can carry. Mirrors [isXLMAsset]; kept as a function so the SQL scan and
// [xlmBaseTierFor]'s Go gate cannot drift.
func xlmAssetForms() []string {
	return []string{canonical.NativeAsset().String(), nativeXLMSAC}
}

// xlmBaseRestampSources resolves the scan's source list: the DEX registry
// filtered by the caller's allow-list.
func xlmBaseRestampSources(allow map[string]bool) []string {
	all := DEXSourceNames()
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

// xlmBaseRestampScan is one candidate row straight off the SELECT, before
// any Go decision has been taken. Split out so [xlmBaseRestampDecide] —
// which holds every rule this tool is judged on — is a pure function unit
// tests can drive without a database.
type xlmBaseRestampScan struct {
	Source  string
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	TS      time.Time

	BaseAsset   string
	QuoteAsset  string
	BaseAmount  string
	QuoteAmount string
	Stored      *string
	Generation  int64
}

// xlmBaseDisposition is what [xlmBaseRestampDecide] concluded about one
// scanned row. Every scanned row gets exactly one.
type xlmBaseDisposition int

const (
	// xlmBaseWrite: the anchor priced the row and the value differs.
	xlmBaseWrite xlmBaseDisposition = iota
	// xlmBaseUnchanged: the row already holds the anchor's value.
	xlmBaseUnchanged
	// xlmBaseQuotePegged: exact tier, not this tool's.
	xlmBaseQuotePegged
	// xlmBaseNotDEX: source subclass is not DEX, or base is not XLM.
	xlmBaseNotDEX
	// xlmBaseUnparseable: amounts or stored value are not decimals.
	xlmBaseUnparseable
	// xlmBaseAnchorDeclined: the XLM anchor produced no value.
	xlmBaseAnchorDeclined
	// xlmBaseSkipNull: stored NULL and FillNull is off.
	xlmBaseSkipNull
	// xlmBaseBelowMinRelDelta: differs, but by less than MinRelDelta.
	xlmBaseBelowMinRelDelta
)

// xlmBaseRestampDecide judges ONE scanned row.
//
// `anchor` is injected rather than called directly so the decision rules
// are testable without a live prices_1m; production passes a closure over
// [tradeUSDVolumeViaXLMBaseAnchor] with the store's real resolver, so the
// number that reaches the column is the number the live insert path
// computes for the same row.
func xlmBaseRestampDecide(
	row xlmBaseRestampScan,
	spec *USDVolumeQuoteSpec,
	fillNull bool,
	minRelDelta *big.Rat,
	anchor func(canonical.Trade) *string,
) (XLMBaseRestampRow, xlmBaseDisposition) {
	out := XLMBaseRestampRow{
		Source: row.Source, Ledger: row.Ledger, TxHash: row.TxHash,
		OpIndex: row.OpIndex, TS: row.TS,
		BaseAsset: row.BaseAsset, QuoteAsset: row.QuoteAsset,
		Stored: row.Stored,
	}
	trade, disp, inTier := xlmBaseRestampScope(row, spec)
	if !inTier {
		return out, disp
	}
	want := anchor(trade)
	if want == nil {
		return out, xlmBaseAnchorDeclined
	}
	out.Want = *want
	wantRat, wok := new(big.Rat).SetString(out.Want)
	if !wok {
		return out, xlmBaseUnparseable
	}
	if row.Stored == nil {
		out.NullFill = true
		out.AbsDelta = new(big.Rat).Abs(wantRat)
		if !fillNull {
			return out, xlmBaseSkipNull
		}
		return out, xlmBaseWrite
	}
	storedRat, sok := new(big.Rat).SetString(*row.Stored)
	if !sok {
		// A NUMERIC that does not render as a decimal is reportable, not
		// silently rewritable — same posture as the exact-tier tool.
		return out, xlmBaseUnparseable
	}
	if storedRat.Cmp(wantRat) == 0 {
		return out, xlmBaseUnchanged
	}
	out.AbsDelta = new(big.Rat).Abs(new(big.Rat).Sub(wantRat, storedRat))
	if storedRat.Sign() != 0 {
		out.RelDelta = new(big.Rat).Quo(out.AbsDelta, new(big.Rat).Abs(storedRat))
		out.RelOK = true
	}
	if minRelDelta != nil && minRelDelta.Sign() > 0 && out.RelOK && out.RelDelta.Cmp(minRelDelta) < 0 {
		return out, xlmBaseBelowMinRelDelta
	}
	return out, xlmBaseWrite
}

// xlmBaseRestampScope decides whether one scanned row is IN this tier and
// rebuilds it into the [canonical.Trade] the decoder produced, so the
// anchor can be asked about the same object the insert path was.
//
// The tier decision itself is [xlmBaseTierFor], which sits beside
// [tradeUSDVolume] so the branch order and the population it selects
// cannot drift apart. What is left here is the row-shaped part: parsing
// the two asset ids and the two amounts, with [tradeUSDVolume]'s own
// positive-amount bail-outs.
func xlmBaseRestampScope(row xlmBaseRestampScan, spec *USDVolumeQuoteSpec) (canonical.Trade, xlmBaseDisposition, bool) {
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
		// and the anchor bails on a non-positive base.
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
	// The SQL scan already bounds source and base_asset, but re-asserting
	// the tier in Go keeps its definition in ONE place and makes a
	// widened scan fail CLOSED rather than silently valuing, say, a
	// non-XLM base off a 1e7 divisor it may not have.
	switch xlmBaseTierFor(trade, spec) {
	case xlmBaseTierQuotePegged:
		return canonical.Trade{}, xlmBaseQuotePegged, false
	case xlmBaseTierOutOfScope:
		return canonical.Trade{}, xlmBaseNotDEX, false
	case xlmBaseTierOwns:
		return trade, xlmBaseWrite, true
	default:
		return canonical.Trade{}, xlmBaseNotDEX, false
	}
}

// PlanXLMBaseUSDVolumeRestamp scans one bounded window and returns the
// rows the XLM-base re-derive would rewrite, plus the disposition of
// every row it would not.
//
// READ-ONLY. This is the whole of the dry run and the whole of the report
// mode; [Store.ApplyXLMBaseUSDVolumeRestamp] consumes the plan verbatim.
//
// Requires the store's USD-volume resolution to be installed
// ([InstallUSDVolumeResolution]) — without a resolver the anchor declines
// every row and the run would report a fleet-wide "cannot price", which
// is a configuration error dressed as a finding. Refused up front, in the
// same spirit as [Store.reDeriveNullVolumeGuard].
func (s *Store) PlanXLMBaseUSDVolumeRestamp(ctx context.Context, p XLMBaseRestampParams) (*XLMBaseRestampPlan, error) {
	if !p.To.After(p.From) {
		return nil, fmt.Errorf("timescale: xlm-base restamp: empty window [%s, %s)", p.From, p.To)
	}
	if s.usdVolumeFXResolver == nil {
		return nil, fmt.Errorf("timescale: xlm-base restamp: no USD-volume FX resolver installed — " +
			"the XLM anchor cannot resolve XLM/USD and every row would report as unpriceable; " +
			"call InstallUSDVolumeResolution with the operator's usd_pegged_classic_assets first")
	}
	sources := xlmBaseRestampSources(p.Sources)
	if len(sources) == 0 {
		return &XLMBaseRestampPlan{Stats: NewXLMBaseRestampStats()}, nil
	}
	anchor := func(t canonical.Trade) *string {
		return tradeUSDVolumeViaXLMBaseAnchorFor(ctx, t, s.usdVolumeFXResolver)
	}

	rows, err := s.db.QueryContext(ctx, xlmBaseRestampSelect,
		p.From.UTC(), p.To.UTC(), sources, xlmAssetForms(), p.MaxGeneration)
	if err != nil {
		return nil, fmt.Errorf("timescale: xlm-base restamp scan [%s, %s): %w",
			p.From.Format(time.RFC3339), p.To.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	plan := &XLMBaseRestampPlan{Stats: NewXLMBaseRestampStats()}
	for rows.Next() {
		var (
			r      xlmBaseRestampScan
			ledger int64
			opIdx  int64
			stored sql.NullString
		)
		if err := rows.Scan(&r.Source, &ledger, &r.TxHash, &opIdx, &r.TS,
			&r.BaseAsset, &r.QuoteAsset, &r.BaseAmount, &r.QuoteAmount,
			&stored, &r.Generation); err != nil {
			return nil, fmt.Errorf("timescale: xlm-base restamp scan row: %w", err)
		}
		//nolint:gosec // ledger/op_index are non-negative `integer` columns (CHECK-constrained in migration 0001)
		r.Ledger, r.OpIndex = uint32(ledger), uint32(opIdx)
		if stored.Valid {
			v := stored.String
			r.Stored = &v
		}
		r.TS = r.TS.UTC()
		decision, disp := xlmBaseRestampDecide(r, s.usdVolumeQuoteSpec, p.FillNull, p.MinRelDelta, anchor)
		plan.Record(decision, disp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: xlm-base restamp scan rows [%s, %s): %w",
			p.From.Format(time.RFC3339), p.To.Format(time.RFC3339), err)
	}
	return plan, nil
}

// NewXLMBaseRestampStats initialises the sums + bucket map so callers
// never have to nil-check them.
func NewXLMBaseRestampStats() XLMBaseRestampStats {
	return XLMBaseRestampStats{
		SumStored: new(big.Rat),
		SumWant:   new(big.Rat),
		RelBucket: map[string]int64{},
	}
}

// Record files one decided row into the plan. Exported so the ops layer's
// tests can build a plan without a database and still exercise the SAME
// accounting the planner uses.
func (p *XLMBaseRestampPlan) Record(row XLMBaseRestampRow, disp xlmBaseDisposition) {
	p.Stats.Scanned++
	switch disp {
	case xlmBaseQuotePegged:
		p.Stats.QuotePegged++
	case xlmBaseNotDEX:
		p.Stats.NotDEX++
	case xlmBaseUnparseable:
		p.Stats.Unparseable++
	case xlmBaseAnchorDeclined:
		if row.Stored == nil {
			p.Stats.AnchorDeclinedNull++
		} else {
			p.Stats.AnchorDeclinedStored++
		}
	case xlmBaseUnchanged:
		p.Stats.Unchanged++
	case xlmBaseSkipNull:
		p.Stats.NullCandidates++
	case xlmBaseBelowMinRelDelta:
		p.Stats.BelowMinRelDelta++
	case xlmBaseWrite:
		p.recordChanged(row)
	}
}

// recordChanged files one row of the WRITE set: the counters, the USD
// sums and the relative-move distribution the report prints.
func (p *XLMBaseRestampPlan) recordChanged(row XLMBaseRestampRow) {
	p.Stats.Changed++
	if row.NullFill {
		p.Stats.NullFilled++
		p.Stats.NullCandidates++
	}
	if row.Stored != nil {
		if v, ok := new(big.Rat).SetString(*row.Stored); ok {
			p.Stats.SumStored.Add(p.Stats.SumStored, v)
		}
	}
	if v, ok := new(big.Rat).SetString(row.Want); ok {
		p.Stats.SumWant.Add(p.Stats.SumWant, v)
	}
	if row.RelOK {
		for _, b := range XLMBaseRelBuckets {
			if row.RelDelta.Cmp(b.Min) >= 0 {
				p.Stats.RelBucket[b.Label]++
			}
		}
	}
	p.Rows = append(p.Rows, row)
}

// Merge folds another window's statistics into this one. The per-row
// detail is deliberately NOT carried: 28M candidate rows do not belong in
// one process's memory, so the caller keeps only the extremes and a
// sample and merges the counters.
func (s *XLMBaseRestampStats) Merge(o XLMBaseRestampStats) {
	s.Scanned += o.Scanned
	s.QuotePegged += o.QuotePegged
	s.NotDEX += o.NotDEX
	s.Unparseable += o.Unparseable
	s.AnchorDeclinedNull += o.AnchorDeclinedNull
	s.AnchorDeclinedStored += o.AnchorDeclinedStored
	s.Unchanged += o.Unchanged
	s.Changed += o.Changed
	s.NullFilled += o.NullFilled
	s.NullCandidates += o.NullCandidates
	s.BelowMinRelDelta += o.BelowMinRelDelta
	if o.SumStored != nil {
		s.SumStored.Add(s.SumStored, o.SumStored)
	}
	if o.SumWant != nil {
		s.SumWant.Add(s.SumWant, o.SumWant)
	}
	for k, v := range o.RelBucket {
		s.RelBucket[k] += v
	}
}

// Residual is the reconciliation check on a window's accounting: scanned
// minus every disposition. A non-zero residual means a row was scanned
// and filed nowhere — the shape of bug that makes a report quietly
// understate a population — so the tool prints it rather than trusting it.
//
// NullCandidates is excluded because it is a CROSS-CUT (it double-counts
// the NULL rows that are also in Changed/NullFilled), not a disposition.
func (s XLMBaseRestampStats) Residual() int64 {
	filed := s.QuotePegged + s.NotDEX + s.Unparseable +
		s.AnchorDeclinedNull + s.AnchorDeclinedStored +
		s.Unchanged + s.Changed + s.BelowMinRelDelta +
		(s.NullCandidates - s.NullFilled)
	return s.Scanned - filed
}

// xlmBaseRestampApplyBatch is the default number of rows one UPDATE
// transaction carries. The historical span lives in COMPRESSED chunks, so
// every touched row is decompressed inside the transaction; a bounded
// batch keeps the decompression footprint, the lock footprint and the
// rollback cost bounded, and makes an interrupted run cheap to resume
// (only the in-flight batch is lost, and the tool is idempotent).
const xlmBaseRestampApplyBatch = 2000

// ApplyXLMBaseUSDVolumeRestamp writes the plan's rows and returns how many
// rows the database actually changed.
//
// Discipline, matching [Store.RestampExactTierUSDVolume]:
//
//   - the write set is the PLAN — the same slice the dry run printed. No
//     second predicate is evaluated against the table, so a row cannot be
//     written that the preview did not name.
//   - INV-3: each row is written only while
//     `trades.derive_generation <= generation`, and is stamped with it, so
//     a later live gen-0 replay cannot claw the correction back and a
//     NEWER re-derive cannot be overwritten by this one.
//   - one bounded transaction per batch, each lifting the Timescale
//     decompression cap with `SET LOCAL` — POSTGRES unwinds that at
//     COMMIT/ROLLBACK, so the lifted cap can never escape onto a pooled
//     connection (the pgx-stdlib finding behind CHANGELOG 2026-08).
//   - `batch <= 0` uses [xlmBaseRestampApplyBatch].
func (s *Store) ApplyXLMBaseUSDVolumeRestamp(ctx context.Context, plan *XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
	if plan == nil || len(plan.Rows) == 0 {
		return 0, nil
	}
	if batch <= 0 {
		batch = xlmBaseRestampApplyBatch
	}
	var total int64
	for start := 0; start < len(plan.Rows); start += batch {
		end := start + batch
		if end > len(plan.Rows) {
			end = len(plan.Rows)
		}
		n, err := s.applyXLMBaseRestampBatch(ctx, plan.Rows[start:end], generation)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// applyXLMBaseRestampBatch writes one bounded batch in its own
// transaction. Split from the loop so the transaction's lifetime is a
// single function body and cannot accidentally span batches.
func (s *Store) applyXLMBaseRestampBatch(ctx context.Context, rows []XLMBaseRestampRow, generation int64) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	args := []any{generation}
	var values strings.Builder
	for i, r := range rows {
		if i > 0 {
			values.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&values, "($%d::text, $%d::integer, $%d::text, $%d::integer, $%d::timestamptz, $%d::numeric)",
			n+1, n+2, n+3, n+4, n+5, n+6)
		args = append(args, r.Source, int64(r.Ledger), r.TxHash, int64(r.OpIndex), r.TS.UTC(), r.Want)
	}
	// Every fragment is code-built here; all values (the generation and
	// each row's primary key + value) travel as positional placeholders —
	// gosec G202 is a false positive.
	//nolint:gosec // no caller-supplied text reaches the statement
	q := `UPDATE trades t
	         SET usd_volume = v.usd_volume, derive_generation = $1
	        FROM (VALUES ` + values.String() + `) AS v(source, ledger, tx_hash, op_index, ts, usd_volume)
	       WHERE t.source   = v.source
	         AND t.ledger   = v.ledger
	         AND t.tx_hash  = v.tx_hash
	         AND t.op_index = v.op_index
	         AND t.ts       = v.ts
	         AND t.derive_generation <= $1`

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("timescale: xlm-base restamp begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0"); err != nil {
		return 0, fmt.Errorf("timescale: xlm-base restamp: raise decompression cap: %w", err)
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("timescale: xlm-base restamp update (%d rows from %s): %w",
			len(rows), rows[0].TS.Format(time.RFC3339), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: xlm-base restamp rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("timescale: xlm-base restamp commit (%d rows from %s): %w",
			len(rows), rows[0].TS.Format(time.RFC3339), err)
	}
	return n, nil
}

// MaxTradeLedgerInRange returns the highest on-chain ledger sequence
// carried by any trade in [from, to), and ok=false when the range holds
// no on-chain rows at all.
//
// It exists for the live-overlap guard: a restamp must only ever rewrite
// history the live ingest tail has already passed, and the tail's position
// is a LEDGER (the `ledgerstream` cursor), not a timestamp. Off-chain rows
// carry `ledger = 0` (migration 0004) and are excluded — they have no
// place on the ledger axis and would otherwise drag the answer to 0.
//
// Callers scope this to ONE day so the scan stays inside a single chunk.
func (s *Store) MaxTradeLedgerInRange(ctx context.Context, from, to time.Time) (uint32, bool, error) {
	const q = `SELECT max(ledger) FROM trades WHERE ts >= $1 AND ts < $2 AND ledger > 0`
	var maxLedger sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q, from.UTC(), to.UTC()).Scan(&maxLedger); err != nil {
		return 0, false, fmt.Errorf("timescale: max trade ledger [%s, %s): %w",
			from.Format(time.RFC3339), to.Format(time.RFC3339), err)
	}
	if !maxLedger.Valid || maxLedger.Int64 <= 0 {
		return 0, false, nil
	}
	//nolint:gosec // ledger is a positive `integer` column (CHECK ledger > 0)
	return uint32(maxLedger.Int64), true, nil
}
