package timescale

import (
	"context"
	"fmt"
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
// alignment silently (lib/pq reports nothing when counts still
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
