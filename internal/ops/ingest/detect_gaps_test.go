package ingest

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// closedURL returns an http URL on a loopback port nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// TestDetectGaps_RPCFlagOverridesDeadConfigEndpoint: -rpc replaces stellar.rpc_endpoints[0] for the tip read, since a rendered config may name a loopback stellar-rpc the host does not run.
func TestDetectGaps_RPCFlagOverridesDeadConfigEndpoint(t *testing.T) {
	var tipCalls atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"getLatestLedger"`) {
			tipCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		// closeTime is "now" so this fixture clears the RPC
		// freshness gate and reaches the storage-open failure this test
		// actually pins.
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"id":"ab","protocolVersion":22,"sequence":1000,"closeTime":"%d"}}`,
			time.Now().Unix())
	}))
	defer live.Close()

	dead := closedURL(t)
	path := filepath.Join(t.TempDir(), "stellarindex.toml")
	body := "[stellar]\nrpc_endpoints = [\"" + dead + "\"]\n\n[storage]\n" +
		"postgres_dsn = \"postgres://u:p@" + strings.TrimPrefix(closedURL(t), "http://") +
		"/db?sslmode=disable&connect_timeout=2\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	err := detectGaps([]string{"-config", path})
	if err == nil || !strings.HasPrefix(err.Error(), "rpc:") {
		t.Fatalf("without -rpc: want the config endpoint's rpc error, got %v", err)
	}

	err = detectGaps([]string{"-config", path, "-rpc", live.URL})
	if got := tipCalls.Load(); got != 1 {
		t.Fatalf("-rpc endpoint served %d getLatestLedger calls, want 1 (err=%v)", got, err)
	}
	// Past the tip read, the unreachable DSN is the next failure.
	if err == nil || !strings.HasPrefix(err.Error(), "storage:") {
		t.Fatalf("with -rpc: want to reach storage, got %v", err)
	}
}

// TestDetectGaps_StaleRPCTipFailsBeforeStorage pins the freshness gate: a stuck stellar-rpc would otherwise make every cursor compare "ok" against a frozen tip, so a far-past closeTime fails closed before storage.
func TestDetectGaps_StaleRPCTipFailsBeforeStorage(t *testing.T) {
	staleClose := time.Now().Add(-1 * time.Hour).Unix()
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"id":"ab","protocolVersion":22,"sequence":1000,"closeTime":"%d"}}`,
			staleClose)
	}))
	defer stale.Close()

	path := filepath.Join(t.TempDir(), "stellarindex.toml")
	body := "[stellar]\nrpc_endpoints = [\"" + closedURL(t) + "\"]\n\n[storage]\n" +
		"postgres_dsn = \"postgres://u:p@" + strings.TrimPrefix(closedURL(t), "http://") +
		"/db?sslmode=disable&connect_timeout=2\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	err := detectGaps([]string{"-config", path, "-rpc", stale.URL})
	if err == nil || !strings.HasPrefix(err.Error(), "rpc tip stale:") {
		t.Fatalf("want a stale-tip error, got %v", err)
	}
	if strings.HasPrefix(err.Error(), "storage:") {
		t.Fatalf("stale-tip check must fail BEFORE storage is opened, got %v", err)
	}
}

// TestParseRPCCloseTime: a missing or malformed closeTime errors, never reads as "now".
func TestParseRPCCloseTime(t *testing.T) {
	now := time.Now().Unix()
	got, err := parseRPCCloseTime(strconv.FormatInt(now, 10))
	if err != nil {
		t.Fatalf("valid closeTime: unexpected error %v", err)
	}
	if got.Unix() != now {
		t.Fatalf("parseRPCCloseTime(%d) = %v, want unix %d", now, got, now)
	}

	for _, bad := range []string{"", "not-a-number", "12.5"} {
		if _, err := parseRPCCloseTime(bad); err == nil {
			t.Errorf("parseRPCCloseTime(%q): want error, got nil (fail-closed contract broken)", bad)
		}
	}
}

// TestDetectGaps_ProjectorEnabledDefaultSourcesRegistryOK: enabling the projector with the default enabled_sources builds a registry without error.
func TestDetectGaps_ProjectorEnabledDefaultSourcesRegistryOK(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"id":"ab","protocolVersion":22,"sequence":1000,"closeTime":"%d"}}`,
			time.Now().Unix())
	}))
	defer live.Close()

	path := filepath.Join(t.TempDir(), "stellarindex.toml")
	body := "[stellar]\nrpc_endpoints = [\"" + closedURL(t) + "\"]\n\n[storage]\n" +
		"postgres_dsn = \"postgres://u:p@" + strings.TrimPrefix(closedURL(t), "http://") +
		"/db?sslmode=disable&connect_timeout=2\"\n\n[ingestion.projector]\nenabled = true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	err := detectGaps([]string{"-config", path, "-rpc", live.URL})
	// Registry-build errors surface as "projector registry: ..."; if the
	// default enabled_sources set built cleanly, the next failure is the
	// unreachable storage DSN, same as every other fixture in this file.
	if err == nil || !strings.HasPrefix(err.Error(), "storage:") {
		t.Fatalf("projector-enabled default config: want to reach storage, got %v", err)
	}
}

