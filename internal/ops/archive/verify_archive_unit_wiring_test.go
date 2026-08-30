package archive

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── #282: the deployed unit must EXPORT the P1 counter ────────────
//
// `stellarindex_stellar_archive_divergence` (severity: page) selects
// `stellarindex_verify_archive_mismatches_total`. Declaring that
// counter in internal/obs is not enough to make the page fireable —
// the metric also needs an EXPORT PATH in the deployed topology, and
// it had none: the only exporter was the opt-in `-metrics-listen`
// HTTP endpoint, which neither verify-archive unit passed and which
// `configs/prometheus/prometheus.r1.yml` has no scrape job for. A
// real chain break therefore opened a P3 ticket
// (`stellarindex_verify_archive_unit_failed`) instead of paging.
//
// `scripts/ci/lint-metric-refs.sh` cannot catch this class: it asks
// "does anything in the repo emit this name?", and internal/obs
// always did. This test asks the question that actually matters —
// "does the unit that runs on r1 wire an export path?" — because
// neither `go vet` nor `promtool` can see across the Go ↔ systemd
// seam. Same reasoning as
// internal/ops/chops/clickhouse_serving_profile_test.go.
//
// Both trees are asserted: the ansible template is the authority for
// r1 (CLAUDE.md — r1 config is ansible-managed) and the
// deploy/systemd copy is the operator-facing reference. A fix in one
// and not the other is how they drift.

// verifyArchiveUnitFiles are the four unit definitions that run
// `stellarindex-ops verify-archive` on a scheduled timer.
var verifyArchiveUnitFiles = []struct {
	path string
	prom string // the .prom basename this unit must own, uniquely
}{
	{"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-a.service.j2", "verify_archive_tier_a.prom"},
	{"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-b.service.j2", "verify_archive_tier_b.prom"},
	{"deploy/systemd/verify-archive-tier-a.service", "verify_archive_tier_a.prom"},
	{"deploy/systemd/verify-archive-tier-b.service", "verify_archive_tier_b.prom"},
}

func TestVerifyArchiveUnits_ExportMismatchCounter(t *testing.T) {
	t.Parallel()
	for _, unit := range verifyArchiveUnitFiles {
		t.Run(filepath.Base(unit.path), func(t *testing.T) {
			t.Parallel()
			body := readRepoFile(t, unit.path)

			if !strings.Contains(body, "-textfile-output ${VERIFY_ARCHIVE_TEXTFILE}") {
				t.Errorf("%s: ExecStart does not pass -textfile-output — "+
					"stellarindex_verify_archive_mismatches_total has no export path, so the P1 "+
					"stellarindex_stellar_archive_divergence page cannot fire (#282)", unit.path)
			}

			want := "Environment=VERIFY_ARCHIVE_TEXTFILE=/var/lib/node_exporter/textfile_collector/" + unit.prom
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q — the path must land in node_exporter's "+
					"--collector.textfile.directory (10-observability.yml) and be unique per unit, "+
					"so tier-a and tier-b don't clobber each other's carry-forward", unit.path, want)
			}
		})
	}
}

// TestVerifyArchiveUnits_TierLabelIsUnique pins the other half of the
// duplicate-series hazard: node_exporter merges every .prom in its
// directory into ONE target, so two units exposing the same metric
// with the same label set make the whole textfile collector report a
// scrape error for the directory. The `tier` label the emitter writes
// comes from the unit's `-tier` flag, so tier-a and tier-b must
// differ in BOTH the file they write and the tier they label it with.
func TestVerifyArchiveUnits_TierLabelIsUnique(t *testing.T) {
	t.Parallel()
	pairs := map[string]string{} // "<prom>|<tier>" -> first unit that claimed it
	for _, unit := range verifyArchiveUnitFiles {
		tier := execStartTier(t, unit.path)
		pairs[unit.prom+"|"+tier] = unit.path
	}
	want := []string{
		"verify_archive_tier_a.prom|chain",
		"verify_archive_tier_b.prom|checkpoint",
	}
	if len(pairs) != len(want) {
		t.Fatalf("(.prom, tier) pairs = %v, want exactly %v — two units sharing a pair "+
			"expose an identical series through one node_exporter target", pairs, want)
	}
	for _, key := range want {
		if pairs[key] == "" {
			t.Errorf("no unit wires %q; got %v", key, pairs)
		}
	}
}

