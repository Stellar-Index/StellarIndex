package controlwiring

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── GH-1171: archival-node migrate step must report changed on a real
// schema change, not on every run ───────────────────────────────────
//
// cmd/stellarindex-migrate's cmdUp prints exactly one of two strings:
// "already at latest version — nothing to do" (no-op) or "migrated to
// version %d (dirty=%v)" (applied something). Neither contains "no
// change" — the sibling task in deploy-binary.yml hit this same defect
// and fixed its copy; this file's copy still tested for "no change",
// so changed_when was true on EVERY run and the weekly ansible-drift
// report could never tell a real schema change from noise (#496).
//
// This test extracts the REAL changed_when expression from the shipped
// task and evaluates it, through ansible-playbook, against the two
// literal strings cmdUp actually emits.

const archivalServicesTasks = "configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml"

const migrateTaskName = "Run pending migrations against the stellarindex database"

// migrateChangedWhen extracts the changed_when expression from the
// shipped migrate task. A rename must re-derive this test rather than
// silently pass on a task that no longer exists.
func migrateChangedWhen(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(archivalServicesTasks)))
	if err != nil {
		t.Fatalf("read %s: %v", archivalServicesTasks, err)
	}
	var tasks []map[string]any
	if err := yaml.Unmarshal(raw, &tasks); err != nil {
		t.Fatalf("parse %s: %v", archivalServicesTasks, err)
	}
	for _, task := range tasks {
		if name, _ := task["name"].(string); name != migrateTaskName {
			continue
		}
		expr, ok := task["changed_when"].(string)
		if !ok || expr == "" {
			t.Fatalf("task %q has no changed_when", migrateTaskName)
		}
		return expr
	}
	t.Fatalf("task %q not found in %s — re-derive this test", migrateTaskName, archivalServicesTasks)
	return ""
}

// runChangedWhen evaluates expr as a real ansible changed_when against
// a task whose registered migrate_result.stdout is stdout, and reports
// whether the task was marked changed.
func runChangedWhen(t *testing.T, bin, expr, stdout string) bool {
	t.Helper()
	dir := t.TempDir()

	play := []map[string]any{{
		"hosts":        "localhost",
		"connection":   "local",
		"gather_facts": false,
		"vars": map[string]any{
			"migrate_result": map[string]any{"stdout": stdout},
		},
		"tasks": []map[string]any{{
			"name":         "probe",
			"debug":        map[string]any{"msg": "probe"},
			"changed_when": expr,
		}},
	}}
	buf, err := yaml.Marshal(play)
	if err != nil {
		t.Fatalf("marshal playbook: %v", err)
	}
	pb := filepath.Join(dir, "probe.yml")
	if err := os.WriteFile(pb, buf, 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	cmd := exec.Command(bin, "-i", "localhost,", pb)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False",
		"ANSIBLE_DEPRECATION_WARNINGS=False",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-playbook failed for stdout %q: %v\n%s", stdout, err, out)
	}
	return recapShowsChanged(string(out))
}

func recapShowsChanged(out string) bool {
	// PLAY RECAP line: "localhost : ok=1 changed=1 unreachable=0 ..."
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "ok=") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if field == "changed=1" {
				return true
			}
			if field == "changed=0" {
				return false
			}
		}
	}
	return false
}

func TestGH1171_ArchivalMigrateChangedWhenMatchesRealCmdUpOutput(t *testing.T) {
	t.Parallel()

	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skipf("ansible-playbook not on PATH: %v", err)
	}

	expr := migrateChangedWhen(t)

	cases := []struct {
		name        string
		stdout      string // literal cmdUp output, cmd/stellarindex-migrate/main.go
		wantChanged bool
	}{
		{
			name:        "no-op",
			stdout:      "already at latest version — nothing to do",
			wantChanged: false,
		},
		{
			name:        "applied",
			stdout:      "migrated to version 42 (dirty=false)",
			wantChanged: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := runChangedWhen(t, bin, expr, tc.stdout)
			if got != tc.wantChanged {
				t.Errorf("changed_when %q against stdout %q: got changed=%v, want %v",
					expr, tc.stdout, got, tc.wantChanged)
			}
		})
	}
}
