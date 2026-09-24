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
