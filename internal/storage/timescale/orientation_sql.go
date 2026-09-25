package timescale

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// nativeXLMSAC is the Stellar Asset Contract address wrapping native
// XLM — the same literal the XLM/USD CTEs already hardcode. Mirrors
// canonical's unexported nativeSAC.
const nativeXLMSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

// stablecoinInListSQL renders canonical.StablecoinCodes as a sorted,
// single-quoted SQL IN-list (e.g. "'DAI', 'EURC', …"). Sorted so the
// generated query string is stable (plan cache + golden tests). The
// codes are plain ASCII uppercase identifiers from a closed in-repo
// map — no injection surface — but we still only ever emit them as
// quoted literals.
func stablecoinInListSQL() string {
	codes := make([]string, 0, len(canonical.StablecoinCodes))
	for c := range canonical.StablecoinCodes {
		codes = append(codes, "'"+c+"'")
	}
	sort.Strings(codes)
	return strings.Join(codes, ", ")
}

// quoteRankSQL is a SQL CASE mirroring canonical.quoteRank for the
// given asset column: fiat (4) > stablecoin (3) > XLM (2) > token (1).
// Higher = more quote-like.
func quoteRankSQL(col string) string {
	return fmt.Sprintf(`(CASE
        WHEN %[1]s LIKE 'fiat:%%' THEN 4
        WHEN split_part(replace(%[1]s, 'crypto:', ''), '-', 1) IN (%[2]s) THEN 3
        WHEN %[1]s IN ('native', 'crypto:XLM', '%[3]s') THEN 2
        ELSE 1 END)`, col, stablecoinInListSQL(), nativeXLMSAC)
}

// marketKeySQL is a market's identity for COUNTING: its unordered
// {base, quote} pair. canonical.Orient is a function of that set alone,
// so DISTINCT on this key counts exactly what DISTINCT on
// canonOrientSQL's (base, quote) does, without the rank CASE.
const marketKeySQL = `LEAST(base_asset, quote_asset), GREATEST(base_asset, quote_asset)`

// canonLastPriceSQL is a canonical market's newest last_price across its
// stored orientations, the flipped row's price inverted. Both
// orientations routinely share a last_trade_at, so the tie goes to the
// canonically stored row rather than to scan order. flipped is
// canonOrientSQL's third expression.
func canonLastPriceSQL(flipped string) string {
	return `(array_agg(
                    CASE WHEN ` + flipped + ` AND last_price IS NOT NULL
                         THEN (1.0 / NULLIF(last_price::numeric, 0))::text
                         ELSE last_price END
                    ORDER BY last_trade_at DESC NULLS LAST, ` + flipped + `)
                  FILTER (WHERE last_price IS NOT NULL))[1]`
}

// canonOrientSQL returns SQL expressions for the canonical (base,
// quote) orientation of a market stored as (base_asset, quote_asset), plus a
// boolean `flipped` — true when the stored row is reversed relative to
// canonical (so the caller inverts that row's price before combining).
// Mirrors canonical.Orient: the canonical quote is the higher-quoteRank
// asset, ties broken by the greater asset_id string. Each returned
// expression is fully parenthesised and safe to inline.
//
// Stored orientation is per source family and the families are INVERSE:
// SDEX stores base = the resting offer's AssetSold (what the taker
// received), Soroban AMMs (aquarius, comet, phoenix, soroswap) store
// base = token_in (what the taker sold). The same economic trade lands in
// opposite orientations, so a read combining sources must orient here.
func canonOrientSQL() (canonBase, canonQuote, flipped string) {
	const bcol, qcol = "base_asset", "quote_asset"
	rb, rq := quoteRankSQL(bcol), quoteRankSQL(qcol)
	// The stored base (bcol) is actually the canonical QUOTE when it
	// outranks the stored quote, or on a tie sorts after it.
	flipped = fmt.Sprintf("(%[1]s > %[2]s OR (%[1]s = %[2]s AND %[3]s > %[4]s))", rb, rq, bcol, qcol)
	canonBase = fmt.Sprintf("(CASE WHEN %s THEN %s ELSE %s END)", flipped, qcol, bcol)
	canonQuote = fmt.Sprintf("(CASE WHEN %s THEN %s ELSE %s END)", flipped, bcol, qcol)
	return canonBase, canonQuote, flipped
}
