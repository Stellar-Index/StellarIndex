package timescale

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func directoryEntriesN(n int) []DirectoryEntry {
	out := make([]DirectoryEntry, n)
	for i := range out {
		out[i] = DirectoryEntry{
			Address: fmt.Sprintf("G%055d", i),
			Name:    fmt.Sprintf("Entry %d", i),
			Domain:  "example.org",
			Tags:    []string{"exchange"},
			Source:  "stellar-expert",
		}
	}
	return out
}

// TestBuildDirectoryUpsert_PlaceholderLayout — 4 params per row plus
// exactly one shared source param at the tail position every row
// references. A drifted placeholder count corrupts column/value
// alignment silently (the driver reports nothing when counts still
// happen to match).
func TestBuildDirectoryUpsert_PlaceholderLayout(t *testing.T) {
	chunk := directoryEntriesN(3)
	q, args := buildDirectoryUpsert(chunk, "stellar-expert")

	if got, want := len(args), 3*4+1; got != want {
		t.Fatalf("len(args) = %d, want %d (4 per row + 1 shared source)", got, want)
	}
	if args[len(args)-1] != "stellar-expert" {
		t.Errorf("last arg = %v, want the shared source", args[len(args)-1])
	}
	// Every row must reference the shared source placeholder ($13 for
	// a 3-row chunk) in the pre-now() position — once per VALUES tuple.
	if got := strings.Count(q, "$13, now())"); got != 3 {
		t.Errorf("shared source placeholder used %d times, want 3 (once per row)", got)
	}
	// Highest per-row placeholder is $12; $14 must not exist.
	if strings.Contains(q, "$14") {
		t.Error("statement references $14 — placeholder math drifted past the arg list")
	}
	if !strings.Contains(q, "ON CONFLICT (address) DO UPDATE") {
		t.Error("statement lost its upsert arm")
	}
}

// TestDedupDirectoryEntriesByAddress — RA-3: two upstream files
// declaring the same address body would put the same conflict key
// twice into one multi-row upsert chunk, which Postgres rejects
// ("ON CONFLICT DO UPDATE command cannot affect row a second time"),
// aborting the whole day's sync. ReplaceDirectory must collapse
// duplicate addresses (last-wins) before chunking so the statement is
// always single-key-per-chunk.
func TestDedupDirectoryEntriesByAddress(t *testing.T) {
	in := []DirectoryEntry{
		{Address: "GA", Name: "first-A", Source: "stellar-expert"},
		{Address: "GB", Name: "only-B", Source: "stellar-expert"},
		{Address: "GA", Name: "second-A", Source: "stellar-expert"}, // dup of GA
		{Address: "GC", Name: "only-C", Source: "stellar-expert"},
		{Address: "GB", Name: "second-B", Source: "stellar-expert"}, // dup of GB
	}
	got := dedupDirectoryEntriesByAddress(in)

	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 distinct addresses (%+v)", len(got), got)
	}
	// No address may appear twice — a duplicate here is exactly the
	// "affect row a second time" abort in SQL form.
	seen := map[string]int{}
	for _, e := range got {
		seen[e.Address]++
		if seen[e.Address] > 1 {
			t.Fatalf("address %s survived twice — chunk would abort the upsert", e.Address)
		}
	}
	// Last-wins: the later occurrence's fields must be the ones kept.
	byAddr := map[string]DirectoryEntry{}
	for _, e := range got {
		byAddr[e.Address] = e
	}
	if byAddr["GA"].Name != "second-A" {
		t.Errorf("GA Name = %q, want last-wins %q", byAddr["GA"].Name, "second-A")
	}
	if byAddr["GB"].Name != "second-B" {
		t.Errorf("GB Name = %q, want last-wins %q", byAddr["GB"].Name, "second-B")
	}
	// Surviving relative order follows first appearance: GA, GB, GC.
	if got[0].Address != "GA" || got[1].Address != "GB" || got[2].Address != "GC" {
		t.Errorf("order = %s,%s,%s, want GA,GB,GC", got[0].Address, got[1].Address, got[2].Address)
	}
}

