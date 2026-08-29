package timescale

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// ─── W5.3: the exact-tier usd_volume RE-STAMP ─────────────────────
//
// [usd_volume_reconcile.go] is the READ side of the exact-tier identity
// (`usd_volume = pegged_leg / 10^decimals`): verify-usd-volume judges a
// day's groups against it and reports violations. This file is the WRITE
// side — the corrective path for rows that fail it.
//
// The population it exists for: trades stamped BEFORE the peg identity was
// the insert path (the pre-2026-07-23 era — measured 2026-07-30 at 66 dirty
// days, every violation `[base_pegged] sdex` USDC-base rows valued by the
// resolver's VWAP instead of the $1 peg; ~+0.7% drift on dust groups). The
// 2026-07-30 fix was a hand SQL UPDATE on r1; this is that UPDATE as a
// tool, with the discipline the hand run lacked baked in:
//
//   - the tier and the scale come from [ClassifyUSDVolumeTier] — the
//     SAME classifier the verifier uses, which is itself lock-stepped to
//     the insert waterfall by TestClassifyUSDVolumeTier_TracksTheWaterfall.
//     The tool never decides "which leg / which scale" on its own (the
//     verifier header's reimplementation-trap warning);
//   - INV-3: every corrected row is stamped with the run's
//     `derive_generation`, and the write is guarded by
//     `derive_generation <= $gen` exactly like the InsertTrade upsert, so
//     a later live gen-0 replay can never claw a correction back and an
//     older re-derive can never overwrite a newer one;
//   - a row that ALREADY satisfies the identity is not touched at all —
//     not its value, not its derive_generation — so re-running a window
//     is idempotent (`usd_volume IS DISTINCT FROM <identity>`);
//   - NULL rows are left alone by default. Filling an unpriced exact-tier
//     row is the same arithmetic, but it is a COVERAGE change the
//     operator opts into (FillNull), not something a value-repair tool
//     does silently.
//
// Estimated tiers (3/4: FX rate / XLM anchor at trade time) are OUT of
// scope by construction — their value is not reproducible from the row
// alone and needs the full resolver-backed waterfall, which is
// `ch-rebuild`'s job (docs/operations/usd-volume-rederive-2026-08.md).

// ExactTierUSDVolume renders the exact-tier identity for one row: the
// pegged leg divided by 10^decimals, rendered EXACTLY as [tradeUSDVolume]
// renders it (`big.Rat.FloatString(8)`), so a restamped value is
// byte-identical to what the insert path would have written.
//
// ok=false when the row is one the waterfall would NOT value: a
// non-exact tier, an unparseable amount, a non-positive quote (the
// waterfall bails before any tier when quote_amount ≤ 0) or a non-positive
// pegged leg. Callers must leave such rows untouched — writing a value the
// insert path would have declined is a different defect, not a repair.
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

// USDVolumeRestampDecision is the per-row differential: given the stored
// usd_volume (nil = SQL NULL) and the row's legs, it returns the value the
// identity says the row should carry and whether writing it would CHANGE
// the row.
//
// Comparison is by NUMERIC value, not by string: Postgres may render the
// same number as "1.5" or "1.50000000", and neither is a violation.
// A NULL stored value is a change only when fillNull is set.
//
// ok=false mirrors [ExactTierUSDVolume]: the row is not restampable and
// must be left alone.
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

// USDVolumeRestampGroup is one (source, base, quote) group the restamp
// applies the identity to. Built by the caller from
// [ClassifyUSDVolumeTier]; Tier must be an exact tier.
type USDVolumeRestampGroup struct {
	Source     string
	BaseAsset  string
	QuoteAsset string
	Tier       USDVolumeTier
	Decimals   int
}

// USDVolumeRestampParams scopes one restamp statement.
type USDVolumeRestampParams struct {
	// Groups are the exact-tier groups to judge. Rows outside these
	// (source, base, quote) triples are never read.
	Groups []USDVolumeRestampGroup
	// [From, To) bounds the rows by ts. Callers slice a day into bounded
	// windows so a single UPDATE never has to decompress a whole chunk
	// in one transaction.
	From, To time.Time
	// FillNull also restamps rows whose usd_volume is NULL. Off by
	// default — see the file header.
	FillNull bool
	// Generation is the run's derive_generation (INV-3). Rows already at
	// a HIGHER generation are never touched, and every row written is
	// stamped with it.
	Generation int64
}

// exactTierRestampScope renders the group VALUES relation and the WHERE
// clause shared by the dry-run count and the apply UPDATE, so the two can
// never disagree about which rows are candidates (the SELECT joins the
// relation in its FROM list; the UPDATE takes it as its FROM clause).
// `$1..$3` are from/to/generation; each group contributes its own
// placeholders. The pegged-leg expression and the 10^decimals
// denominator are decided in Go ([USDVolumeRestampGroup]) and only
// EVALUATED in SQL — the arithmetic is `round(leg / denom, 8)`, the SQL
// spelling of `big.Rat.FloatString(8)` (both round half away from zero;
// for decimals ≤ 8 the quotient is exact and no rounding occurs at all).
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
   AND ` + legExpr + ` > 0
   AND t.usd_volume IS DISTINCT FROM ` + identity + nullClause
	return groupRel, where, identity, args, nil
}

// CountUSDVolumeRestampCandidates is the DRY RUN: how many rows in the
// window the apply path WOULD rewrite. Same scope predicate as
// [Store.RestampExactTierUSDVolume], so the count is the write's exact
// preview.
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

// RestampExactTierUSDVolume applies the identity to every candidate row in
// the window and returns the number of rows rewritten. Rows that already
// satisfy the identity are not touched (value or derive_generation).
//
// Runs on a DEDICATED connection with
// `timescaledb.max_tuples_decompressed_per_dml_transaction = 0`: the
// historical span lives in COMPRESSED chunks, and the default 100k-tuple
// cap aborts a single day's DML (measured 2026-07-30: one day needed
// 265k). Session-scoped on purpose — the GUC must not leak into the
// pool's serving connections.
func (s *Store) RestampExactTierUSDVolume(ctx context.Context, p USDVolumeRestampParams) (int64, error) {
	groupRel, where, identity, args, err := exactTierRestampScope(p)
	if err != nil {
		return 0, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("timescale: usd-volume restamp conn: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SET timescaledb.max_tuples_decompressed_per_dml_transaction = 0"); err != nil {
		return 0, fmt.Errorf("timescale: usd-volume restamp: raise decompression cap: %w", err)
	}
	// Every fragment is code-built by exactTierRestampScope; all values
	// (window, generation, group triples, leg, denominator) travel as
	// positional placeholders — gosec G202 is a false positive here.
	//nolint:gosec // no caller-supplied text reaches the statement
	q := "UPDATE trades t SET usd_volume = " + identity + ", derive_generation = $3 FROM " + groupRel + where
	res, err := conn.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("timescale: usd-volume restamp [%s, %s): %w",
			p.From.Format(time.RFC3339), p.To.Format(time.RFC3339), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: usd-volume restamp rows affected: %w", err)
	}
	return n, nil
}
