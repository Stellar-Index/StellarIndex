package aquarius

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// The router's census lists its own set_protocol_fee, and the protocol doc
// promises router/pool rows for the fee and kill-switch tables. A gate on
// reg.Has alone never admits the router, which is a factory, not a pool.
func TestMatches_routerEmittedFeeAndKillPassGate(t *testing.T) {
	d := NewDecoder()
	foreign := makeContractStrkey(t, 0xAE)
	for _, topic := range []string{
		TopicSymbolSetProtocolFee, TopicSymbolClaimProtocolFee,
		TopicSymbolKillDeposit, TopicSymbolUnkillDeposit,
		TopicSymbolKillSwap, TopicSymbolUnkillSwap,
		TopicSymbolKillClaim, TopicSymbolUnkillClaim,
		TopicSymbolKillGaugesClaim, TopicSymbolUnkillGaugesClaim,
	} {
		if !d.Matches(events.Event{ContractID: MainnetRouter, Topic: []string{topic}}) {
			t.Errorf("%s from the canonical router: Matches=false, want true", topic)
		}
		if d.Matches(events.Event{ContractID: foreign, Topic: []string{topic}}) {
			t.Errorf("%s from an unregistered contract: Matches=true, want false", topic)
		}
	}

	out, err := d.Decode(events.Event{
		ContractID:     MainnetRouter,
		Ledger:         61_000_000,
		TxHash:         "router-kill",
		EventIndex:     1,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
		Topic:          []string{TopicSymbolKillSwap},
		Value:          "AAAAAQ==", // SCV_VOID
	})
	if err != nil {
		t.Fatalf("Decode(router kill_swap): %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode(router kill_swap) emitted %d events, want 1", len(out))
	}
	if ke, ok := out[0].(KillEvent); !ok || ke.ContractID != MainnetRouter || ke.Action != EventKillSwap {
		t.Errorf("Decode(router kill_swap) = %+v, want a KillEvent from the router with action %q", out[0], EventKillSwap)
	}
}

var backticked = regexp.MustCompile("`([a-z_]+)`")

// Every topic the protocol doc lists for the canonical router must either
// pass the gate or be named in the README's "Known gap" section, so a
// router topic cannot sit unmatched behind an "every topic" claim.
func TestRouterCensusTopics_matchedOrKnownGap(t *testing.T) {
	proto, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "protocols", "aquarius.md"))
	if err != nil {
		t.Fatalf("read protocol doc: %v", err)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	var row string
	for _, line := range strings.Split(string(proto), "\n") {
		if strings.HasPrefix(line, "| Liquidity-pool router |") {
			row = line
			break
		}
	}
	cols := strings.Split(row, "|")
	if len(cols) < 4 {
		t.Fatalf("router census row not found or malformed: %q", row)
	}
	topics := backticked.FindAllStringSubmatch(cols[3], -1)
	if len(topics) == 0 {
		t.Fatalf("router census row lists no topics: %q", cols[3])
	}

	var gap string
	if i := strings.Index(string(readme), "## ⚠️ Known gap — router"); i >= 0 {
		gap = string(readme[i:])
		if j := strings.Index(gap[1:], "\n## "); j >= 0 {
			gap = gap[:j+1]
		}
	}

	d := NewDecoder()
	for _, m := range topics {
		name := m[1]
		ev := events.Event{ContractID: MainnetRouter, Topic: []string{scval.MustEncodeSymbol(name)}}
		if d.Matches(ev) {
			continue
		}
		if !strings.Contains(gap, "`"+name+"`") {
			t.Errorf("router topic %q neither passes the gate nor is named in the README's router Known gap section", name)
		}
	}
}