// TestReplaceDirectory_RefusesEmptySet — an empty snapshot means the
// fetch broke; syncing it would prune the whole source. Must error
// before touching the DB (s.db is nil here — a DB call would panic,
// so reaching the guard proves order).
func TestReplaceDirectory_RefusesEmptySet(t *testing.T) {
	s := &Store{}
	if _, _, err := s.ReplaceDirectory(context.Background(), "stellar-expert", nil); err == nil {
		t.Fatal("ReplaceDirectory(empty) = nil error, want refusal")
	}
	if _, _, err := s.ReplaceDirectory(context.Background(), "", directoryEntriesN(1)); err == nil {
		t.Fatal("ReplaceDirectory(no source) = nil error, want refusal")
	}
}

// TestDirectoryTagsWithoutScamFlags pins the correction a false-positive
// override writes: every scam-class spelling the gate would count goes,
// and every other tag stays — `issuer` above all, since it is what admits
// the address to the RWA recognition funnel.
func TestDirectoryTagsWithoutScamFlags(t *testing.T) {
	got := DirectoryTagsWithoutScamFlags([]string{"issuer", "UNSAFE", "memo-required", " Malicious ", "anchor", "phishing"})
	want := []string{"issuer", "memo-required", "anchor"}
	if !slices.Equal(got, want) {
		t.Fatalf("DirectoryTagsWithoutScamFlags = %q, want %q", got, want)
	}
	for _, tag := range DirectoryScamFlagTags {
		if left := DirectoryTagsWithoutScamFlags([]string{tag, "issuer"}); !slices.Equal(left, []string{"issuer"}) {
			t.Errorf("scam tag %q survived: %q", tag, left)
		}
	}
	if got := DirectoryTagsWithoutScamFlags(nil); got == nil || len(got) != 0 {
		t.Errorf("DirectoryTagsWithoutScamFlags(nil) = %#v, want a non-nil empty set (tags is NOT NULL)", got)
	}
}

// TestBuildDirectoryUpsert_BindsCanonicalTags: the SQL scam predicates
// fold case but do not trim, so a padded upstream tag must be stored
// canonical or the churn guard, the rank tier and the explorer miss a
// flag the trimming Go price gate still acts on.
func TestBuildDirectoryUpsert_BindsCanonicalTags(t *testing.T) {
	chunk := directoryEntriesN(1)
	chunk[0].Tags = []string{"scam ", " Exchange", "", "exchange", "\tPHISHING\n", "  "}
	_, args := buildDirectoryUpsert(chunk, "stellar-expert")
	got, ok := args[3].([]string)
	if !ok {
		t.Fatalf("tags arg is %T, want []string", args[3])
	}
	if want := []string{"scam", "exchange", "phishing"}; !slices.Equal(got, want) {
		t.Fatalf("bound tags = %q, want %q", got, want)
	}
	for _, tag := range got {
		if isDirectoryScamFlagTag(tag) && !slices.Contains(DirectoryScamFlagTags, strings.ToLower(tag)) {
			t.Errorf("stored tag %q is flagged by the Go rule but not by the lower(t) SQL rule", tag)
		}
	}
	if got := CanonicalDirectoryTags(nil); got == nil || len(got) != 0 {
		t.Errorf("CanonicalDirectoryTags(nil) = %#v, want a non-nil empty set (tags is NOT NULL)", got)
	}
}

