package timescale

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
)

// TestCCTPEventType_MatchesSourceEventNames cross-checks IsValid against the Event* constants in
// internal/sources/cctp: a rename fails to compile, and a set diff catches an event added or removed on one
// side.
func TestCCTPEventType_MatchesSourceEventNames(t *testing.T) {
	t.Parallel()
	fromSource := []string{
		cctp.EventDepositForBurn,
		cctp.EventMintAndWithdraw,
		cctp.EventMessageSent,
		cctp.EventMessageReceived,
		cctp.EventMintAndForward,
		cctp.EventOwnershipTransfer,
		cctp.EventOwnershipTransferCompleted,
		cctp.EventAdminChanged,
		cctp.EventRemoteTokenMessengerAdded,
		cctp.EventTokenPairLinked,
		cctp.EventAdminChangeStarted,
		cctp.EventAttesterEnabled,
		cctp.EventAttesterManagerUpdated,
		cctp.EventDenylisted,
		cctp.EventDenylisterChanged,
		cctp.EventFeeRecipientSet,
		cctp.EventMaxMessageBodySizeUpdated,
		cctp.EventMinFeeControllerSet,
		cctp.EventPauserChanged,
		cctp.EventRescuerChanged,
		cctp.EventSetBurnLimitPerMessage,
		cctp.EventSetTokenController,
		cctp.EventSignatureThresholdUpdated,
		cctp.EventSwapMinterConfigSet,
		cctp.EventTokenDecimalConfigAdded,
		cctp.EventUnDenylisted,
	}
	storageKnown := map[CCTPEventType]bool{
		CCTPDepositForBurn:             true,
		CCTPMintAndWithdraw:            true,
		CCTPMessageSent:                true,
		CCTPMessageReceived:            true,
		CCTPMintAndForward:             true,
		CCTPOwnershipTransfer:          true,
		CCTPOwnershipTransferCompleted: true,
		CCTPAdminChanged:               true,
		CCTPRemoteTokenMessengerAdded:  true,
		CCTPTokenPairLinked:            true,
		CCTPAdminChangeStarted:         true,
		CCTPAttesterEnabled:            true,
		CCTPAttesterManagerUpdated:     true,
		CCTPDenylisted:                 true,
		CCTPDenylisterChanged:          true,
		CCTPFeeRecipientSet:            true,
		CCTPMaxMessageBodySizeUpdated:  true,
		CCTPMinFeeControllerSet:        true,
		CCTPPauserChanged:              true,
		CCTPRescuerChanged:             true,
		CCTPSetBurnLimitPerMessage:     true,
		CCTPSetTokenController:         true,
		CCTPSignatureThresholdUpdated:  true,
		CCTPSwapMinterConfigSet:        true,
		CCTPTokenDecimalConfigAdded:    true,
		CCTPUnDenylisted:               true,
	}

	if len(fromSource) != len(storageKnown) {
		t.Fatalf("layer mismatch: internal/sources/cctp has %d Event* constants, "+
			"timescale.CCTPEventType has %d — a new/removed event was added to one "+
			"layer without the other", len(fromSource), len(storageKnown))
	}

	seen := make(map[CCTPEventType]bool, len(fromSource))
	for _, name := range fromSource {
		kind := CCTPEventType(name)
		if !storageKnown[kind] {
			t.Errorf("cctp.Event %q has no matching timescale.CCTPEventType constant", name)
			continue
		}
		if !kind.IsValid() {
			t.Errorf("timescale.CCTPEventType(%q).IsValid() = false, want true", name)
		}
		seen[kind] = true
	}
	for kind := range storageKnown {
		if !seen[kind] {
			t.Errorf("timescale.CCTPEventType %q has no matching internal/sources/cctp Event* constant", kind)
		}
	}
}

func TestCCTPEventType_IsValid_RejectsUnknown(t *testing.T) {
	t.Parallel()
	for _, bad := range []CCTPEventType{"", "bogus_event", "admin_change_started_v2"} {
		if bad.IsValid() {
			t.Errorf("IsValid(%q) = true, want false", bad)
		}
	}
}

// cctpBothMintTypesRe finds a cctp_events scan keyed on BOTH mint event types (either literal
// order), which file-doc rule 2 allows ONLY for a non-value aggregate such as distinct
// recipients, never for sum(amount): the forward-restatement row would double the value.
var cctpBothMintTypesRe = regexp.MustCompile(
	`event_type\s+IN\s*\(\s*'mint_and_(?:withdraw','mint_and_forward|forward','mint_and_withdraw)'\s*\)`)

// TestCCTPInboundSumsExcludeForwardRestatement pins file-doc rule 2: mint_and_forward restates
// mint_and_withdraw at the local 7-decimal SAC scale (10x), so summing both counts one transfer 11x.
// cctp_events has no scale column to catch this.
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
