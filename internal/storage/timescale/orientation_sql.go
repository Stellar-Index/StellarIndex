package timescale

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// nativeSACParam is the placeholder for the installed network's native
// XLM SAC, bound at $n from canonical.NativeSACContractID().
func nativeSACParam(n int) string { return fmt.Sprintf("$%d::text", n) }

// xlmQuotesBound is XLM in both on-chain identity forms, 'native' and its
// SAC bound at $n. A VWAP against these is a price in XLM.
func xlmQuotesBound(n int) string { return "'native', " + nativeSACParam(n) }

// Constant forms of [xlmQuotesBound] for the SQL that must stay compile-time
// constant; TestXLMQuotesBoundConsts pins them to the helper.
const (
	xlmQuotesBound1 = "'native', $1::text"
	xlmQuotesBound2 = "'native', $2::text"
	xlmQuotesBound3 = "'native', $3::text"
	xlmQuotesBound4 = "'native', $4::text"
)

// stablecoinInListSQL renders canonical.StablecoinCodes as a sorted quoted IN-list, sorted so the query text is plan-cache stable.
func stablecoinInListSQL() string {
	codes := make([]string, 0, len(canonical.StablecoinCodes))
	for c := range canonical.StablecoinCodes {
		codes = append(codes, "'"+c+"'")
	}
	sort.Strings(codes)
	return strings.Join(codes, ", ")
}

// quoteRankSQL mirrors canonical.quoteRank for col: fiat 4 > stablecoin 3 > XLM 2 > token 1; per query because stablecoin SACs come from the
// alias registry.
func quoteRankSQL(col string, sacIdx int) string {
	return fmt.Sprintf(`(CASE
        WHEN %[1]s LIKE 'fiat:%%' THEN 4
        WHEN split_part(replace(%[1]s, 'crypto:', ''), '-', 1) IN (%[2]s) THEN 3%[4]s
        WHEN %[1]s IN ('native', 'crypto:XLM', %[3]s) THEN 2
        ELSE 1 END)`, col, stablecoinInListSQL(), nativeSACParam(sacIdx), stablecoinSACArmSQL(col))
}

// stablecoinSACArmSQL is quoteRankSQL's rank-3 arm for declared stablecoin SACs, or "" when none (an empty IN list is a syntax error).
func stablecoinSACArmSQL(col string) string {
	forms := canonical.StablecoinSACForms()
	if len(forms) == 0 {
		return ""
	}
	return fmt.Sprintf("\n        WHEN %s IN ('%s') THEN 3", col, strings.Join(forms, "', '"))
}

// marketKeySQL is a market's identity for COUNTING: its unordered {base, quote} pair. Orient is a
// function of that set alone, so DISTINCT on it equals DISTINCT on canonOrientSQL without the CASE.
const marketKeySQL = `LEAST(base_asset, quote_asset), GREATEST(base_asset, quote_asset)`

// canonLastPriceSQL is the newest last_price across a market's orientations, the flipped row inverted; ties go to the canonically stored
// row.
func canonLastPriceSQL(flipped string) string {
	return `(array_agg(
                    CASE WHEN ` + flipped + ` AND last_price IS NOT NULL
                         THEN (1.0 / NULLIF(last_price::numeric, 0))::text
                         ELSE last_price END
                    ORDER BY last_trade_at DESC NULLS LAST, ` + flipped + `)
                  FILTER (WHERE last_price IS NOT NULL))[1]`
}

// canonOrientSQL returns SQL for the canonical (base, quote) orientation of a stored market plus `flipped`, mirroring canonical.Orient.
// Stored orientation is inverse per source family (SDEX base = AssetSold, Soroban AMMs base = token_in), so cross-source reads must orient.
func canonOrientSQL(sacIdx int) (canonBase, canonQuote, flipped string) {
	const bcol, qcol = "base_asset", "quote_asset"
	rb, rq := quoteRankSQL(bcol, sacIdx), quoteRankSQL(qcol, sacIdx)
	// Stored base is the canonical QUOTE when it outranks the stored quote or ties and sorts after.
	// COLLATE "C" matches Go's byte-order compare; the default collation orders mixed case differently.
	flipped = fmt.Sprintf(`(%[1]s > %[2]s OR (%[1]s = %[2]s AND %[3]s COLLATE "C" > %[4]s COLLATE "C"))`, rb, rq, bcol, qcol)
	canonBase = fmt.Sprintf("(CASE WHEN %s THEN %s ELSE %s END)", flipped, qcol, bcol)
	canonQuote = fmt.Sprintf("(CASE WHEN %s THEN %s ELSE %s END)", flipped, bcol, qcol)
	return canonBase, canonQuote, flipped
}