// TestDirectoryChurn_BoundsUnflagged: a snapshot that keeps every row
// but strips the scam tags un-withholds every flagged issuer, the same
// harm as pruning them, so it must hit the ceiling too.
func TestDirectoryChurn_BoundsUnflagged(t *testing.T) {
	before := &directoryChurn{rows: 4000, flagged: map[string]struct{}{}}
	for i := range 1000 {
		before.flagged[fmt.Sprintf("G%055d", i)] = struct{}{}
	}

	newly, cleared := before.tally(nil)
	if newly != 0 || cleared != 1000 {
		t.Fatalf("tags stripped: newly=%d cleared=%d, want 0/1000", newly, cleared)
	}
	err := before.check(DefaultDirectoryChurnLimit, DirectorySyncResult{NewlyFlagged: newly, Unflagged: cleared})
	if !errors.Is(err, ErrDirectoryChurnExceeded) {
		t.Fatalf("1000 of 4000 un-flagged in place: err = %v, want ErrDirectoryChurnExceeded", err)
	}

	// Ordinary churn: 50 cleared (the 5 %-of-1000-flagged un-flag cap)
	// and 3 newly flagged (under the 200-row cap).
	after := []string{"G" + strings.Repeat("7", 55), "G" + strings.Repeat("6", 55), "G" + strings.Repeat("5", 55)}
	for i := 50; i < 1000; i++ {
		after = append(after, fmt.Sprintf("G%055d", i))
	}
	newly, cleared = before.tally(after)
	if newly != 3 || cleared != 50 {
		t.Fatalf("ordinary churn: newly=%d cleared=%d, want 3/50", newly, cleared)
	}
	if err := before.check(DefaultDirectoryChurnLimit, DirectorySyncResult{NewlyFlagged: newly, Unflagged: cleared}); err != nil {
		t.Fatalf("ordinary churn refused: %v", err)
	}
}

// TestDirectoryChurnLimit_Ceiling pins the arithmetic behind the sync
// refusal: 5 % of the held rows, never below the floor, unbounded
// for a source's first sync and for the operator's explicit opt-in.
// The refusal itself runs against Postgres in
// test/integration/pg_accounts_test.go.
func TestDirectoryChurnLimit_Ceiling(t *testing.T) {
	cases := []struct {
		name     string
		limit    DirectoryChurnLimit
		existing int64
		want     int64
		bounded  bool
	}{
		{"default over the live table", DefaultDirectoryChurnLimit, 18500, 925, true},
		{"default rounds up", DefaultDirectoryChurnLimit, 18501, 926, true},
		{"floor holds a small table open", DefaultDirectoryChurnLimit, 2, 100, true},
		{"floor is the minimum, not an offset", DefaultDirectoryChurnLimit, 2000, 100, true},
		{"first sync is unbounded", DefaultDirectoryChurnLimit, 0, 0, false},
		{"operator opt-in is unbounded", DirectoryChurnUnbounded, 18500, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bounded := tc.limit.ceiling(tc.existing)
			if bounded != tc.bounded || got != tc.want {
				t.Fatalf("ceiling(%d) = (%d, %v), want (%d, %v)", tc.existing, got, bounded, tc.want, tc.bounded)
			}
		})
	}
}

// TestDirectoryChurn_UnflagCeilingIsSizedOnTheFlaggedSet: the un-flag
// cap is a fraction of the addresses flagged before the sync, not of
// every row the source holds. Flagged rows are a minority, so a
// row-sized cap (925 on the live table) admitted clearing almost the
// whole flagged set in one run.
func TestDirectoryChurn_UnflagCeilingIsSizedOnTheFlaggedSet(t *testing.T) {
	cases := []struct {
		name      string
		rows      int64
		flagged   int
		unflagged int64
		refused   bool
	}{
		{"row-sized cap no longer admits clearing the set", 18500, 1000, 925, true},
		{"one over 5 % of the flagged set", 18500, 1000, 51, true},
		{"5 % of the flagged set", 18500, 1000, 50, false},
		{"floor holds a small flagged set open", 18500, 40, 10, false},
		{"floor is the minimum, not an offset", 18500, 40, 11, true},
		{"whole small flagged set", 18500, 40, 40, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := &directoryChurn{rows: tc.rows, flagged: map[string]struct{}{}}
			for i := range tc.flagged {
				before.flagged[fmt.Sprintf("G%055d", i)] = struct{}{}
			}
			err := before.check(DefaultDirectoryChurnLimit, DirectorySyncResult{Unflagged: tc.unflagged})
			if got := errors.Is(err, ErrDirectoryChurnExceeded); got != tc.refused {
				t.Fatalf("%d of %d flagged un-flagged: err = %v, refused = %v, want %v", tc.unflagged, tc.flagged, err, got, tc.refused)
			}
			if err := before.check(DirectoryChurnUnbounded, DirectorySyncResult{Unflagged: tc.unflagged}); err != nil {
				t.Fatalf("-accept-churn refused %d un-flags: %v", tc.unflagged, err)
			}
		})
	}
}

