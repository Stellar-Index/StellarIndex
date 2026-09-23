package timescale

import (
	"encoding/json"
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
        WHEN %[1]s IN ('native', '%[3]s') THEN 2
        ELSE 1 END)`, col, stablecoinInListSQL(), nativeXLMSAC)
}

// canonOrientSQL returns SQL expressions for the canonical (base,
// quote) orientation of a market stored as (base_asset, quote_asset), plus a
// boolean `flipped` — true when the stored row is reversed relative to
// canonical (so the caller inverts that row's price before combining).
// Mirrors canonical.Orient: the canonical quote is the higher-quoteRank
// asset, ties broken by the greater asset_id string. Each returned
// expression is fully parenthesised and safe to inline.
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

// aliasFoldSQL folds the asset column col onto its canonical alias form
// (the XLM SAC and crypto:XLM onto native, a configured SAC onto its
// classic asset) through the JSON object bound at placeholder $idx — see
// [aliasFoldArg]. A market is one market whichever venue spelling printed
// it, so a group key must be folded BEFORE [canonOrientSQL] orients it:
// orienting alone leaves an SDEX row and its Soroban twin as two markets.
func aliasFoldSQL(col string, idx int) string {
	return fmt.Sprintf("COALESCE($%d::text::jsonb ->> %s, %s)", idx, col, col)
}

// aliasFoldArg is the value bound to [aliasFoldSQL]'s placeholder:
// [canonical.AllAliasForms] as a JSON object, resolved per call so it
// reflects the registry installed at binary start-up.
func aliasFoldArg() string {
	// A map[string]string always marshals; there is no error to handle.
	b, _ := json.Marshal(canonical.AllAliasForms())
	return string(b)
}

// aliasFoldedSelect renders `SELECT <folded base_asset>, <folded
// quote_asset>, cols... FROM rel`: the relation a [canonOrientSQL] group
// reads so its key is alias-folded (see [aliasFoldSQL]).
func aliasFoldedSelect(rel string, idx int, cols ...string) string {
	sel := aliasFoldSQL("base_asset", idx) + " AS base_asset, " +
		aliasFoldSQL("quote_asset", idx) + " AS quote_asset"
	for _, c := range cols {
		sel += ", " + c
	}
	return "SELECT " + sel + " FROM " + rel
}
