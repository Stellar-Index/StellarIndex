package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The packaged redis/postgres exporters take their bind from ARGS in an
// env-file; nothing else observes whether it applied, so an ineffective
// bind leaves the exporter on every interface while every run stays green.
// These tests pin a post-restart socket check for each loopback ARGS bind.

const exportersTasksPath = "configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml"

var loopbackArgsPortRE = regexp.MustCompile(`ARGS="--web\.listen-address=127\.0\.0\.1:(\d+)"`)

func loadExporterTasks(t *testing.T) (string, []map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(exportersTasksPath)))
	if err != nil {
		t.Fatalf("read %s: %v", exportersTasksPath, err)
	}
	var tasks []map[string]any
	if err := yaml.Unmarshal(raw, &tasks); err != nil {
		t.Fatalf("parse %s: %v", exportersTasksPath, err)
	}
	return string(raw), tasks
}

func exporterTaskIndex(t *testing.T, tasks []map[string]any, name string) int {
	t.Helper()
	for i, task := range tasks {
		if n, _ := task["name"].(string); n == name {
			return i
		}
	}
	t.Fatalf("task %q not found in %s — re-derive this test", name, exportersTasksPath)
	return -1
}

func TestArchivalExporters_BindPostConditionCoversEveryLoopbackArgs(t *testing.T) {
	raw, tasks := loadExporterTasks(t)

	matches := loopbackArgsPortRE.FindAllStringSubmatch(raw, -1)
	if len(matches) < 2 {
		t.Fatalf("%s: found %d loopback ARGS binds, want the redis (9121) and postgres (9187) exporters",
			exportersTasksPath, len(matches))
	}

	readIdx := exporterTaskIndex(t, tasks, "Read the exporters' listening sockets")
	loop, _ := tasks[readIdx]["loop"].(string)
	for _, m := range matches {
		if !strings.Contains(loop, m[1]) {
			t.Errorf("socket check loop %q does not cover port %s — its ARGS bind is never verified", loop, m[1])
		}
	}
	cmd, _ := tasks[readIdx]["ansible.builtin.command"].(map[string]any)
	argv, _ := cmd["argv"].([]any)
	if len(argv) == 0 || argv[0] != "ss" {
		t.Errorf("socket check argv = %v, want an `ss` listener query", argv)
	}

	assertIdx := exporterTaskIndex(t, tasks, "Assert the exporters listen on loopback only")
	mod, _ := tasks[assertIdx]["ansible.builtin.assert"].(map[string]any)
	that, _ := mod["that"].([]any)
	var cond string
	if len(that) > 0 {
		cond, _ = that[0].(string)
	}
	if !strings.Contains(cond, "127[.]0[.]0[.]1") {
		t.Errorf("loopback assert `that` = %v, want it to reject any listener not on 127.0.0.1", that)
	}
	if assertIdx < readIdx {
		t.Errorf("assert (index %d) runs before the socket read (index %d)", assertIdx, readIdx)
	}
}

// Env-file edits reach the exporters only through the end-of-play restart
// handlers; without a flush the check reads the pre-change process.
func TestArchivalExporters_FlushesRestartsBeforeBindCheck(t *testing.T) {
	_, tasks := loadExporterTasks(t)
	redisStart := exporterTaskIndex(t, tasks, "Enable + start prometheus-redis-exporter")
	groupB := exporterTaskIndex(t, tasks, "Group B — postgres_exporter (requires Postgres)")
	readIdx := exporterTaskIndex(t, tasks, "Read the exporters' listening sockets")

	flushIdx := -1
	for i := max(redisStart, groupB) + 1; i < readIdx; i++ {
		if meta, _ := tasks[i]["ansible.builtin.meta"].(string); meta == "flush_handlers" {
			flushIdx = i
		}
	}
	if flushIdx == -1 {
		t.Fatalf("%s: no `ansible.builtin.meta: flush_handlers` between the exporter start tasks "+
			"and the socket check — a changed ARGS is checked before its restart", exportersTasksPath)
	}
}