func TestDirectoryChurnLimit_UnflagCeiling(t *testing.T) {
	if got, ok := DefaultDirectoryChurnLimit.unflagCeiling(1001); !ok || got != 51 {
		t.Fatalf("unflagCeiling(1001) = (%d, %v), want (51, true): rounds up", got, ok)
	}
	if _, ok := DefaultDirectoryChurnLimit.unflagCeiling(0); ok {
		t.Fatal("unflagCeiling(0) bounded, want unbounded: nothing flagged, nothing to clear")
	}
	if _, ok := DirectoryChurnUnbounded.unflagCeiling(1000); ok {
		t.Fatal("DirectoryChurnUnbounded.unflagCeiling bounded, want unbounded")
	}
}

// TestReplaceDirectoryWithin_RefusesBeforeTheDB — the argument checks
// run before any DB call (s.db is nil: reaching one would panic), for
// the bounded entry point exactly as for ReplaceDirectory.
func TestReplaceDirectoryWithin_RefusesBeforeTheDB(t *testing.T) {
	s := &Store{}
	ctx := t.Context()
	if _, err := s.ReplaceDirectoryWithin(ctx, "stellar-expert", nil, DirectoryChurnUnbounded); err == nil {
		t.Fatal("ReplaceDirectoryWithin(empty) = nil error, want refusal")
	}
	if _, err := s.ReplaceDirectoryWithin(ctx, DirectoryOperatorOverrideSource, directoryEntriesN(1), DirectoryChurnUnbounded); err == nil {
		t.Fatal("ReplaceDirectoryWithin(operator-override source) = nil error, want refusal")
	}
}

// TestBuildDirectoryUpsert_ConflictArmIsOwnershipScoped — the conflict
// arm must update only rows the syncing source already OWNS.
//
// Two things broke when it did not. Migration 0136 promises the table is
// "scoped by `source` so a future second directory source can coexist
// without the syncs deleting each other's rows", and an unconditional
// `source = EXCLUDED.source` makes every sync steal every shared address
// from the other. And with `source` rewritten there is no durable
// operator override for a false-positive scam flag at all: a hand-held
// correction is adopted into the upstream snapshot and overwritten by
// the next daily run, while the issuer's price stays withheld.
func TestBuildDirectoryUpsert_ConflictArmIsOwnershipScoped(t *testing.T) {
	q, _ := buildDirectoryUpsert(directoryEntriesN(2), "stellar-expert")

	if !strings.Contains(q, "WHERE account_directory.source = EXCLUDED.source") {
		t.Errorf("conflict arm is not ownership-scoped — a sync would overwrite rows another source owns:\n%s", q)
	}
	// `source` must never appear in the SET list: the WHERE already pins
	// it equal, and rewriting it is exactly how a row changed hands.
	// EXCLUDED.source may therefore be referenced exactly once, by the
	// guard.
	if got := strings.Count(q, "EXCLUDED.source"); got != 1 {
		t.Errorf("EXCLUDED.source referenced %d times, want 1 (the ownership guard only):\n%s", got, q)
	}
}

// TestReplaceDirectory_RefusesReservedOverrideSource — a sync running as
// the operator source would adopt every override into its snapshot and
// then prune the ones that snapshot omits, deleting the corrections the
// source exists to protect. Must refuse before touching the DB (s.db is
// nil here, so a DB call would panic — reaching the guard proves order).
func TestReplaceDirectory_RefusesReservedOverrideSource(t *testing.T) {
	s := &Store{}
	_, _, err := s.ReplaceDirectory(context.Background(), DirectoryOperatorOverrideSource, directoryEntriesN(1))
	if err == nil {
		t.Fatal("ReplaceDirectory(operator-override source) = nil error, want refusal")
	}
	if !strings.Contains(err.Error(), DirectoryOperatorOverrideSource) {
		t.Errorf("error %q does not name the reserved source", err)
	}
}
