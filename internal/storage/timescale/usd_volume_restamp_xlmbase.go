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

// The XLM-BASE usd_volume re-derive: the estimated-tier counterpart of [usd_volume_restamp.go]
// (`usd_volume = base_amount/1e7 x XLM/USD at ts`). Most of the population is coverage
// (`-fill-null`), not correction; prices_1m/prices_1d coalesce NULL to 0, so a CAGG ratio misreads
// holes as valuation errors.
//
// The arithmetic stays in Go, not SQL:
//   1. Each row is rebuilt into its [canonical.Trade] and valued by the live
//      [usdVolumeViaXLMBaseAnchor], so there is no second waterfall to drift.
//   2. A row the anchor declines is REPORTED, never valued via [tradeUSDVolumeViaFX]: a wrong value
//      at a high `derive_generation` cannot be clawed back; an unpriced one can.
//   3. A declined row that carries a number is never overwritten with NULL; it is counted in
//      [XLMBaseRestampStats.AnchorDeclinedStored].
//
// Pegged quotes belong to `-tier exact`. The scan is the shared [restampScanSelect] with
// `base_asset = ANY(<the two XLM wire forms>)`.

// XLMBaseRestampParams scopes one window of the XLM-base re-derive.
type XLMBaseRestampParams struct {
	// [From, To) bounds the rows by ts. Callers slice a day into bounded windows so no scan or
	// UPDATE decompresses a whole chunk in one transaction.
	From, To time.Time

	// Sources, when non-empty, restricts the scan to these names. Names outside the DEX registry
	// are dropped: the anchor declines them and scanning them only widens the read.
	Sources map[string]bool

	// FillNull admits rows whose stored usd_volume is NULL. Off by default: filling an unpriced
	// row is a COVERAGE change the operator opts into. A dry run counts the NULL population anyway.
	FillNull bool

	// MaxGeneration is the derive_generation read guard: rows stamped by a LATER re-derive are not
	// candidates. Pass the run's own generation, or a lower one to target a vintage.
	MaxGeneration int64

	// MinRelDelta, when non-nil and positive, drops rows whose relative move |new-old|/|old| is
	// below it from the WRITE set; nil writes every differing row. It never suppresses a
	// NULL->value fill, which has no relative move.
	MinRelDelta *big.Rat
}

// XLMBaseRestampRow is one row the re-derive would rewrite: its primary key, the stored value, and the value
// [usdVolumeViaXLMBaseAnchor] produces for it.
type XLMBaseRestampRow struct {
	Source  string
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	TS      time.Time

	BaseAsset  string
	QuoteAsset string

	// BaseAmount, QuoteAmount and Generation are the row as the plan read it; the apply writes
	// only while the row still matches them and Stored.
	BaseAmount  string
	QuoteAmount string
	Generation  int64

	// Stored is the current column value; nil for SQL NULL.
	Stored *string
	// Want is the anchor's value, rendered as the insert path renders it (FloatString(8)); Basis is the tier's.
	Want  string
	Basis USDBasis

	// AbsDelta is |Want - Stored| in USD; equal to Want for a NULL fill.
	AbsDelta *big.Rat
	// RelDelta is |Want - Stored| / |Stored|. RelOK is false when Stored is NULL or zero.
	RelDelta *big.Rat
	RelOK    bool
	// NullFill marks a row that goes from SQL NULL to a value.
	NullFill bool
}

// XLMBaseRestampStats is the accounting for one planned window.
type XLMBaseRestampStats struct {
	// Scanned is every row the SQL predicate returned.
	Scanned int64
	// QuotePegged rows have a USD-pegged quote leg: exact tier, owned by `-tier exact`.
	QuotePegged int64
	// NotDEX rows came from a non-DEX source or a non-XLM base leg. Expected zero; counted so a
	// registry re-class shows up instead of shrinking the population.
	NotDEX int64
	// Unparseable rows carry an amount or stored usd_volume that is not a positive decimal.
	Unparseable int64
	// AnchorDeclinedNull / AnchorDeclinedStored are rows the anchor cannot price, split by what
	// they hold. The first is unrecoverable coverage; the second stays WRONG after a clean run.
	AnchorDeclinedNull   int64
	AnchorDeclinedStored int64
	// Unchanged rows already hold exactly the anchor's value.
	Unchanged int64
	// Changed is every row in the write set; NullFilled is the subset going NULL -> value.
	Changed    int64
	NullFilled int64
	// NullCandidates counts rows the anchor CAN price that are stored NULL, whether or not
	// FillNull admitted them, so a dry run still reports the coverage on offer.
	NullCandidates int64
	// BelowMinRelDelta counts rows suppressed from the write set by MinRelDelta.
	BelowMinRelDelta int64
	// FXDeclinedStale counts rows the cex-fx tier refused because the nearest earlier fx_quote
	// is outside the as-of tolerance. A CROSS-CUT of AnchorDeclinedNull/Stored, so
	// [XLMBaseRestampStats.Residual] ignores it.
	FXDeclinedStale int64

	// SumStored / SumWant are the USD sums over the CHANGED rows, so the operator can read the
	// run's aggregate effect up front; RelBucket counts changed rows by relative-move magnitude.
	SumStored *big.Rat
	SumWant   *big.Rat
	RelBucket map[string]int64
}