// TestCatalogueMissingProjectorSources_ReapedSourceFlagged: a catalogued source whose ("projector", name) cursor row was reaped or never created must be reported, not absent because minLedgerBySource only sees existing rows.
func TestCatalogueMissingProjectorSources_ReapedSourceFlagged(t *testing.T) {
	cursors := []timescale.Cursor{
		{Source: "ledgerstream", Sub: "", LastLedger: 900_000},
		{Source: "projector", Sub: "soroswap", LastLedger: 899_500},
		// "aquarius" is catalogued (expected) but its row was reaped —
		// or ingest for it never started.
	}
	expected := []string{"soroswap", "aquarius"}

	got := catalogueMissingProjectorSources(cursors, expected)

	want := []string{"aquarius"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalogueMissingProjectorSources = %v, want %v", got, want)
	}
}

// TestCatalogueMissingProjectorSources_NothingMissing: a fully-cursored catalogue raises no flag.
func TestCatalogueMissingProjectorSources_NothingMissing(t *testing.T) {
	cursors := []timescale.Cursor{
		{Source: "ledgerstream", Sub: "", LastLedger: 900_000},
		{Source: "projector", Sub: "soroswap", LastLedger: 899_500},
		{Source: "projector", Sub: "band", LastLedger: 899_800},
	}
	expected := []string{"soroswap", "band"}

	if got := catalogueMissingProjectorSources(cursors, expected); len(got) != 0 {
		t.Fatalf("catalogueMissingProjectorSources = %v, want none missing", got)
	}
}

// TestMinLedgerBySource_ExcludesOneShotJobNamespaces: finished one-shot job shards (backfill, projected-rebuild, census-backfill, ...) sit millions of ledgers behind tip and would cause a false LAGGING report.
func TestMinLedgerBySource_ExcludesOneShotJobNamespaces(t *testing.T) {
	cursors := []timescale.Cursor{
		{Source: "ledgerstream", Sub: "", LastLedger: 900_000},
		{Source: "projector", Sub: "soroswap", LastLedger: 899_500},
		{Source: "projector", Sub: "band", LastLedger: 899_800},
		// One-shot job shards — abandoned or long-finished.
		{Source: "backfill", Sub: "sdex-shard-1", LastLedger: 12_000},
		{Source: "projected-rebuild", Sub: "shard-9", LastLedger: 4_000},
		{Source: "census-backfill", Sub: "shard-2", LastLedger: 1},
	}

	got := minLedgerBySource(cursors)

	want := map[string]uint32{
		"ledgerstream": 900_000,
		"projector":    899_500, // min across soroswap/band sub-cursors
	}
	if len(got) != len(want) {
		t.Fatalf("minLedgerBySource returned %v, want exactly %v", got, want)
	}
	for source, wantLedger := range want {
		if got[source] != wantLedger {
			t.Errorf("source %q: got %d, want %d", source, got[source], wantLedger)
		}
	}
	for _, oneShot := range []string{"backfill", "projected-rebuild", "census-backfill"} {
		if _, present := got[oneShot]; present {
			t.Errorf("one-shot namespace %q must not appear in the live verdict, got %v", oneShot, got)
		}
	}
}

// TestArchivalNodeConfig_RPCEndpointsNotLoopback: the archival-node role deploys no stellar-rpc, so a loopback endpoint is dead and would fail detect-gaps on every run.
func TestArchivalNodeConfig_RPCEndpointsNotLoopback(t *testing.T) {
	role := filepath.Join("..", "..", "..", "configs", "ansible", "roles", "archival-node")
	tmpl, err := os.ReadFile(filepath.Join(role, "templates", "stellarindex.toml.j2"))
	if err != nil {
		t.Fatal(err)
	}
	loopback := regexp.MustCompile(`127\.0\.0\.1|localhost|\[::1\]`)
	line := regexp.MustCompile(`(?m)^rpc_endpoints\s*=.*$`).FindString(string(tmpl))
	if !strings.Contains(line, "{{ stellar_rpc_endpoints ") || loopback.MatchString(line) {
		t.Fatalf("stellar.rpc_endpoints must render from stellar_rpc_endpoints, got %q", line)
	}

	defaults, err := os.ReadFile(filepath.Join(role, "defaults", "main.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"pubnet", "testnet", "futurenet"} {
		re := regexp.MustCompile(`(?m)^stellar_rpc_endpoints_by_network:\n(?:  .*\n)*?  ` + network + `:\s*(\[.*\])$`)
		m := re.FindStringSubmatch(string(defaults))
		if m == nil {
			t.Errorf("defaults/main.yml: stellar_rpc_endpoints_by_network has no %s entry", network)
			continue
		}
		if !strings.Contains(m[1], "https://") || loopback.MatchString(m[1]) {
			t.Errorf("defaults/main.yml: %s rpc endpoints %s must be a non-loopback https URL", network, m[1])
		}
	}
}
