// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func curatedEntriesN(n int) []CuratedRWAEntry {
	out := make([]CuratedRWAEntry, 0, n)
	for i := range n {
		out = append(out, CuratedRWAEntry{
			Address:       fmt.Sprintf("C%055d", i),
			AssetCode:     fmt.Sprintf("T%d", i),
			Company:       "Example",
			AssetSubclass: "US Treasuries",
			PriceUSD:      "1.00",
			PricedAt:      time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC),
		})
	}
	return out
}

func TestBuildCuratedRWAUpsert_PlaceholderLayout(t *testing.T) {
	t.Parallel()

	q, args := buildCuratedRWAUpsert("dune:stellar", curatedEntriesN(3), "dune.stellar.dataset_recognized_assets")
	if got, want := len(args), 3*9; got != want {
		t.Fatalf("len(args) = %d, want %d (9 per row)", got, want)
	}
	if strings.Contains(q, "$28") {
		t.Error("statement references $28 — placeholder math drifted past the arg list")
	}
	if !strings.Contains(q, "ON CONFLICT (curator, address) DO UPDATE") {
		t.Error("statement lost its upsert arm, or its conflict target is not the (curator, address) key")
	}
	// synced_at is transaction-stable now(): the prune's "same curator,
	// older synced_at" depends on every row of one run sharing a clock.
	if strings.Contains(q, "clock_timestamp()") {
		t.Error("synced_at must be transaction-stable now(), never clock_timestamp()")
	}
	// Explicit casts on the price and time binds — an untyped parameter
	// is left for Postgres to infer, which is the 42883-class trap.
	for _, want := range []string{"$7, '')::numeric", "$8::timestamptz"} {
		if !strings.Contains(q, want) {
			t.Errorf("statement must bind %s explicitly; got:\n%s", want, q)
		}
	}
}

// A price never travels without its stamp, and a stamp never without a
// price: the table CHECK refuses one without the other, and a price with
// no verifiable age is exactly the value the bound exists to reject.
func TestBuildCuratedRWAUpsert_PriceAndTimeTravelTogether(t *testing.T) {
	t.Parallel()

	stamp := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		entry     CuratedRWAEntry
		wantBound bool
	}{
		{"price with stamp", CuratedRWAEntry{Address: "C" + strings.Repeat("A", 55), PriceUSD: "1.00", PricedAt: stamp}, true},
		{"price without stamp", CuratedRWAEntry{Address: "C" + strings.Repeat("A", 55), PriceUSD: "1.00"}, false},
		{"stamp without price", CuratedRWAEntry{Address: "C" + strings.Repeat("A", 55), PricedAt: stamp}, false},
		{"neither", CuratedRWAEntry{Address: "C" + strings.Repeat("A", 55)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, args := buildCuratedRWAUpsert("dune:stellar", []CuratedRWAEntry{tc.entry}, "src")
			price, pricedAt := args[6], args[7]
			if tc.wantBound {
				if price != "1.00" || pricedAt == nil {
					t.Fatalf("bound (price=%v, priced_at=%v), want both present", price, pricedAt)
				}
				return
			}
			if price != "" || pricedAt != nil {
				t.Fatalf("bound (price=%v, priced_at=%v), want both empty — a price and its stamp travel together or not at all", price, pricedAt)
			}
		})
	}
}

func TestDedupCuratedRWAEntries_KeepsTheLast(t *testing.T) {
	t.Parallel()

	a := "C" + strings.Repeat("A", 55)
	got := dedupCuratedRWAEntries([]CuratedRWAEntry{
		{Address: a, Company: "first"},
		{Address: "C" + strings.Repeat("B", 55), Company: "other"},
		{Address: a, Company: "last"},
	})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Address != a || got[0].Company != "last" {
		t.Errorf("first slot = %+v, want the LAST entry for %s", got[0], a[:8])
	}
}

func TestReplaceCuratedRWADirectory_Refusals(t *testing.T) {
	t.Parallel()

	s := &Store{}
	ctx := context.Background()
	if _, _, err := s.ReplaceCuratedRWADirectory(ctx, "dune:stellar", nil, "src"); err == nil {
		t.Fatal("empty set accepted — it would prune the whole curator into silence")
	}
	if _, _, err := s.ReplaceCuratedRWADirectory(ctx, "", curatedEntriesN(1), "src"); err == nil {
		t.Fatal("empty curator accepted")
	}
	if _, _, err := s.ReplaceCuratedRWADirectory(ctx, "dune:stellar", curatedEntriesN(1), ""); err == nil {
		t.Fatal("empty source accepted")
	}
}

// Both bounds must be spliced into every read, asserted on the RENDERED
// predicates: a constant nothing splices in bounds nothing.
func TestCuratedRWADirectory_BothBoundsAreEnforcedInSQL(t *testing.T) {
	t.Parallel()

	wantRecognition := "synced_at > now() - INTERVAL '" + curatedRWARecognitionMaxAge + "'"
	wantPrice := "priced_at > now() - INTERVAL '" + curatedRWAPriceMaxAge + "'"
	for name, q := range map[string]string{
		"by-address read": curatedRWAByAddressSQL,
		"census":          curatedRWACensusSQL,
	} {
		if !strings.Contains(q, wantRecognition) {
			t.Errorf("%s does not enforce the recognition bound", name)
		}
		if !strings.Contains(q, wantPrice) {
			t.Errorf("%s does not enforce the price bound", name)
		}
		if !strings.Contains(q, "curator = $1") {
			t.Errorf("%s is not scoped to one curator", name)
		}
	}
	// A stale price must read as an ABSENCE, never as a value: the read
	// NULLs the price columns behind the same predicate the census counts
	// with, so the two cannot disagree.
	if strings.Count(curatedRWAByAddressSQL, "CASE WHEN "+curatedRWAPriceFreshSQL) != 2 {
		t.Error("the read does not gate BOTH price columns on the price bound")
	}
}

func TestCuratedRWABounds_AreParseableIntervals(t *testing.T) {
	t.Parallel()

	for _, b := range []string{curatedRWARecognitionMaxAge, curatedRWAPriceMaxAge} {
		fields := strings.Fields(b)
		if len(fields) != 2 {
			t.Fatalf("bound %q is not `<n> <unit>`", b)
		}
	}
	// The two clocks are deliberately different lengths and for stated
	// reasons: recognition is OUR daily sync (48h = one missed run), the
	// price is the CURATOR's upload cadence (a week). A change that made
	// them equal or inverted would have to say why.
	if curatedRWARecognitionMaxAge != "48 hours" || curatedRWAPriceMaxAge != "7 days" {
		t.Errorf("bounds = (%q, %q); a change here must update the reasoning in the file header", curatedRWARecognitionMaxAge, curatedRWAPriceMaxAge)
	}
}