// XLMBaseRelBuckets are the relative-move magnitudes the report counts changed rows into.
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

// XLMBaseRestampPlan is one window's decisions: the rows to write plus the accounting for every row scanned and not written; a write
// run hands the SAME plan to ApplyXLMBaseUSDVolumeRestamp, so -write writes exactly what the dry run reported.
type XLMBaseRestampPlan struct {
	Rows  []XLMBaseRestampRow
	Stats XLMBaseRestampStats
}

// DEXSourceNames is the on-chain venues whose trades the XLM-base anchor will value, exported so `usd-volume-restamp` can print its scope.
func DEXSourceNames() []string { return dexSourceNames() }

// xlmAssetForms are the two on-chain wire forms of XLM a base leg can carry. Mirrors [isXLMAsset],
// so the SQL scan and [xlmBaseTierFor]'s Go gate cannot drift.
func xlmAssetForms() []string {
	return []string{canonical.NativeAsset().String(), canonical.NativeSACContractID()}
}

// xlmBaseRestampSources resolves the scan's sources: the DEX registry filtered by the allow-list.
func xlmBaseRestampSources(allow map[string]bool) []string {
	return restampScanSources(DEXSourceNames(), allow)
}

// xlmBaseRestampScan is one candidate row straight off the SELECT. It keeps [xlmBaseRestampDecide],
// which holds every rule this tool is judged on, a pure function testable without a database.
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

// xlmBaseDisposition is what [xlmBaseRestampDecide] concluded about one row; every row gets one.
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

// xlmBaseRestampDecide judges ONE scanned row for the xlm-base tier: the shared skeleton ([restampDecide]) with this tier's gate.
// `anchor` is injected for testability; production passes [usdVolumeViaXLMBaseAnchor] so the stored number matches the live insert path.
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

// PlanXLMBaseUSDVolumeRestamp scans one bounded window and returns the rows the XLM-base re-derive would rewrite plus the disposition
// of every row it would not; READ-ONLY, the whole dry run, and ApplyXLMBaseUSDVolumeRestamp consumes the plan verbatim.
func (s *Store) PlanXLMBaseUSDVolumeRestamp(ctx context.Context, p XLMBaseRestampParams) (*XLMBaseRestampPlan, error) {
	return s.planRestampTier(ctx, p, restampTierScan{
		Tier:    "xlm-base",
		Basis:   BasisXLMBase,
		Sources: xlmBaseRestampSources(p.Sources),
		Leg:     restampLegBase,
		Assets:  xlmAssetForms(),
		Gate:    xlmBaseTierFor,
		Value: func(t canonical.Trade) (*string, error) {
			return tradeUSDVolumeViaXLMBaseAnchorFor(ctx, t, s.usdVolumeFXResolver)
		},
	})
}

// NewXLMBaseRestampStats initialises the sums and bucket map so callers need no nil checks.
func NewXLMBaseRestampStats() XLMBaseRestampStats {
	return XLMBaseRestampStats{
		SumStored: new(big.Rat),
		SumWant:   new(big.Rat),
		RelBucket: map[string]int64{},
	}
}

// Record files one decided row into the plan.
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

// recordChanged files one row of the WRITE set into the counters, USD sums and move distribution.
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

// Merge folds another window's statistics into this one; per-row detail is deliberately not carried, so callers keep only the extremes
// and a sample.
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

// Residual is scanned minus every disposition; non-zero means a row was filed nowhere, which would understate a population, and
// NullCandidates is a cross-cut, not a disposition.
func (s XLMBaseRestampStats) Residual() int64 {
	filed := s.QuotePegged + s.NotDEX + s.Unparseable +
		s.AnchorDeclinedNull + s.AnchorDeclinedStored +
		s.Unchanged + s.Changed + s.BelowMinRelDelta +
		(s.NullCandidates - s.NullFilled)
	return s.Scanned - filed
}

