package timescale

import (
	"strings"
	"testing"
)

// The census query must label every leg with the logical source name
// the protocol registry uses — guard the multi-table sums' labels so
// a table rename or copy-paste slip can't silently re-bucket a leg.
func TestCountRecentEventsQuery_labelsEveryLeg(t *testing.T) {
	for _, want := range []string{
		"'blend'", "'phoenix'", "'comet'", "'soroswap'",
		"'defindex'", "'cctp'", "'rozo'", "'soroswap-router'",
	} {
		if !strings.Contains(countRecentEventsQuery, want) {
			t.Errorf("census query missing %s leg label", want)
		}
	}
	for _, table := range []string{
		"trades", "blend_positions", "blend_emissions", "blend_admin",
		"blend_auctions", "phoenix_liquidity", "phoenix_stake_events",
		"comet_liquidity", "soroswap_skim_events", "defindex_flows",
		"cctp_events", "rozo_events", "soroswap_router_swaps",
		"oracle_updates",
	} {
		if !strings.Contains(countRecentEventsQuery, "FROM "+table) {
			t.Errorf("census query missing FROM %s leg", table)
		}
	}
}
