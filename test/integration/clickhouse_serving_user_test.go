//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	servingRoleTask     = "configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml"
	servingRoleTaskName = "Install the api_serving ClickHouse profile + user (ADR-0048 D4)"
	servingITUser       = "api_serving_it"
	servingITPassword   = "not-a-real-serving-password" // gitleaks:allow
	servingITUsersFile  = "/etc/clickhouse-server/users.d/api-serving-it.xml"
	// A partition no lake test writes; its sentinel is the newest, so the movements plan is usable.
	servingITPart = 4290
)

// The API reads ClickHouse as the role's api_serving user; every serving read must run under exactly the privileges that
// drop-in grants. The counts-rollup plan reads system.parts, which allow_databases=stellar alone refused with code 497, so
// every account read silently fell back to the source scan.
func TestServingUserRunsCountsRollupPlanRead(t *testing.T) {
	ctx := context.Background()
	addr := clickhouseAddr(t)
	admin := openCHDefault(ctx, t, addr)
	installServingUser(ctx, t, admin)

	if err := admin.Exec(ctx, `INSERT INTO stellar.account_movement_counts (part, account, movement_kind, n, through)
		VALUES (?, '', '', 0, ?)`, uint32(servingITPart), uint32(servingITPart*1_000_000+1)); err != nil {
		t.Fatalf("seed movements sentinel: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec(context.WithoutCancel(ctx), `ALTER TABLE stellar.account_movement_counts DROP PARTITION `+strconv.Itoa(servingITPart))
	})

	serving := openCHAs(ctx, t, addr, servingITUser, servingITPassword)
	var n uint64
	if err := serving.QueryRow(ctx, `SELECT count() FROM system.parts WHERE database = 'stellar'`).Scan(&n); err != nil {
		t.Fatalf("serving user cannot read system.parts (the counts-rollup plan read): %v", err)
	}
	// The serving path's other non-table reads: the TTL-liveness probe and a per-query SETTINGS clause.
	for _, q := range []string{
		`EXISTS TABLE stellar.ttl_live_until`,
		`SELECT count() FROM stellar.ledgers SETTINGS max_threads = 4, max_execution_time = 30`,
	} {
		if err := serving.Exec(ctx, q); err != nil {
			t.Fatalf("serving user cannot run %q: %v", q, err)
		}
	}

	r, err := chstore.OpenExplorerReaderAuth(addr, servingITUser, servingITPassword)
	if err != nil {
		t.Fatalf("open serving explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err := r.AccountMovements(ctx, "GSERVINGUSERIT", 10, chstore.AccountMovementCursor{}, chstore.AccountMovementFilter{}); err != nil {
		t.Fatalf("AccountMovements as the serving user: %v", err)
	}
	if got := testutil.ToFloat64(obs.CountsRollupPlanOK.WithLabelValues(chstore.MovementCountsRollup.Name)); got != 1 {
		t.Fatalf("stellarindex_counts_rollup_plan_ok{rollup=movements} = %v as the serving user, want 1", got)
	}

	assertServingUserDenied(ctx, t, serving)
}

// assertServingUserDenied pins the other half of least privilege: no other system table, no SYSTEM command, no write.
func assertServingUserDenied(ctx context.Context, t *testing.T, serving driver.Conn) {
	t.Helper()
	for _, q := range []string{
		`SELECT count() FROM system.merges`,
		`SELECT count() FROM system.processes`,
		`SYSTEM DROP DNS CACHE`,
		`KILL QUERY WHERE query_id = 'none' SYNC`,
		`INSERT INTO stellar.account_movement_counts (part, account, movement_kind, n, through) VALUES (0, 'x', 'x', 1, 1)`,
		`CREATE TABLE stellar.serving_it_forbidden (x UInt8) ENGINE = Memory`,
	} {
		// 497 (ACCESS_DENIED), not readonly's 164: the grants refuse it, whatever the profile says.
		if err := serving.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "code: 497") {
			t.Errorf("serving user ran %q: err = %v; want ACCESS_DENIED (497)", q, err)
		}
	}
}

