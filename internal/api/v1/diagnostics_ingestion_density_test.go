package v1

import (
	"testing"
	"time"

	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// TestMergeCoverageIntervals covers the four interesting shapes the
// sweep-line merge has to handle: non-overlapping, overlapping,
// adjacent (touching), and out-of-order input. Behavior must match
// the contract documented on the function.
func TestMergeCoverageIntervals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []coverageInterval
		want []coverageInterval
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			name: "single",
			in:   []coverageInterval{{1, 100}},
			want: []coverageInterval{{1, 100}},
		},
		{
			name: "non-overlapping",
			in:   []coverageInterval{{1, 100}, {200, 300}},
			want: []coverageInterval{{1, 100}, {200, 300}},
		},
		{
			name: "overlapping",
			in:   []coverageInterval{{1, 100}, {50, 150}},
			want: []coverageInterval{{1, 150}},
		},
		{
			name: "adjacent (End+1 == next.Start)",
			in:   []coverageInterval{{1, 100}, {101, 200}},
			want: []coverageInterval{{1, 200}},
		},
		{
			name: "out-of-order input gets sorted",
			in:   []coverageInterval{{200, 300}, {1, 100}, {150, 250}},
			want: []coverageInterval{{1, 100}, {150, 300}},
		},
		{
			name: "fully nested (inner inside outer)",
			in:   []coverageInterval{{1, 1000}, {500, 600}},
			want: []coverageInterval{{1, 1000}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeCoverageIntervals(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d intervals %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("interval %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSumCoverageIntervals confirms the inclusive-bounds counting
// (End - Start + 1) per interval — easy to get wrong as an
// off-by-one if Go-loop intuition takes over.
func TestSumCoverageIntervals(t *testing.T) {
	t.Parallel()
	got := sumCoverageIntervals([]coverageInterval{{1, 10}, {100, 100}, {1000, 1099}})
	want := int64(10 + 1 + 100)
	if got != want {
		t.Errorf("sum = %d, want %d", got, want)
	}
}

// TestDecoderSetContains pins the substring-vs-token-match
// distinction. "reflector-dex" must NOT match
// "reflector-dex-extended" if that ever ships, and a leading or
// trailing decoder must still work.
func TestDecoderSetContains(t *testing.T) {
	t.Parallel()
	cases := []struct {
		set, source string
		want        bool
	}{
		{"sdex", "sdex", true},
		{"sdex,soroswap", "sdex", true},
		{"sdex,soroswap", "soroswap", true},
		{"sdex,soroswap,aquarius", "soroswap", true},
		{"sdex,soroswap", "aquarius", false},
		{"reflector-dex-extended", "reflector-dex", false},
		{"", "sdex", false},
		{"sdex", "", false},
	}
	for _, tc := range cases {
		if got := decoderSetContains(tc.set, tc.source); got != tc.want {
			t.Errorf("decoderSetContains(%q, %q) = %v, want %v", tc.set, tc.source, got, tc.want)
		}
	}
}

// TestParseBackfillSubFull covers the three-piece parse — start,
// end, decoder. parseBackfillSub already exists for end-only; this
// is the new helper for density projection.
func TestParseBackfillSubFull(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sub                string
		wantStart, wantEnd int64
		wantDecoder        string
	}{
		{"100-200:sdex", 100, 200, "sdex"},
		{"100-200:sdex,soroswap", 100, 200, "sdex,soroswap"},
		{"2-15300000:sdex", 2, 15300000, "sdex"},
		{"malformed", 0, 0, ""},
		{":sdex", 0, 0, ""},
		{"100:sdex", 0, 0, "sdex"}, // missing dash → decoder OK, start/end zero
	}
	for _, tc := range cases {
		gotStart, gotEnd, gotDecoder := parseBackfillSubFull(tc.sub)
		if gotStart != tc.wantStart || gotEnd != tc.wantEnd || gotDecoder != tc.wantDecoder {
			t.Errorf("parseBackfillSubFull(%q) = (%d, %d, %q), want (%d, %d, %q)",
				tc.sub, gotStart, gotEnd, gotDecoder, tc.wantStart, tc.wantEnd, tc.wantDecoder)
		}
	}
}

// TestComputeSourceDensity covers the full pipeline: cursor rows
// → filter by source → completed-portion extraction → interval
// merge → density computation.
func TestComputeSourceDensity(t *testing.T) {
	t.Parallel()
	now := time.Now()

	cases := []struct {
		name           string
		cursors        []timescale.Cursor
		source         string
		genesis        int64
		tip            int64
		wantCovered    int64
		wantDensityMin float64 // inclusive lower bound
		wantDensityMax float64 // inclusive upper bound
	}{
		{
			name:           "no cursors → zero density",
			cursors:        nil,
			source:         "sdex",
			genesis:        1,
			tip:            1000,
			wantCovered:    0,
			wantDensityMin: 0.0,
			wantDensityMax: 0.0,
		},
		{
			name: "single complete range covers full expected → 100%",
			cursors: []timescale.Cursor{
				{Source: "backfill", Sub: "1-1000:sdex", LastLedger: 1000, UpdatedAt: now},
			},
			source:         "sdex",
			genesis:        1,
			tip:            1000,
			wantCovered:    1000,
			wantDensityMin: 1.0,
			wantDensityMax: 1.0,
		},
		{
			name: "partial range (worker only got halfway)",
			cursors: []timescale.Cursor{
				{Source: "backfill", Sub: "1-1000:sdex", LastLedger: 500, UpdatedAt: now},
			},
			source:         "sdex",
			genesis:        1,
			tip:            1000,
			wantCovered:    500,
			wantDensityMin: 0.499,
			wantDensityMax: 0.501,
		},
		{
			name: "two non-overlapping ranges cover ~half",
			cursors: []timescale.Cursor{
				{Source: "backfill", Sub: "1-200:sdex", LastLedger: 200, UpdatedAt: now},
				{Source: "backfill", Sub: "501-800:sdex", LastLedger: 800, UpdatedAt: now},
			},
			source:         "sdex",
			genesis:        1,
			tip:            1000,
			wantCovered:    200 + 300, // [1,200] + [501,800]
			wantDensityMin: 0.499,
			wantDensityMax: 0.501,
		},
		{
			name: "overlapping ranges get merged (don't double-count)",
			cursors: []timescale.Cursor{
				{Source: "backfill", Sub: "1-500:sdex", LastLedger: 500, UpdatedAt: now},
				{Source: "backfill", Sub: "300-1000:sdex", LastLedger: 1000, UpdatedAt: now},
			},
			source:         "sdex",
			genesis:        1,
			tip:            1000,
			wantCovered:    1000, // merged [1, 1000], not 500 + 700 = 1200
			wantDensityMin: 1.0,
			wantDensityMax: 1.0,
		},
		{
			name: "multi-decoder cursor: includes source when present",
			cursors: []timescale.Cursor{
				{Source: "backfill", Sub: "1-1000:sdex,soroswap", LastLedger: 1000, UpdatedAt: now},
			},
			source:         "soroswap",
			genesis:        1,
			tip:            1000,
			wantCovered:    1000,
			wantDensityMin: 1.0,
			wantDensityMax: 1.0,
		},
		{
			name: "multi-decoder cursor: excludes when source absent",
			cursors: []timescale.Cursor{
				{Source: "backfill", Sub: "1-1000:sdex,soroswap", LastLedger: 1000, UpdatedAt: now},
			},
			source:         "aquarius",
			genesis:        1,
			tip:            1000,
			wantCovered:    0,
			wantDensityMin: 0.0,
			wantDensityMax: 0.0,
		},
		{
			name: "non-backfill cursors ignored (live ledgerstream)",
			cursors: []timescale.Cursor{
				{Source: "ledgerstream", Sub: "", LastLedger: 1000, UpdatedAt: now},
			},
			source:         "sdex",
			genesis:        1,
			tip:            1000,
			wantCovered:    0, // density is backfill-only by design
			wantDensityMin: 0.0,
			wantDensityMax: 0.0,
		},
		{
			name: "range extends past tip → clamped",
			cursors: []timescale.Cursor{
				{Source: "backfill", Sub: "1-2000:sdex", LastLedger: 2000, UpdatedAt: now},
			},
			source:         "sdex",
			genesis:        1,
			tip:            1000,
			wantCovered:    1000, // clamped to [1, 1000]
			wantDensityMin: 1.0,
			wantDensityMax: 1.0,
		},
		{
			name: "range starts before genesis → clamped",
			cursors: []timescale.Cursor{
				{Source: "backfill", Sub: "1-1000:sdex", LastLedger: 1000, UpdatedAt: now},
			},
			source:         "sdex",
			genesis:        500,
			tip:            1000,
			wantCovered:    501, // [500, 1000] inclusive
			wantDensityMin: 1.0,
			wantDensityMax: 1.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCovered, gotDensity := computeSourceDensity(tc.cursors, tc.source, tc.genesis, tc.tip)
			if gotCovered != tc.wantCovered {
				t.Errorf("covered = %d, want %d", gotCovered, tc.wantCovered)
			}
			if gotDensity < tc.wantDensityMin || gotDensity > tc.wantDensityMax {
				t.Errorf("density = %v, want in [%v, %v]", gotDensity, tc.wantDensityMin, tc.wantDensityMax)
			}
		})
	}
}