// execStartTier extracts the `-tier` value from a unit's ExecStart.
func execStartTier(t *testing.T, rel string) string {
	t.Helper()
	body := readRepoFile(t, rel)
	for _, tier := range []string{"chain", "checkpoint", "peers", "archivist", "all"} {
		if strings.Contains(body, "-tier "+tier+" \\\n") {
			return tier
		}
	}
	t.Fatalf("%s: no -tier flag found in ExecStart", rel)
	return ""
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	// internal/ops/archive -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	b, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// ─── #282 (repair): the DOCUMENTED APPLY must render both units ────
//
// Wiring `-textfile-output` into the unit templates only closes the
// P1's export path if the templates actually reach r1. The runbook
// prescribes `--tags ops-jobs` (deliberately: a full archival-node
// run can restart galexie, and a galexie restart is a ~9-minute
// mainnet cold catchup — the 2026-08-27 incident), and the tier-a
// install/enable tasks carry that tag. The tier-b block did not: it
// is a separate `when: verify_archive_tier_b_enabled` block added
// later, and it was never tagged. So the documented apply rendered
// tier-a's unit, skipped tier-b's, and the confirm step still read
// green off tier-a alone — a half-applied fix that reads healthy,
// with the CHECKPOINT tier (the cross-archive anchor the P1's own
// summary describes) left permanently unwired.
//
// Neither `go vet`, `promtool`, nor the jinja/ansible linters can
// see a missing tag; `ansible-playbook --list-tasks --tags ops-jobs`
// can, but ansible is not a build dependency of this repo. So the
// tag is pinned here, next to the ExecStart assertion it exists to
// deliver.

// opsJobsTaskFile is the role task file that installs and enables
// the verify-archive units.
const opsJobsTaskFile = "configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml"

// opsJobsTag is the tag docs/operations/runbooks/archive-divergence.md
// tells the operator to apply with.
const opsJobsTag = "ops-jobs"

func TestVerifyArchiveUnits_ReachableUnderOpsJobsTag(t *testing.T) {
	t.Parallel()

	var tasks []map[string]any
	if err := yaml.Unmarshal([]byte(readRepoFile(t, opsJobsTaskFile)), &tasks); err != nil {
		t.Fatalf("parse %s: %v", opsJobsTaskFile, err)
	}
	if len(tasks) == 0 {
		t.Fatalf("%s parsed to zero tasks", opsJobsTaskFile)
	}

	// Every top-level task or block that installs, enables or removes
	// a verify-archive tier-a/tier-b unit must be selected by
	// `--tags ops-jobs`. The trailing dot keeps the weekly Tier D
	// cron (`stellarindex-verify-archive-tier-d`, a root cron with no
	// unit file and no textfile export) out of scope.
	needles := []string{"verify-archive-tier-a.", "verify-archive-tier-b."}

	seen := 0
	for _, task := range tasks {
		body, err := yaml.Marshal(task)
		if err != nil {
			t.Fatalf("re-marshal task: %v", err)
		}
		touches := false
		for _, needle := range needles {
			if strings.Contains(string(body), needle) {
				touches = true
			}
		}
		if !touches {
			continue
		}
		seen++
		name, _ := task["name"].(string)
		if !slices.Contains(taskTags(t, task), opsJobsTag) {
			t.Errorf("task %q in %s has tags %v — it must carry %q, or the runbook's "+
				"documented `--tags ops-jobs` apply skips it and the P1 "+
				"stellarindex_stellar_archive_divergence stays dead for that tier (#282)",
				name, opsJobsTaskFile, taskTags(t, task), opsJobsTag)
		}
	}

	// Self-accounting: a needle that matches nothing would make every
	// assertion above vacuous.
	if seen < 3 {
		t.Fatalf("only %d verify-archive task(s)/block(s) matched in %s — expected the tier-a "+
			"install, the timer enable, and the tier-b install block at minimum; the match "+
			"is stale, so this test proves nothing", seen, opsJobsTaskFile)
	}
	t.Logf("checked %d verify-archive task(s)/block(s) in %s for the %q tag", seen, opsJobsTaskFile, opsJobsTag)
}

// taskTags normalises Ansible's two accepted `tags` spellings (a
// single string, or a list) into a slice.
func taskTags(t *testing.T, task map[string]any) []string {
	t.Helper()
	switch v := task["tags"].(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				t.Fatalf("non-string tag %v in task %v", item, task["name"])
			}
			out = append(out, s)
		}
		return out
	default:
		t.Fatalf("unexpected tags type %T in task %v", v, task["name"])
		return nil
	}
}
