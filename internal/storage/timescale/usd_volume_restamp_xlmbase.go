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

// ─── the XLM-BASE usd_volume RE-DERIVE ───────────────────────────────
//
// [usd_volume_restamp.go] repairs the EXACT tiers, whose value is a pure
// decimal rescaling of an amount already on the row and can therefore be
// evaluated in SQL. This file is the ESTIMATED-tier counterpart for the
// one estimated tier whose input is not authorable by a counterparty: the
// XLM-base anchor (`usd_volume = base_amount/1e7 x XLM/USD at ts`,
// [usdVolumeViaXLMBaseAnchor]).
//
// The population: every on-chain DEX trade with an XLM BASE leg, with a
// non-USD-pegged quote, whose `usd_volume` the anchor tier would have
// written differently. Rows a later re-derive already stamped carry a
// higher `derive_generation`, so the window ends the day before that
// re-derive's first row (the runbook gives the `-to` value).
//
// It is mostly coverage, not valuation: per month the PRICED rows aggregate to
// within 0.3% of the anchor, and the gap is rows stored NULL. So the bulk of a
// run is `-fill-null`; wrong-leg corrections (an `XLM/<token>` trade valued
// through the token's own thin `<token>/USDC` bucket) are a long tail. A
// CAGG-derived ratio misreads this: `prices_1m`/`prices_1d` coalesce a NULL
// `usd_volume` to 0, so a coverage hole reads as a valuation error.
//
// Three rules make this a re-derive rather than a guess, and they are the
// reason the arithmetic is NOT done in SQL:
//
//  1. THE VALUE COMES FROM THE LIVE FUNCTION. Each candidate row is
//     rebuilt into the [canonical.Trade] the decoder produced and handed
//     to [usdVolumeViaXLMBaseAnchor] — the same function
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
//     quote-side estimate here would re-commit the error this tool
//     corrects, and it would do so at a HIGH `derive_generation`, which is
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

// The candidate scan for one bounded window is the shared one
// ([restampScanSelect], usd_volume_restamp_legs.go) with this tier's leg:
// `base_asset = ANY(<the two XLM wire forms>)`.

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

	// FillNull admits rows whose stored usd_volume is NULL. Off by default:
	// filling an unpriced row is a COVERAGE change, which the operator opts
	// into rather than a value-repair tool doing it silently. The planner
	// counts the NULL population either way, so a dry run always shows what
	// -fill-null would add.
	FillNull bool

	// MaxGeneration is the derive_generation read guard: rows already stamped by a
	// LATER re-derive are not candidates. Callers pass the run's own
	// generation (the same value the UPDATE writes), or a lower value to
	// target a specific vintage — e.g. 0 for the never-re-derived
	// population.
	MaxGeneration int64

	// MinRelDelta, when non-nil and positive, suppresses rows whose
	// relative move |new-old|/|old| is below it from the WRITE set. Nil
	// (the default) writes every row whose value differs at all. It never
	// suppresses a NULL->value fill, which has no relative move to
	// measure.
	MinRelDelta *big.Rat
}

// XLMBaseRestampRow is one row the re-derive would rewrite: its primary
// key, what is stored now, and the value [usdVolumeViaXLMBaseAnchor]
// produces for it today.
type XLMBaseRestampRow struct {
	Source  string
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	TS      time.Time

	BaseAsset  string
	QuoteAsset string

	// BaseAmount, QuoteAmount and Generation are the row as the plan read
	// it; the apply writes only while the row still matches them and Stored.
	BaseAmount  string
	QuoteAmount string
	Generation  int64

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
	// FXDeclinedStale counts rows the cex-fx tier refused because the
	// nearest fx_quote at or before the trade is outside the as-of
	// tolerance (see [Store.PlanCEXFiatUSDVolumeRestamp]). A CROSS-CUT of
	// AnchorDeclinedNull/Stored — it says WHY those rows were declined,
	// not where they were filed — so [XLMBaseRestampStats.Residual]
	// ignores it, exactly as it ignores NullCandidates.
	FXDeclinedStale int64

	// SumStored / SumWant are the USD sums over the CHANGED rows, so the
	// operator can read the aggregate effect of the run before running
	// it, and RelBucket counts changed rows by relative-move magnitude.
	SumStored *big.Rat
	SumWant   *big.Rat
	RelBucket map[string]int64
}

// XLMBaseRelBuckets are the relative-move magnitudes the report counts changed
// rows into. Reporting a DISTRIBUTION rather than a single "materially
// changed" number is deliberate: that count depends entirely on where the
// threshold is put, and landing a guessed threshold on a money surface is
// exactly the mistake verify-usd-volume's own footer warns about. The operator
// reads the split and picks.
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
	return []string{canonical.NativeAsset().String(), canonical.NativeSACContractID()}
}

