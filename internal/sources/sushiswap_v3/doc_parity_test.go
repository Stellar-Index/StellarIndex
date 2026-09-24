package sushiswap_v3

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// feeTierRowRE matches a row of the protocol page's fee-tier table:
// | Fee | Pips | Tick spacing | Pools |.
var feeTierRowRE = regexp.MustCompile(`(?m)^\| [0-9.]+% \| (\d+) \| \d+ \| (\d+) \|$`)

// TestDocs_FeeTierPoolCountsMatchMainnetPools keeps the protocol page's
// per-tier pool counts equal to the in-code MainnetPools seed.
func TestDocs_FeeTierPoolCountsMatchMainnetPools(t *testing.T) {
	body, err := os.ReadFile("../../../docs/protocols/sushiswap_v3.md")
	if err != nil {
		t.Fatalf("read protocol page: %v", err)
	}
	want := map[uint32]int{}
	for _, p := range MainnetPools {
		want[p.FeePips]++
	}
	rows := feeTierRowRE.FindAllStringSubmatch(string(body), -1)
	if len(rows) != len(want) {
		t.Fatalf("page lists %d fee tiers, MainnetPools has %d", len(rows), len(want))
	}
	for _, r := range rows {
		pips, err := strconv.ParseUint(r[1], 10, 32)
		if err != nil {
			t.Fatalf("parse pips %q: %v", r[1], err)
		}
		documented, err := strconv.Atoi(r[2])
		if err != nil {
			t.Fatalf("parse pool count %q: %v", r[2], err)
		}
		if got := want[uint32(pips)]; got != documented {
			t.Errorf("fee tier %d pips: page says %d pools, MainnetPools has %d", pips, documented, got)
		}
	}
}