// xlmBaseRestampApplyBatch is the default rows per UPDATE tx: bounds decompression, lock and rollback cost in COMPRESSED chunks;
// an interrupted run loses only the in-flight batch (idempotent).
const xlmBaseRestampApplyBatch = 2000

// xlmBaseRestampMaxBatch caps rows per UPDATE tx: the extended protocol carries at most 65,535 parameters ([xlmBaseRestampArgsPerRow]
// per row plus 3 fixed); above that pgx refuses the Exec mid-run.
const xlmBaseRestampMaxBatch = (65535 - 3) / xlmBaseRestampArgsPerRow

// xlmBaseRestampArgsPerRow is the placeholders one row binds: primary key, new value and basis, and the
// plan-read state it must still match.
const xlmBaseRestampArgsPerRow = 11

// ApplyXLMBaseUSDVolumeRestamp writes the plan's rows and returns how many the database changed; the write set is the plan the dry run
// printed, with no second predicate evaluated.
func (s *Store) ApplyXLMBaseUSDVolumeRestamp(ctx context.Context, plan *XLMBaseRestampPlan, generation int64, batch int) (int64, error) {
	return s.ApplyUSDVolumeRestampPlan(ctx, plan, generation, batch)
}

// applyXLMBaseRestampBatches is the batch loop behind both applies; a non-nil `before` runs ahead of EVERY batch and its error stops
// the loop with rows written so far ([Store.ApplyXLMBaseUSDVolumeRestampInChunk]).
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

// The batch UPDATE carries a redundant `ts` range because `trades` is mostly compressed chunks: a
// `ts` constrained only through the join (`t.ts = v.ts`) cannot be pruned at planning time, so
// every chunk is decompressed wholesale, producing enormous WAL for a handful of rows. The join
// already forces matched `ts` into the batch, so bounding by their min and max excludes no row.
//   - `tx_hash` is `char(64)`: bound as `text` the comparison becomes `(t.tx_hash)::text =
//     v.tx_hash`, which no index serves; bound as `bpchar` the join rides `trades_pkey`.
//   - Pruning needs a CUSTOM plan (a promoted generic plan cannot know the bounds), so
//     `SET LOCAL plan_cache_mode = force_custom_plan` pins it.

// xlmBaseRestampTSBounds returns the inclusive `ts` span of a batch, computed rather than assumed first/last row so a planner reorder
// cannot narrow the UPDATE.
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

// applyXLMBaseRestampBatch writes one bounded batch in its own transaction.
func (s *Store) applyXLMBaseRestampBatch(ctx context.Context, rows []XLMBaseRestampRow, generation int64) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	lo, hi := xlmBaseRestampTSBounds(rows)
	args := []any{generation, lo, hi}
	var values strings.Builder
	for i, r := range rows {
		if r.Basis == "" {
			// A blank basis would land as '' beside a non-NULL usd_volume.
			return 0, fmt.Errorf("timescale: xlm-base restamp: %s ledger %d op %d planned without a basis", r.Source, r.Ledger, r.OpIndex)
		}
		if i > 0 {
			values.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&values, "($%d::text, $%d::integer, $%d::bpchar, $%d::integer, $%d::timestamptz, $%d::numeric, "+
			"$%d::numeric, $%d::numeric, $%d::numeric, $%d::bigint, $%d::text)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10, n+11)
		var stored any
		if r.Stored != nil {
			stored = *r.Stored
		}
		args = append(args, r.Source, int64(r.Ledger), r.TxHash, int64(r.OpIndex), r.TS.UTC(), r.Want,
			r.BaseAmount, r.QuoteAmount, stored, r.Generation, string(r.Basis))
	}
	// Fragments are code-built; every value travels as a positional placeholder.
	return s.restampTradesUSDVolume(ctx, usdVolumeRestampWrite{
		label: "xlm-base restamp",
		scope: fmt.Sprintf("(%d rows from %s)", len(rows), rows[0].TS.Format(time.RFC3339)),
		rel:   `(VALUES ` + values.String() + `) AS v(source, ledger, tx_hash, op_index, ts, usd_volume, base_amount, quote_amount, prior_usd_volume, prior_generation, usd_basis)`,
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
		basis: "v.usd_basis",
		gen:   "$1",
		args:  args,
	})
}

// MaxTradeLedgerInRange returns the highest on-chain ledger sequence of any trade in [from, to), ok=false when none; it serves the
// live-overlap guard so a restamp only rewrites history the live ingest tail has passed.
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
