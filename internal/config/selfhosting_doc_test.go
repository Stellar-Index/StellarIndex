package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
)

// selfHostingGuide is the operator-facing bring-up guide. Its §4.3 tells a
// fresh operator to `cp configs/example.toml` and edit the keys it names, so
// the guide and that file are one contract: a key the guide names must exist
// in the schema AND be visible in the file the reader was told to copy.
const selfHostingGuide = "docs/operations/self-hosting.md"

// configRefRe matches a backticked, table-qualified config reference in either
// form the guide uses: "`[storage] clickhouse_addr`" or
// "`storage.clickhouse_live_sink = false`". Bare keys ("`redis_addr`") are
// deliberately NOT matched — without a table they are ambiguous, and prose
// like "`ledger_entry_changes`" (a ClickHouse table, not a config key) would
// otherwise be swept in.
var configRefRe = regexp.MustCompile("`(?:\\[([a-z][a-z_0-9]*)\\] +([a-z][a-z_0-9]*)|([a-z][a-z_0-9]*)\\.([a-z][a-z_0-9]*))")

// TestSelfHostingGuide_ConfigRefs holds the guide to the two promises it
// makes about configuration, both of which had broken:
//
//	(1) Every key it names resolves against the real schema. The guide told
//	    operators to set `ingestion.clickhouse_projector_source`, but that
//	    field lives under [storage] — and Load rejects unknown keys as a hard
//	    error, so following the guide literally left every binary refusing to
//	    start with "config: unknown keys in ...".
//	(2) Every key it names is present in configs/example.toml. The guide's
//	    §4.3 says to copy that file and edit `[storage] clickhouse_addr` /
//	    `clickhouse_live_sink` — neither of which appeared anywhere in it,
//	    leaving the ON-by-default ClickHouse dual-sink (ADR-0041) as a knob
//	    the operator could not see, on a deployment with no ClickHouse.
//
// Commented-out keys count as present: example.toml documents opt-in knobs
// that way, and a commented line is still a key the reader can find and edit.
func TestSelfHostingGuide_ConfigRefs(t *testing.T) {
	root := filepath.Join("..", "..")

	guide, err := os.ReadFile(filepath.Join(root, selfHostingGuide))
	if err != nil {
		t.Fatalf("read %s: %v", selfHostingGuide, err)
	}
	example, err := os.ReadFile(filepath.Join(root, "configs", "example.toml"))
	if err != nil {
		t.Fatalf("read configs/example.toml: %v", err)
	}

	// Every dotted path the schema knows, plus the set of top-level tables —
	// the latter is what separates a config reference ("ingestion.foo") from
	// incidental dotted prose ("galexie.toml", "apt.stellar.org").
	known := map[string]bool{}
	tables := map[string]bool{}
	for _, f := range cfg.Describe() {
		known[f.Path] = true
		tables[strings.SplitN(f.Path, ".", 2)[0]] = true
	}

	exampleKeys := tomlKeysIn(string(example))

	seen := map[string]bool{}
	refs := 0
	for i, line := range strings.Split(string(guide), "\n") {
		for _, m := range configRefRe.FindAllStringSubmatch(line, -1) {
			table, key := m[1], m[2]
			if table == "" {
				table, key = m[3], m[4]
			}
			if !tables[table] {
				continue // not a config reference
			}
			path := table + "." + key
			refs++
			if seen[path] {
				continue
			}
			seen[path] = true

			if !known[path] {
				t.Errorf("%s:%d names config key %q, which is not in the schema — "+
					"internal/config/load.go rejects unknown keys as a hard error, so an "+
					"operator who follows this line gets a binary that refuses to start",
					selfHostingGuide, i+1, path)
				continue
			}
			if !exampleKeys[key] {
				t.Errorf("%s:%d names config key %q, which appears nowhere in "+
					"configs/example.toml — the guide's §4.3 tells the operator to copy that "+
					"file as their config, so a key it never shows is a knob they cannot find",
					selfHostingGuide, i+1, path)
			}
		}
	}
	// A rewritten guide that stops naming config keys would otherwise pass
	// this test vacuously.
	if refs < 5 {
		t.Fatalf("extracted only %d config references from %s — the extractor has "+
			"drifted from the guide's notation; fix configRefRe rather than letting "+
			"this check read green over nothing", refs, selfHostingGuide)
	}
}

// tomlKeysIn returns every key assigned in body, including keys on
// commented-out lines ("# clickhouse_live_sink = true").
func tomlKeysIn(body string) map[string]bool {
	keys := map[string]bool{}
	assign := regexp.MustCompile(`^#?\s*([a-z][a-z_0-9]*)\s*=`)
	for _, line := range strings.Split(body, "\n") {
		if m := assign.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			keys[m[1]] = true
		}
	}
	return keys
}
