package clickhouse

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// TestTopic0Predicate_MatchesBothTopicEncodings guards the lake string-topic prefilter. topic_0_sym is empty
// for an ScvString topic[0], so `topic_0_sym IN (...)` alone would match nothing for phoenix's create
// announcement.
func TestTopic0Predicate_MatchesBothTopicEncodings(t *testing.T) {
	t.Parallel()
	pred := topic0Predicate([]string{"create"})

	// The Symbol arm every other gated source depends on must survive.
	if !strings.Contains(pred, "topic_0_sym IN ('create')") {
		t.Errorf("predicate dropped the topic_0_sym arm: %s", pred)
	}
	// The String arm must match the exact XDR blob the lake stores.
	wantB64 := scval.MustEncodeString("create")
	if !strings.Contains(pred, "topics_xdr[1] IN ('"+wantB64+"')") {
		t.Errorf("predicate does not match ScvString(\"create\") = %q in topics_xdr[1]: %s",
			wantB64, pred)
	}
	// ClickHouse arrays are 1-indexed; [0] would be an error, not topic[0].
	if strings.Contains(pred, "topics_xdr[0]") {
		t.Errorf("topics_xdr is 1-indexed in ClickHouse; [0] is not topic[0]: %s", pred)
	}
	// The two arms must be OR-ed inside their own parentheses, or the
	// surrounding `AND` binds tighter than the OR and the contract_id /
	// ledger-range predicates are silently voided by a matching topic.
	if !strings.HasPrefix(pred, "(") || !strings.HasSuffix(pred, ")") {
		t.Errorf("predicate must be parenthesised so a preceding AND binds correctly: %s", pred)
	}
	if !strings.Contains(pred, " OR ") {
		t.Errorf("predicate must accept EITHER encoding: %s", pred)
	}
}

// rawTopic0SymInclude matches an INCLUDE predicate on topic_0_sym (`IN (` or
// `= <non-empty value>`). NOT IN, != and the empty-string test only widen a
// scan or single out the String case, so they cannot hide a String-topic row.
var rawTopic0SymInclude = regexp.MustCompile(`topic_0_sym\s+(?i:in)\s*\(|topic_0_sym\s*=\s*('[^']|\?|\$|%)`)

// TestTopic0SymIncludePredicates_OnlyReviewedSites: filtering on topic_0_sym alone silently drops
// String-topic events (phoenix, soroswap, defindex). Route through topic0Predicate, or add the site here with
// why Symbol-only is complete.
func TestTopic0SymIncludePredicates_OnlyReviewedSites(t *testing.T) {
	t.Parallel()
	allowed := map[string]int{
		// topic0Predicate itself, and symbolTopic0Predicate for reconcile sources whose
		// decoder rejects a String topic[0] (pinned by the chops catalogue test).
		"internal/storage/clickhouse/event_reader.go": 2,
		// sep41_supply's decoder matches mint/burn/clawback as Symbols only.
		"internal/storage/clickhouse/supply_reader.go": 1,
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	got := map[string]int{}
	for _, top := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				// The Postgres landing zone fills topic_0_sym for Symbol OR String.
				if d.Name() == "testdata" || rel == "internal/storage/timescale" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				if n := len(rawTopic0SymInclude.FindAllString(line, -1)); n > 0 {
					got[rel] += n
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	for rel, n := range got {
		if n != allowed[rel] {
			t.Errorf("%s: %d topic_0_sym include predicate(s), %d reviewed; use topic0Predicate so String-topic rows still match", rel, n, allowed[rel])
		}
	}
	for rel, n := range allowed {
		if got[rel] != n {
			t.Errorf("%s: reviewed %d topic_0_sym include predicate(s), found %d; update the allow-list", rel, n, got[rel])
		}
	}
}

// TestTopic0Predicate_GatesStringArmOnEmptySymbol pins the conjunct that keeps topics_xdr out of the scan for granules holding only
// other Symbol topics: a String topic[0] always leaves topic_0_sym empty, so admitting the empty string loses no row.
func TestTopic0Predicate_GatesStringArmOnEmptySymbol(t *testing.T) {
	t.Parallel()
	pred := topic0Predicate([]string{"create"})
	want := "(topic_0_sym IN ('create','') AND (topic_0_sym IN ('create') OR topics_xdr[1] IN ('" + scval.MustEncodeString("create") + "')))"
	if pred != want {
		t.Errorf("predicate = %s\nwant        %s", pred, want)
	}
}

// TestTopic0Predicate_SymbolOnlyNamesStillFilter pins that the widening does
// not turn the prefilter into a pass-through: a name that is not requested
// must not appear, in either encoding.
func TestTopic0Predicate_SymbolOnlyNamesStillFilter(t *testing.T) {
	t.Parallel()
	pred := topic0Predicate([]string{"deploy", "add_pool"})
	for _, want := range []string{
		"'deploy'", "'add_pool'",
		"'" + scval.MustEncodeString("deploy") + "'",
		"'" + scval.MustEncodeString("add_pool") + "'",
	} {
		if !strings.Contains(pred, want) {
			t.Errorf("predicate is missing %s: %s", want, pred)
		}
	}
	for _, unwanted := range []string{"'create'", "'transfer'"} {
		if strings.Contains(pred, unwanted) {
			t.Errorf("predicate admits unrequested name %s: %s", unwanted, pred)
		}
	}
}
