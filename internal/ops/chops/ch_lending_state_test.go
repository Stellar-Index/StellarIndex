package chops

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestCheckLendingRegistry_RefusesAMissingKnownPool(t *testing.T) {
	fixed := clickhouse.LendingContract{ID: blnt.MainnetFixedPool, Protocol: clickhouse.LendingBLNT}
	yield := clickhouse.LendingContract{ID: blnt.MainnetYieldBlentPool, Protocol: clickhouse.LendingBLNT}
	if err := checkLendingRegistry([]clickhouse.LendingContract{fixed, yield}, 70_000_000); err != nil {
		t.Fatalf("complete registry refused: %v", err)
	}
	err := checkLendingRegistry([]clickhouse.LendingContract{fixed}, 70_000_000)
	if err == nil || !strings.Contains(err.Error(), blnt.MainnetYieldBlentPool) {
		t.Fatalf("registry without the yield pool: err = %v, want it named", err)
	}
	// A pool deployed after the registry's ledger is not required yet.
	if err := checkLendingRegistry([]clickhouse.LendingContract{fixed}, 65_200_000); err != nil {
		t.Fatalf("registry before the yield pool's deploy refused: %v", err)
	}
	// Listed under the wrong protocol, or only as a backstop, is still missing.
	wrong := clickhouse.LendingContract{ID: blnt.MainnetYieldBlentPool, Protocol: clickhouse.LendingBlendV2}
	if err := checkLendingRegistry([]clickhouse.LendingContract{fixed, wrong}, 70_000_000); err == nil {
		t.Fatal("yield pool under blend_v2 accepted")
	}
}

// Every key changed since the watermark is compared, proved new, or counted not compared; none is silently passed.
func TestLendingProof_ClassifiesEveryKey(t *testing.T) {
	const wm = uint32(5_000_000)
	h := time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC)
	ks := []clickhouse.LendingProofKey{
		{Key: "same", Present: true, Same: true},
		{Key: "differs", Present: true},
		{Key: "missing"},
		{Key: "ahead", AheadHit: true},
		{Key: "c-ok", Changed: true, Present: true, Ledger: wm - 10, Entry: "E"},
		{Key: "c-later", Changed: true, Present: true, Ledger: wm - 10, Entry: "E"},
		{Key: "c-gone", Changed: true, Present: true, Ledger: wm - 10, Entry: "E"},
		{Key: "c-ahead", Changed: true, Present: true, Ledger: wm - 10, Entry: "E", Ahead: []time.Time{h}},
		{Key: "c-deep", Changed: true, Present: true, Ledger: wm - lendingProofDepth - 1},
		{Key: "n-new", Changed: true},
		{Key: "n-hole", Changed: true},
		{Key: "n-unknown", Changed: true},
	}
	floors := map[string]uint32{"n-new": wm - 100, "n-hole": wm - 100}
	p, pend := classifyLendingKeys(ks, wm, func(k string) uint32 { return floors[k] })
	los := map[string]uint32{}
	for _, q := range pend {
		los[q.k.Key] = q.lo
	}
	if los["c-ok"] != wm-10 || los["n-new"] != wm-100 || los["n-unknown"] != wm-lendingProofDepth || len(pend) != 7 {
		t.Fatalf("pending reads = %v, want each from its history ledger, deploy floor, or the depth bound", los)
	}
	p.settle(pend, map[string]clickhouse.LendingChange{
		"c-ok":    {Ledger: wm - 10, Entry: "E", Hour: h},
		"c-later": {Ledger: wm - 5, Entry: "E", Hour: h}, // a change the history missed, same entry
		"c-ahead": {Ledger: wm - 5, Entry: "F", Hour: h},
		"n-hole":  {Ledger: wm - 50, Entry: "X", Hour: h},
	})
	// compared: 4 unchanged + c-ok, c-later, c-gone, c-ahead, n-hole
	if p.keys != 9 || p.mismatched != 5 || p.ahead != 2 {
		t.Errorf("keys/mismatched/ahead = %d/%d/%d, want 9/5/2", p.keys, p.mismatched, p.ahead)
	}
	if !slices.Equal(p.deep, []string{"c-deep"}) || !slices.Equal(p.unplaced, []string{"n-unknown"}) {
		t.Errorf("not compared: deep %q, unplaced %q; want [c-deep], [n-unknown]", p.deep, p.unplaced)
	}
	var out strings.Builder
	if err := p.report(&out, wm); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"not compared (history row too deep): c-deep", "lake not read to the contract's deploy): n-unknown", "INCOMPLETE"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report %q lacks %q", out.String(), want)
		}
	}
}
