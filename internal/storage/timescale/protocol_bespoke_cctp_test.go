package timescale

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// cctpBothMintTypesRe finds a scan of cctp_events keyed on BOTH mint
// event types together (either literal order), the shape file-doc rule 2
// says is safe ONLY for a non-value aggregate (e.g. distinct recipients),
// never for a sum(amount) — a forward-restatement row would double one
// transfer's value into the total.
var cctpBothMintTypesRe = regexp.MustCompile(
	`event_type\s+IN\s*\(\s*'mint_and_(?:withdraw','mint_and_forward|forward','mint_and_withdraw)'\s*\)`)

// TestCCTPInboundSumsExcludeForwardRestatement pins file-doc rule 2: a
// mint_and_forward event restates its sibling mint_and_withdraw at the
// LOCAL 7-decimal SAC scale (exactly 10x), so any query that sums amount
// across both event types in the same aggregate would count one transfer
// 11x over. cctp_events carries no scale/discriminator column to catch
// this at the schema level, so the invariant is enforced here.
func TestCCTPInboundSumsExcludeForwardRestatement(t *testing.T) {
	src, err := os.ReadFile("protocol_bespoke_cctp.go")
	if err != nil {
		t.Fatalf("read protocol_bespoke_cctp.go: %v", err)
	}
	text := string(src)

	locs := cctpBothMintTypesRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		t.Fatal("no `event_type IN (mint_and_withdraw, mint_and_forward)` scan " +
			"found in protocol_bespoke_cctp.go — this guard would pass " +
			"vacuously; update it to match the file's current query shape")
	}
	for _, loc := range locs {
		start := loc[0] - 300
		if start < 0 {
			start = 0
		}
		window := text[start:loc[0]]
		selIdx := strings.LastIndex(window, "SELECT")
		clause := window
		if selIdx >= 0 {
			clause = window[selIdx:]
		}
		if strings.Contains(clause, "sum(amount)") {
			t.Errorf("a SELECT scanning both mint_and_withdraw and mint_and_forward "+
				"also sums amount — file-doc rule 2: mint_and_forward restates "+
				"mint_and_withdraw at 10x, so summing both double-counts (11x) a "+
				"single transfer:\n%s", clause)
		}
	}

	// Pin the two value CTEs to their single-event-type filters directly,
	// since they are the only sanctioned source of a sum(amount) in this
	// file (cctpMintsCTE / cctpBurnsCTE).
	if !strings.Contains(text, "event_type = 'mint_and_withdraw' AND amount IS NOT NULL") {
		t.Error("cctpMintsCTE must filter event_type = 'mint_and_withdraw' only " +
			"— widening it to include mint_and_forward reintroduces the 11x over-count")
	}
	if !strings.Contains(text, "event_type = 'deposit_for_burn' AND amount IS NOT NULL") {
		t.Error("cctpBurnsCTE must filter event_type = 'deposit_for_burn' only")
	}
}

// cctpOrderByTerms returns the comma-separated terms of the LAST ORDER BY
// in q, with any trailing LIMIT stripped and whitespace collapsed.
func cctpOrderByTerms(q string) []string {
	q = strings.Join(strings.Fields(q), " ")
	i := strings.LastIndex(q, "ORDER BY ")
	if i < 0 {
		return nil
	}
	clause := q[i+len("ORDER BY "):]
	if j := strings.Index(clause, " LIMIT "); j >= 0 {
		clause = clause[:j]
	}
	terms := strings.Split(clause, ",")
	for k := range terms {
		terms[k] = strings.TrimSpace(terms[k])
	}
	return terms
}

// collectPerChainSeries starts a new series whenever chain_key changes, so
// chains with equal window volume must not interleave by bucket.
func TestCCTPPerChainSeriesOrderIsDeterministic(t *testing.T) {
	topRe := regexp.MustCompile(`top AS \((SELECT chain_key, sum\(amount\) AS vol FROM j[^)]*)\)`)
	for _, inbound := range []bool{true, false} {
		for _, windowDays := range []int{1, 7, 30, 90} {
			q := cctpPerChainSeriesQuery(windowDays, inbound)

			m := topRe.FindStringSubmatch(q)
			if m == nil {
				t.Fatalf("inbound=%v window=%d: top CTE not found in query:\n%s", inbound, windowDays, q)
			}
			if got, want := cctpOrderByTerms(m[1]), []string{"2 DESC", "chain_key ASC"}; strings.Join(got, ", ") != strings.Join(want, ", ") {
				t.Errorf("inbound=%v window=%d: top CTE ORDER BY = %q, want %q (volume ties must break on chain_key before LIMIT 5)",
					inbound, windowDays, got, want)
			}

			if got, want := cctpOrderByTerms(q), []string{"top.vol DESC", "j.chain_key ASC", "2 ASC"}; strings.Join(got, ", ") != strings.Join(want, ", ") {
				t.Errorf("inbound=%v window=%d: outer ORDER BY = %q, want %q (chain_key must precede the bucket so each chain's rows are contiguous)",
					inbound, windowDays, got, want)
			}
		}
	}
}
