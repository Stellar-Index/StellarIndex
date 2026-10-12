package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// The exact-tier usd_volume RE-STAMP: the WRITE side of the identity
// `usd_volume = pegged_leg / 10^decimals` that [usd_volume_reconcile.go] verifies.
//
//   - tier and scale come from [ClassifyUSDVolumeTier], the SAME classifier the verifier uses
//     (lock-stepped by TestClassifyUSDVolumeTier_TracksTheWaterfall);
//   - a corrected row gets the run's `derive_generation`, guarded by `derive_generation <= $gen`
//     like the InsertTrade upsert, so a gen-0 replay or older re-derive cannot undo it;
//   - a row already satisfying the identity is untouched, so re-running a window is idempotent;
//   - each rewrite first logs the prior value in `usd_volume_restamp_log` in the same transaction;
//   - NULL rows are left alone unless FillNull is set.
//
// Estimated tiers are out of scope: their value is not reproducible from the row alone.

// ExactTierUSDVolume renders the pegged leg divided by 10^decimals exactly as tradeUSDVolume does (big.Rat.FloatString(8)) so a
// restamped value is byte-identical to the insert path's; ok=false when the waterfall would not value the row.
func ExactTierUSDVolume(tier USDVolumeTier, decimals int, baseAmount, quoteAmount string) (string, bool) {
	if !tier.Exact() {
		return "", false
	}
	quote, ok := new(big.Int).SetString(quoteAmount, 10)
	if !ok || quote.Sign() <= 0 {
		return "", false
	}
	leg := quote
	if tier == TierBasePegged {
		base, bok := new(big.Int).SetString(baseAmount, 10)
		if !bok || base.Sign() <= 0 {
			return "", false
		}
		leg = base
	}
	return new(big.Rat).SetFrac(leg, scaleDenominator(decimals)).FloatString(usdVolumeRenderDecimals), true
}

// USDVolumeRestampDecision returns the value the identity says a row should carry and whether writing it would change the row (stored
// nil is SQL NULL, a change only when fillNull is set); compared by NUMERIC value, not string.
func USDVolumeRestampDecision(stored *string, tier USDVolumeTier, decimals int, baseAmount, quoteAmount string, fillNull bool) (want string, changed, ok bool) {
	want, ok = ExactTierUSDVolume(tier, decimals, baseAmount, quoteAmount)
	if !ok {
		return "", false, false
	}
	if stored == nil {
		return want, fillNull, true
	}
	storedRat, sok := new(big.Rat).SetString(*stored)
	if !sok {
		// A corrupt NUMERIC render is reportable, not silently rewritable.
		return want, false, false
	}
	wantRat, _ := new(big.Rat).SetString(want)
	return want, storedRat.Cmp(wantRat) != 0, true
}

// USDVolumeRestampGroup is one (source, base, quote) group built from [ClassifyUSDVolumeTier]; Tier must be exact.
type USDVolumeRestampGroup struct {
	Source     string
	BaseAsset  string
	QuoteAsset string
	Tier       USDVolumeTier
	Decimals   int
}

// USDVolumeRestampParams scopes one restamp statement.
type USDVolumeRestampParams struct {
	// Groups are the exact-tier groups to judge; rows outside them are never read.
	Groups []USDVolumeRestampGroup
	// [From, To) bounds rows by ts. Callers slice a day into small windows so one UPDATE never
	// decompresses a whole chunk.
	From, To time.Time
	// FillNull also restamps NULL usd_volume rows (a coverage change, so off by default).
	FillNull bool
	// Generation is the run's derive_generation: rows at a HIGHER generation are never touched
	// and every row written is stamped with it.
	Generation int64
}

// exactTierRestampScope renders the group VALUES relation and WHERE shared by the dry-run count and apply UPDATE so they cannot
// disagree. $1..$3 are from/to/generation. The pegged leg and 10^decimals are decided in Go; `round(leg / denom, 8)` matches
// `big.Rat.FloatString(8)` (exact for decimals <= 8).
func exactTierRestampScope(p USDVolumeRestampParams) (groupRel, where, identity string, args []any, err error) {
	if len(p.Groups) == 0 {
		return "", "", "", nil, fmt.Errorf("timescale: usd-volume restamp: no groups")
	}
	if !p.To.After(p.From) {
		return "", "", "", nil, fmt.Errorf("timescale: usd-volume restamp: empty window [%s, %s)", p.From, p.To)
	}
	args = []any{p.From, p.To, p.Generation}
	var values strings.Builder
	for i, g := range p.Groups {
		if !g.Tier.Exact() {
			return "", "", "", nil, fmt.Errorf("timescale: usd-volume restamp: group %s %s/%s has non-exact tier %q",
				g.Source, g.BaseAsset, g.QuoteAsset, g.Tier)
		}
		leg := "quote"
		if g.Tier == TierBasePegged {
			leg = "base"
		}
		if i > 0 {
			values.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&values, "($%d::text, $%d::text, $%d::text, $%d::text, $%d::numeric)", n+1, n+2, n+3, n+4, n+5)
		args = append(args, g.Source, g.BaseAsset, g.QuoteAsset, leg, scaleDenominator(g.Decimals).String())
	}
	const legExpr = "(CASE WHEN g.leg = 'base' THEN t.base_amount ELSE t.quote_amount END)"
	identity = "round(" + legExpr + " / g.denom, " + fmt.Sprint(usdVolumeRenderDecimals) + ")"
	nullClause := ""
	if !p.FillNull {
		nullClause = "\n   AND t.usd_volume IS NOT NULL"
	}
	groupRel = "(VALUES " + values.String() + ") AS g(source, base_asset, quote_asset, leg, denom)"
	where = `
 WHERE t.ts >= $1 AND t.ts < $2
   AND t.source = g.source AND t.base_asset = g.base_asset AND t.quote_asset = g.quote_asset
   AND t.derive_generation <= $3
   AND t.quote_amount > 0
   AND ` + legExpr + ` >= 0
   AND t.usd_volume IS DISTINCT FROM ` + identity + nullClause
	return groupRel, where, identity, args, nil
}

