package config_test

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
)

// The self-hosting guide's section 4.3 tells operators to `cp configs/example.toml` and edit named keys. So a key the
// guide names must exist in the schema and be visible in that file.
const selfHostingGuide = "docs/operations/self-hosting.md"

// configRefRe matches backticked table-qualified refs: `[storage] clickhouse_addr` or `storage.clickhouse_live_sink =
// false`. Bare keys are not matched: they are ambiguous, and prose like `ledger_entry_changes` (a ClickHouse table)
// would be swept in.
var configRefRe = regexp.MustCompile("`(?:\\[([a-z][a-z_0-9]*)\\] +([a-z][a-z_0-9]*)|([a-z][a-z_0-9]*)\\.([a-z][a-z_0-9]*))")

// Holds the guide to two promises. (1) Every key it names resolves in the schema; Load hard-fails on unknown keys. (2)
// Every key it names is in configs/example.toml; a missing one hides the default-on ClickHouse dual-sink from operators.
// Commented-out keys count as present: example.toml documents opt-in knobs that way.
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

// Pins the one ClickHouse native port three places must agree on: clickhouse_addr's default, the ansible
// clickhouse_tcp_port, the guide. CH's own default 9000 is held by MinIO (section 4.4), so a guide naming only the dial
// address leaves the indexer dialling nothing.
func TestSelfHostingGuide_ClickHouseNativePort(t *testing.T) {
	root := filepath.Join("..", "..")

	_, port, err := net.SplitHostPort(cfg.Default().Storage.ClickHouseAddr)
	if err != nil {
		t.Fatalf("default storage.clickhouse_addr: %v", err)
	}

	defaults, err := os.ReadFile(filepath.Join(root,
		"configs", "ansible", "roles", "archival-node", "defaults", "main.yml"))
	if err != nil {
		t.Fatalf("read archival-node defaults: %v", err)
	}
	m := regexp.MustCompile(`(?m)^clickhouse_tcp_port:\s*(\d+)`).FindSubmatch(defaults)
	if m == nil {
		t.Fatal("archival-node defaults no longer set clickhouse_tcp_port")
	}
	if got := string(m[1]); got != port {
		t.Errorf("ansible clickhouse_tcp_port = %s, storage.clickhouse_addr default port = %s", got, port)
	}

	raw, err := os.ReadFile(filepath.Join(root, selfHostingGuide))
	if err != nil {
		t.Fatalf("read %s: %v", selfHostingGuide, err)
	}
	guide := string(raw)

	if want := "<tcp_port>" + port + "</tcp_port>"; !strings.Contains(guide, want) {
		t.Errorf("%s never tells the operator to set %s in clickhouse-server's config; "+
			"the server stays on 9000 (MinIO's port) and the indexer's dial of :%s fails",
			selfHostingGuide, want, port)
	}

	invocations := 0
	for i, line := range strings.Split(guide, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "clickhouse-client") {
			continue
		}
		invocations++
		if !strings.Contains(line, "--port "+port) {
			t.Errorf("%s:%d: clickhouse-client without --port %s dials the client default 9000 (MinIO): %s",
				selfHostingGuide, i+1, port, strings.TrimSpace(line))
		}
	}
	if invocations == 0 {
		t.Errorf("%s has no clickhouse-client command; the schema-apply step this test guards has moved", selfHostingGuide)
	}
}

// The test-net render sets [region] deployment; pubnet keeps the "production" default so r1's /v1/status wire contract
// is byte-identical. The tier is not a Go literal, so api.testnet.stellarindex.io must not answer "production".
func TestRegionDeployment_DefaultsToProduction(t *testing.T) {
	if got := config.Default().Region.Deployment; got != "production" {
		t.Errorf("Default().Region.Deployment = %q, want production", got)
	}

	c, err := config.LoadReader(strings.NewReader(`
[region]
id = "testnet"
deployment = "testnet"
`), "inline")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Region.Deployment != "testnet" {
		t.Errorf("Region.Deployment = %q, want testnet", c.Region.Deployment)
	}
}
