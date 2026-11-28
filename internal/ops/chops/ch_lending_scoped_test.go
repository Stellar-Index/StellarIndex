package chops

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The scoped walk is a -from re-derive only, never beside the reads or the interest flags.
func TestCheckScoped_RefusesOtherModes(t *testing.T) {
	ids := []string{comet.MainnetBLNTUSDCPool}
	for name, r := range map[string]lendingRun{
		"no -from":        {contracts: ids},
		"-prove":          {contracts: ids, from: 1, prove: true},
		"-print-prefixes": {contracts: ids, from: 1, printPrefixes: true},
		"-interest-only":  {contracts: ids, from: 1, interest: interestFlags{only: true}},
	} {
		if err := r.checkScoped(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (lendingRun{contracts: ids, from: 1}).checkScoped(); err != nil {
		t.Errorf("-lending-contract -from: %v", err)
	}
}

// The range stays at or under the watermark, which the walk never moves: it cannot write hours the watermark lacks.
func TestScopedRange_StaysUnderTheWatermark(t *testing.T) {
	if lo, hi, err := scopedRange(100, 0, 500); err != nil || lo != 100 || hi != 500 {
		t.Errorf("to = 0: [%d,%d] %v, want [100,500]", lo, hi, err)
	}
	if lo, hi, err := scopedRange(100, 300, 500); err != nil || lo != 100 || hi != 300 {
		t.Errorf("to = 300: [%d,%d] %v", lo, hi, err)
	}
	for _, c := range [][3]uint32{{100, 501, 500}, {600, 0, 500}, {100, 0, 0}, {300, 200, 500}} {
		if _, _, err := scopedRange(c[0], c[1], c[2]); err == nil {
			t.Errorf("scopedRange%v accepted", c)
		}
	}
}

// A Comet pool with no instance row is reported unwalked, naming the scoped walk, whether or not the lake holds it.
func TestLendingContractKeys_UnwalkedCometPool(t *testing.T) {
	c := clickhouse.LendingContract{ID: comet.MainnetBLNTUSDCPool, Protocol: clickhouse.LendingComet}
	if note, failed := lendingContractKeys(c, nil); note != lendingUnwalked || failed != nil {
		t.Errorf("unwalked, not in the lake: %q %q", note, failed)
	}
	live := map[string]lendingKeyCount{c.ID + "/AllRecordData": {current: 1}}
	if note, failed := lendingContractKeys(c, live); note != "" || len(failed) != 1 || !strings.Contains(failed[0], "scoped walk") {
		t.Errorf("unwalked, live in the lake: %q %q", note, failed)
	}
}

// Only registered contracts are scoped, and a Comet pool alone arms no interest redo.
func TestScopeLendingRegistry(t *testing.T) {
	cps, err := lendingCometPools()
	if err != nil {
		t.Fatal(err)
	}
	fixed := clickhouse.LendingContract{ID: blnt.MainnetFixedPool, Protocol: clickhouse.LendingBLNT}
	reg := append([]clickhouse.LendingContract{fixed}, cps...)
	got, err := scopeLendingRegistry(reg, []string{comet.MainnetBLNTUSDCPool, comet.MainnetBLNTXLMPool})
	if err != nil || len(got) != 2 || got[0].ID != comet.MainnetBLNTUSDCPool {
		t.Fatalf("scope = %v, %v", got, err)
	}
	if n := len(lendingPools(got)); n != 0 {
		t.Errorf("comet scope has %d interest pools, want 0 (no redo)", n)
	}
	if mixed, _ := scopeLendingRegistry(reg, []string{fixed.ID, comet.MainnetBLNTXLMPool}); len(lendingPools(mixed)) != 1 {
		t.Errorf("a scope with a lending pool must arm the redo: %v", mixed)
	}
	if _, err := scopeLendingRegistry(reg, []string{"CUNREGISTERED"}); err == nil || !strings.Contains(err.Error(), "not a registered") {
		t.Errorf("unregistered contract: %v", err)
	}
}