// xlmBaseRestampSources resolves the scan's source list: the DEX registry
// filtered by the caller's allow-list.
func xlmBaseRestampSources(allow map[string]bool) []string {
	return restampScanSources(DEXSourceNames(), allow)
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

// xlmBaseRestampDecide judges ONE scanned row for the xlm-base tier: the
// shared decision skeleton ([restampDecide]) with this tier's gate.
//
// `anchor` is injected rather than called directly so the decision rules
// are testable without a live prices_1m; production passes a closure over
// [usdVolumeViaXLMBaseAnchor] with the store's real resolver, so the
// number that reaches the column is the number the live insert path
// computes for the same row.
func xlmBaseRestampDecide(
	row xlmBaseRestampScan,
	spec *USDVolumeQuoteSpec,
	fillNull bool,
	minRelDelta *big.Rat,
	anchor func(canonical.Trade) *string,
) (XLMBaseRestampRow, xlmBaseDisposition) {
	// A test seam over an infallible anchor: the planner goes through
	// [restampDecide] directly, where a resolver read error aborts the run.
	row2, disp, _ := restampDecide(row, spec, xlmBaseTierFor,
		func(t canonical.Trade) (*string, error) { return anchor(t), nil }, fillNull, minRelDelta)
	return row2, disp
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
	return s.planRestampTier(ctx, p, restampTierScan{
		Tier:    "xlm-base",
		Sources: xlmBaseRestampSources(p.Sources),
		Leg:     restampLegBase,
		Assets:  xlmAssetForms(),
		Gate:    xlmBaseTierFor,
		Value: func(t canonical.Trade) (*string, error) {
			return tradeUSDVolumeViaXLMBaseAnchorFor(ctx, t, s.usdVolumeFXResolver)
		},
	})
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
	s.FXDeclinedStale += o.FXDeclinedStale
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

// xlmBaseRestampMaxBatch caps the rows one UPDATE transaction can carry,
// whatever the operator asked for. The statement binds
// [xlmBaseRestampArgsPerRow] placeholders per row plus 3 fixed ones (the generation and the two `ts` bounds), and the
// extended query protocol carries at most 65,535 parameters — pgx refuses
// the Exec above that, mid-run, after the rest of the walk has already
// paid for itself. `-chunk-batch` defaults to 20,000, so an hour busy
// enough to change more than this many rows would abort the run rather
// than slow it; the cap turns that into one more transaction.
const xlmBaseRestampMaxBatch = (65535 - 3) / xlmBaseRestampArgsPerRow

// xlmBaseRestampArgsPerRow is the placeholders one row binds in the batch
// UPDATE: its primary key, the new value, and the plan-read state the row
// must still match.
const xlmBaseRestampArgsPerRow = 10

// ApplyXLMBaseUSDVolumeRestamp writes the plan's rows and returns how many
// rows the database actually changed.
//
// Discipline, matching [Store.RestampExactTierUSDVolume]:
//
//   - the write set is the PLAN — the same slice the dry run printed. No
//     second predicate is evaluated against the table, so a row cannot be
//     written that the preview did not name.
//   - each row is written only while its amounts, usd_volume and
//     derive_generation still equal what the plan read: a row another
//     writer moved after the plan is skipped, and surfaces as
//     planned-vs-changed, rather than overwritten from its old state.
//   - each row is written only while
//     `trades.derive_generation <= generation`, and is stamped with it, so
//     a later live gen-0 replay cannot claw the correction back and a
//     NEWER re-derive cannot be overwritten by this one.
//   - one bounded transaction per batch, each lifting the Timescale
//     decompression cap with `SET LOCAL` — POSTGRES unwinds that at
//     COMMIT/ROLLBACK, so the lifted cap can never escape onto a pooled
//     connection.
//   - `batch <= 0` uses [xlmBaseRestampApplyBatch].
func (s *Store) ApplyXLMBaseUSDVolumeRestamp(ctx context.Context, plan *XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
	return s.ApplyUSDVolumeRestampPlan(ctx, plan, generation, batch)
}

// applyXLMBaseRestampBatches is the batch loop behind both applies. `before`,
// when not nil, runs ahead of EVERY batch and a non-nil error stops the loop
// with the rows written so far — the chunk walk's is-the-chunk-still-
// decompressed guard ([Store.ApplyXLMBaseUSDVolumeRestampInChunk]).
func (s *Store) applyXLMBaseRestampBatches(ctx context.Context, plan *XLMBaseRestampPlan, generation int64, batch int, before func(context.Context) error) (int64, error) {
	if plan == nil || len(plan.Rows) == 0 {
		return 0, nil
	}
	if batch <= 0 {
		batch = xlmBaseRestampApplyBatch
	}
	if batch > xlmBaseRestampMaxBatch {
		batch = xlmBaseRestampMaxBatch
	}
	var total int64
	for start := 0; start < len(plan.Rows); start += batch {
		if before != nil {
			if err := before(ctx); err != nil {
				return total, err
			}
		}
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

// ─── why the batch UPDATE carries a redundant `ts` range ────────────────
//
// `trades` is a hypertable with 260 chunks, 258 of them compressed. An
// UPDATE that names `trades` and constrains `ts` only through a join
// clause (`t.ts = v.ts`) cannot be pruned at planning time, so the plan
// makes EVERY chunk a result relation and hash-joins the batch against an
// Append over all of them. Measured on production: the plan
// for one batch carried 260 `Update on …_chunk` targets, an Append of 260
// sequential scans estimated at 61.9M rows, cost 10,040,409 — and because
// TimescaleDB has to service the DML on each compressed chunk in that
// list, and none of the join clauses can be turned into a scan key on a
// `segmentby`/`orderby` column, it decompressed the chunks WHOLESALE. A
// 23-row UPDATE ran 60 minutes, wrote ~270 GB of WAL (a 50x jump over the
// box's baseline, ending the second it was cancelled), left 119.7M dead
// tuples across 55 compressed chunks it had no rows in, and changed
// nothing. That — not per-row decompression inside the targeted chunk —
// is what made both the day walk and the chunk walk crawl.
//
// The remedy is information, not a different write: `t.ts = v.ts` already
// forces every matched row's `ts` to be one of the batch's own values, so
// bounding `t.ts` by the batch's own minimum and maximum cannot exclude a row
// the join would have matched. It is a provably redundant predicate that the
// planner can prune on. With it the same batch plans as ONE result relation
// over a nested loop / merge join driven by an index — cost 61.99 at 23 rows,
// 18,614 at 10,000.
//
// Two smaller shapes hang off the same statement:
//
//   - `tx_hash` is `char(64)`. Binding it as `text` makes the comparison
//     `(t.tx_hash)::text = v.tx_hash`, which no index on the column can
//     serve; binding it as `bpchar` lets the join ride `trades_pkey` on
//     all five key columns (`Inner Unique: true`, cost 2.92 a probe).
//   - the pruning only happens in a CUSTOM plan. Batches of equal length
//     produce identical statement text, so pgx reuses one prepared
//     statement and Postgres may promote it to a generic plan, which
//     cannot know the bounds and would silently restore the 260-chunk
//     plan. `SET LOCAL plan_cache_mode = force_custom_plan` pins it, and
//     LOCAL keeps it off the pooled connection the same way the
//     decompression cap is kept off it.

// xlmBaseRestampTSBounds returns the inclusive `ts` span of one batch.
// The planner scan orders by `ts`, so a batch is normally contiguous and
// these are its first and last row — but the bound is computed rather
// than assumed, because a reordering of that scan must not silently
// narrow the range the UPDATE is allowed to match.
func xlmBaseRestampTSBounds(rows []XLMBaseRestampRow) (lo, hi time.Time) {
	lo, hi = rows[0].TS.UTC(), rows[0].TS.UTC()
	for _, r := range rows[1:] {
		switch ts := r.TS.UTC(); {
		case ts.Before(lo):
			lo = ts
		case ts.After(hi):
			hi = ts
		}
	}
	return lo, hi
}

// applyXLMBaseRestampBatch writes one bounded batch in its own
// transaction. Split from the loop so the transaction's lifetime is a
// single function body and cannot accidentally span batches.
func (s *Store) applyXLMBaseRestampBatch(ctx context.Context, rows []XLMBaseRestampRow, generation int64) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	lo, hi := xlmBaseRestampTSBounds(rows)
	args := []any{generation, lo, hi}
	var values strings.Builder
	for i, r := range rows {
		if i > 0 {
			values.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&values, "($%d::text, $%d::integer, $%d::bpchar, $%d::integer, $%d::timestamptz, $%d::numeric, "+
			"$%d::numeric, $%d::numeric, $%d::numeric, $%d::bigint)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10)
		var stored any
		if r.Stored != nil {
			stored = *r.Stored
		}
		args = append(args, r.Source, int64(r.Ledger), r.TxHash, int64(r.OpIndex), r.TS.UTC(), r.Want,
			r.BaseAmount, r.QuoteAmount, stored, r.Generation)
	}
	// Every fragment is code-built here; all values (the generation, the
	// two `ts` bounds and each row's primary key + value) travel as
	// positional placeholders.
	return s.restampTradesUSDVolume(ctx, usdVolumeRestampWrite{
		label: "xlm-base restamp",
		scope: fmt.Sprintf("(%d rows from %s)", len(rows), rows[0].TS.Format(time.RFC3339)),
		rel:   `(VALUES ` + values.String() + `) AS v(source, ledger, tx_hash, op_index, ts, usd_volume, base_amount, quote_amount, prior_usd_volume, prior_generation)`,
		where: `
	       WHERE t.ts      >= $2
	         AND t.ts      <= $3
	         AND t.source   = v.source
	         AND t.ledger   = v.ledger
	         AND t.tx_hash  = v.tx_hash
	         AND t.op_index = v.op_index
	         AND t.ts       = v.ts
	         AND t.derive_generation <= $1
	         AND t.base_amount  = v.base_amount
	         AND t.quote_amount = v.quote_amount
	         AND t.usd_volume IS NOT DISTINCT FROM v.prior_usd_volume
	         AND t.derive_generation = v.prior_generation`,
		value: "v.usd_volume",
		gen:   "$1",
		args:  args,
	})
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
