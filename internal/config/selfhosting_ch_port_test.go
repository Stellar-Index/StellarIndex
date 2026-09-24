package config_test

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
)

// TestSelfHostingGuide_ClickHouseNativePort pins the one port three places must
// agree on: storage.clickhouse_addr's default, the ansible role's
// clickhouse_tcp_port, and the guide's instruction to move clickhouse-server
// there. ClickHouse's own default native port is 9000, which the guide's
// MinIO (§4.4) already holds, so a guide that only names the dial address
// leaves the indexer dialling a port nothing listens on.
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