// CountUSDVolumeRestampCandidates is the dry run: it counts the rows [Store.RestampExactTierUSDVolume] would rewrite, using the same
// scope predicate.
func (s *Store) CountUSDVolumeRestampCandidates(ctx context.Context, p USDVolumeRestampParams) (int64, error) {
	groupRel, where, _, args, err := exactTierRestampScope(p)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM trades t, "+groupRel+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: usd-volume restamp count [%s, %s): %w",
			p.From.Format(time.RFC3339), p.To.Format(time.RFC3339), err)
	}
	return n, nil
}

// RestampExactTierUSDVolume applies the identity to every candidate row in the caller-sliced window and returns the rows rewritten;
// rows already satisfying it are untouched.
func (s *Store) RestampExactTierUSDVolume(ctx context.Context, p USDVolumeRestampParams) (int64, error) {
	groupRel, where, identity, args, err := exactTierRestampScope(p)
	if err != nil {
		return 0, err
	}
	return s.restampTradesUSDVolume(ctx, usdVolumeRestampWrite{
		label: "usd-volume restamp",
		scope: fmt.Sprintf("[%s, %s)", p.From.Format(time.RFC3339), p.To.Format(time.RFC3339)),
		rel:   groupRel, where: where, value: identity, gen: "$3", args: args,
	})
}

// usdVolumeRestampWrite is one in-place rewrite of `trades.usd_volume`: `rel` joins `trades t`, `where` selects, `value` is the new
// expression, `gen` the derive_generation placeholder. Fragments are code-built; values travel in args.
type usdVolumeRestampWrite struct {
	label, scope           string
	rel, where, value, gen string
	args                   []any
}

// statements renders the before-image INSERT and the UPDATE from the SAME rel/where, so the log
// can only name rows the UPDATE rewrites.
func (w usdVolumeRestampWrite) statements() (logStmt, updateStmt string) {
	logStmt = `INSERT INTO usd_volume_restamp_log
	       (source, ledger, tx_hash, op_index, ts, prior_usd_volume, prior_derive_generation, usd_volume, derive_generation)
	SELECT t.source, t.ledger, t.tx_hash, t.op_index, t.ts, t.usd_volume, t.derive_generation, ` + w.value + `, ` + w.gen + `
	  FROM trades t, ` + w.rel + w.where
	updateStmt = "UPDATE trades t SET usd_volume = " + w.value + ", derive_generation = " + w.gen + " FROM " + w.rel + w.where
	return logStmt, updateStmt
}

// restampTradesUSDVolume is the one path rewriting `trades.usd_volume` in place: one REPEATABLE READ tx logs before-images to
// `usd_volume_restamp_log`, rewrites, and refuses to commit unless counts agree. Decompression cap lifted for COMPRESSED history,
// CUSTOM plan forced so `ts` bounds prune chunks; `SET LOCAL`, not `SET`, so nothing rides the pooled connection.
func (s *Store) restampTradesUSDVolume(ctx context.Context, w usdVolumeRestampWrite) (int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return 0, fmt.Errorf("timescale: %s begin: %w", w.label, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0"); err != nil {
		return 0, fmt.Errorf("timescale: %s: raise decompression cap: %w", w.label, err)
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL plan_cache_mode = force_custom_plan"); err != nil {
		return 0, fmt.Errorf("timescale: %s: force a custom plan: %w", w.label, err)
	}
	logStmt, updateStmt := w.statements()
	logged, err := execRowsAffected(ctx, tx, logStmt, w.args)
	if err != nil {
		return 0, fmt.Errorf("timescale: %s before-image %s: %w", w.label, w.scope, err)
	}
	n, err := execRowsAffected(ctx, tx, updateStmt, w.args)
	if err != nil {
		return 0, fmt.Errorf("timescale: %s %s: %w", w.label, w.scope, err)
	}
	if n != logged {
		return 0, fmt.Errorf("timescale: %s %s: before-image holds %d row(s) but the update rewrote %d; rolled back",
			w.label, w.scope, logged, n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("timescale: %s commit %s: %w", w.label, w.scope, err)
	}
	return n, nil
}

// execRowsAffected runs one DML statement in tx and returns its row count.
func execRowsAffected(ctx context.Context, tx *sql.Tx, q string, args []any) (int64, error) {
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
