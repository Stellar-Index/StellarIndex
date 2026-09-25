package ingest

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

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

// TestDetectGaps_RPCFlagOverridesDeadConfigEndpoint pins that -rpc replaces
// stellar.rpc_endpoints[0] for the tip read: r1's rendered config named a
// loopback stellar-rpc the host does not run, so without an override the
// command died on "rpc:" before comparing a single cursor.
func TestDetectGaps_RPCFlagOverridesDeadConfigEndpoint(t *testing.T) {
	var tipCalls atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"getLatestLedger"`) {
			tipCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"id":"ab","protocolVersion":22,"sequence":1000}}`)
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

// TestMinLedgerBySource_ExcludesOneShotJobNamespaces pins CA2-A19-correct-9:
// a finished one-shot job's shard rows (backfill, projected-rebuild,
// census-backfill, …) must not surface in the per-source verdict at
// all — their last_ledger is a historical range end, millions of
// ledgers behind tip on a perfectly healthy system, and including
// them turned a healthy detect-gaps run into a false LAGGING report.
func TestMinLedgerBySource_ExcludesOneShotJobNamespaces(t *testing.T) {
	cursors := []timescale.Cursor{
		{Source: "ledgerstream", Sub: "", LastLedger: 900_000},
		{Source: "projector", Sub: "soroswap", LastLedger: 899_500},
		{Source: "projector", Sub: "band", LastLedger: 899_800},
		// One-shot job shards — abandoned or long-finished, per the
		// finding's r1 census (4,523 projected-rebuild rows, 91
		// SDEX backfill rows from a 2026-05 attempt).
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

// TestArchivalNodeConfig_RPCEndpointsNotLoopback pins that the rendered
// stellar.rpc_endpoints never names a loopback host: the archival-node role
// deploys no stellar-rpc, so such an endpoint is dead and detect-gaps (the
// cursor-stuck / ingestion-lag runbook step) fails on every run.
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
