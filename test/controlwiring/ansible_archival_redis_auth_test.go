package controlwiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── archival-node redis-server must require auth, and must
// finish restarting under that auth BEFORE the stellarindex app
// units are (re)started with STELLARINDEX_REDIS_PASSWORD ──────────
//
// Without this, 08-redis.yml only installs and starts the packaged
// redis-server — no requirepass, no ACL, no bind hardening — while
// this instance holds the SEP-10 replay guard, the rate-limit
// counters and the VWAP cache. Handlers flush in definition order
// (ansible's ordinary end-of-play behaviour), so simply adding a
// requirepass task later in the role without an explicit
// `flush_handlers` would restart redis-server AFTER
// 14-stellarindex-services.yml has already restarted
// api/indexer/aggregator against the OLD (unauthenticated) redis —
// an AUTH-mismatch outage on the first apply. This test pins both
// halves: the requirepass wiring itself, and the flush_handlers
// task ordered between it and the rest of the role.

const redisTasksPath = "configs/ansible/roles/archival-node/tasks/08-redis.yml"

func loadRedisTasks(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(redisTasksPath)))
	if err != nil {
		t.Fatalf("read %s: %v", redisTasksPath, err)
	}
	var tasks []map[string]any
	if err := yaml.Unmarshal(raw, &tasks); err != nil {
		t.Fatalf("parse %s: %v", redisTasksPath, err)
	}
	return tasks
}

func taskIndex(t *testing.T, tasks []map[string]any, name string) int {
	t.Helper()
	for i, task := range tasks {
		if n, _ := task["name"].(string); n == name {
			return i
		}
	}
	t.Fatalf("task %q not found in %s — re-derive this test", name, redisTasksPath)
	return -1
}

func TestArchivalRedis_RequiresPassword(t *testing.T) {
	tasks := loadRedisTasks(t)
	idx := taskIndex(t, tasks, "Require a password to talk to redis-server")
	mod, ok := tasks[idx]["ansible.builtin.lineinfile"].(map[string]any)
	if !ok {
		t.Fatalf("task %q is not an ansible.builtin.lineinfile task", "Require a password to talk to redis-server")
	}
	line, _ := mod["line"].(string)
	if !strings.Contains(line, "requirepass {{ redis_password }}") {
		t.Fatalf("requirepass task line = %q, want it to set requirepass from redis_password", line)
	}
	path, _ := mod["path"].(string)
	if path != "/etc/redis/redis.conf" {
		t.Fatalf("requirepass task targets %q, want /etc/redis/redis.conf", path)
	}
}

func TestArchivalRedis_AssertsPasswordSetBeforeConfiguring(t *testing.T) {
	tasks := loadRedisTasks(t)
	enableIdx := taskIndex(t, tasks, "Enable + start redis-server")
	assertIdx := taskIndex(t, tasks, "Assert redis_password is set")
	requirepassIdx := taskIndex(t, tasks, "Require a password to talk to redis-server")

	if !(enableIdx < assertIdx && assertIdx < requirepassIdx) {
		t.Fatalf("expected order [Enable + start redis-server(%d) < Assert redis_password is set(%d) "+
			"< Require a password to talk to redis-server(%d)]", enableIdx, assertIdx, requirepassIdx)
	}
}

// This is the ordering the verifier objection named directly: without
// an explicit flush here, the "Restart redis" handler fires at the
// role's normal end-of-play point — AFTER 14-stellarindex-services.yml
// has already restarted api/indexer/aggregator against the
// still-unauthenticated redis-server.
func TestArchivalRedis_FlushesHandlersBeforeAppServicesRestart(t *testing.T) {
	tasks := loadRedisTasks(t)
	requirepassIdx := taskIndex(t, tasks, "Require a password to talk to redis-server")

	flushIdx := -1
	for i, task := range tasks {
		if meta, _ := task["ansible.builtin.meta"].(string); meta == "flush_handlers" {
			flushIdx = i
			break
		}
	}
	if flushIdx == -1 {
		t.Fatalf("%s: no `ansible.builtin.meta: flush_handlers` task — redis-server's requirepass "+
			"restart is not guaranteed to happen before 14-stellarindex-services.yml restarts "+
			"api/indexer/aggregator with STELLARINDEX_REDIS_PASSWORD", redisTasksPath)
	}
	if flushIdx < requirepassIdx {
		t.Fatalf("flush_handlers task (index %d) must come AFTER the requirepass task (index %d)",
			flushIdx, requirepassIdx)
	}

	// Cross-file half of the ordering: 08-redis.yml must be imported
	// before 14-stellarindex-services.yml in tasks/main.yml, or the
	// flush inside 08-redis.yml doesn't help.
	mainPath := filepath.Join(repoRoot(t), "configs", "ansible", "roles", "archival-node", "tasks", "main.yml")
	raw, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}
	redisImport := strings.Index(string(raw), "08-redis.yml")
	servicesImport := strings.Index(string(raw), "14-stellarindex-services.yml")
	if redisImport == -1 || servicesImport == -1 {
		t.Fatalf("%s: could not locate both 08-redis.yml and 14-stellarindex-services.yml imports", mainPath)
	}
	if !(redisImport < servicesImport) {
		t.Fatalf("%s: 08-redis.yml must be imported before 14-stellarindex-services.yml", mainPath)
	}
}

func TestArchivalEnv_CarriesRedisPassword(t *testing.T) {
	path := filepath.Join(repoRoot(t), "configs", "ansible", "roles", "archival-node",
		"templates", "stellarindex.env.j2")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), "STELLARINDEX_REDIS_PASSWORD={{ redis_password }}") {
		t.Fatalf("%s: missing STELLARINDEX_REDIS_PASSWORD={{ redis_password }} — "+
			"internal/config/load.go reads STELLARINDEX_REDIS_PASSWORD to authenticate to redis, "+
			"and without this line the app units would never learn the requirepass set on redis-server", path)
	}
}

func TestArchivalPrometheusRedisExporter_CarriesPassword(t *testing.T) {
	path := filepath.Join(repoRoot(t), "configs", "ansible", "roles", "archival-node",
		"tasks", "16-prometheus-exporters.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), "REDIS_PASSWORD={{ redis_password }}") {
		t.Fatalf("%s: prometheus-redis-exporter's env-file has no REDIS_PASSWORD — "+
			"once redis-server requires auth this exporter scrapes NOAUTH on every attempt", path)
	}
}