// installServingUser renders the role's api_serving users.d drop-in into the shared container and waits for the reload. Only
// the Jinja values and the loopback-only network ACL change: the test dials through the mapped port, not from loopback.
func installServingUser(ctx context.Context, t *testing.T, admin driver.Conn) {
	t.Helper()
	xml := renderServingUsersXML(t)
	if err := chContainer.CopyToContainer(ctx, []byte(xml), servingITUsersFile, 0o644); err != nil {
		t.Fatalf("copy serving users.d into container: %v", err)
	}
	t.Cleanup(func() {
		_, _, _ = chContainer.Exec(context.WithoutCancel(ctx), []string{"rm", "-f", servingITUsersFile})
		_ = admin.Exec(context.WithoutCancel(ctx), "SYSTEM RELOAD CONFIG")
	})
	if err := admin.Exec(ctx, "SYSTEM RELOAD CONFIG"); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n uint64
		if err := admin.QueryRow(ctx, `SELECT count() FROM system.users WHERE name = ?`, servingITUser).Scan(&n); err == nil && n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared in system.users: ClickHouse rejected the role's drop-in (see the server log)", servingITUser)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

var servingJinja = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*(\|[^}]*)?\}\}`)

// renderServingUsersXML extracts the api_serving drop-in from the role task and fills its Jinja expressions.
func renderServingUsersXML(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", servingRoleTask))
	if err != nil {
		t.Fatal(err)
	}
	var tasks []map[string]any
	if err := yaml.Unmarshal(raw, &tasks); err != nil {
		t.Fatalf("parse %s: %v", servingRoleTask, err)
	}
	var content string
	for _, task := range tasks {
		if task["name"] == servingRoleTaskName {
			cp, _ := task["ansible.builtin.copy"].(map[string]any)
			content, _ = cp["content"].(string)
		}
	}
	if content == "" {
		t.Fatalf("%s: task %q with a copy content not found", servingRoleTask, servingRoleTaskName)
	}
	sum := sha256.Sum256([]byte(servingITPassword))
	vars := map[string]string{
		"clickhouse_serving_user":                   servingITUser,
		"clickhouse_serving_password":               hex.EncodeToString(sum[:]),
		"clickhouse_serving_max_threads":            "2",
		"clickhouse_serving_max_memory_usage_bytes": "2147483648",
		"clickhouse_serving_max_execution_time_s":   "10",
		"clickhouse_serving_max_concurrent_queries": "20",
	}
	out := servingJinja.ReplaceAllStringFunc(content, func(m string) string {
		sub := servingJinja.FindStringSubmatch(m)
		v, ok := vars[sub[1]]
		if !ok {
			t.Fatalf("%s: unknown Jinja variable %q in the api_serving drop-in", servingRoleTask, sub[1])
		}
		if sub[1] == "clickhouse_serving_password" && !strings.Contains(sub[2], "hash('sha256')") {
			t.Fatalf("%s: the serving password is no longer rendered as its sha256 digest", servingRoleTask)
		}
		return v
	})
	for _, lo := range []string{"<ip>127.0.0.1</ip>", "<ip>::1</ip>"} {
		if !strings.Contains(out, lo) {
			t.Fatalf("%s: api_serving no longer pins %s; update this test's network override", servingRoleTask, lo)
		}
		out = strings.Replace(out, lo, "<ip>::/0</ip>", 1)
	}
	return out
}

func openCHDefault(ctx context.Context, t *testing.T, addr string) driver.Conn {
	t.Helper()
	return openCHAs(ctx, t, addr, "default", "")
}

// openCHAs dials database stellar, as the API's readers do (ops_auth.go).
func openCHAs(ctx context.Context, t *testing.T, addr, user, password string) driver.Conn {
	t.Helper()
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        clickhouse.Auth{Database: "stellar", Username: user, Password: password},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("open clickhouse as %s: %v", user, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping clickhouse as %s: %v", user, err)
	}
	return conn
}
