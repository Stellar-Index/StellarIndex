package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// GH-518: the curated-rwa-sync units twice kept a retired design's
// Description=/Documentation= after their header prose was rewritten,
// and the timer twin was never edited at all. An operator sent here by
// the alert runs `systemctl cat` and follows Documentation= to the
// migration for a table this sync does not write.

const systemdTemplateDir = "configs/ansible/roles/archival-node/templates/systemd"

var repoBlobDocRE = regexp.MustCompile(`^Documentation=https://github\.com/Stellar-Index/StellarIndex/blob/main/(\S+)$`)

// unitDirectives returns every value of key= in a unit template's body.
func unitDirectives(body, key string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

// The table the sync writes is the one its store INSERTs into; each
// unit's Documentation= must name the migration that creates it.
func TestCuratedRWASyncUnits_DocumentTheTableTheSyncWrites(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	store := readRepoFile(t, "internal/storage/timescale/rwa_curated_published.go")
	m := regexp.MustCompile(`INSERT INTO (\w+)`).FindStringSubmatch(store)
	if m == nil {
		t.Fatal("rwa_curated_published.go: no INSERT INTO — the sync's write target moved; re-point this test")
	}
	table := m[1]
	createRE := regexp.MustCompile(`(?i)CREATE TABLE (IF NOT EXISTS )?` + regexp.QuoteMeta(table) + `\b`)

	for _, name := range []string{"curated-rwa-sync.service.j2", "curated-rwa-sync.timer.j2"} {
		body := readRepoFile(t, filepath.Join(systemdTemplateDir, name))
		docs := unitDirectives(body, "Documentation")
		if len(docs) == 0 {
			t.Errorf("%s: no Documentation= directive", name)
		}
		for _, d := range docs {
			sub := repoBlobDocRE.FindStringSubmatch("Documentation=" + d)
			if sub == nil || !strings.HasPrefix(sub[1], "migrations/") {
				continue
			}
			sql, err := os.ReadFile(filepath.Join(root, sub[1]))
			if err != nil {
				t.Errorf("%s: Documentation= names %s, which does not exist: %v", name, sub[1], err)
				continue
			}
			if !createRE.Match(sql) {
				t.Errorf("%s: Documentation= names %s, which does not create %s — the table the sync writes",
					name, sub[1], table)
			}
		}
		for _, d := range unitDirectives(body, "Description") {
			if strings.Contains(strings.ToLower(d), "directory") {
				t.Errorf("%s: Description=%q names the retired per-asset directory design; the sync caches %s",
					name, d, table)
			}
		}
	}
}

// A timer is read beside the service it fires: every repo document the
// timer points at must be one its service points at too, so a rewrite
// of one twin that misses the other fails here.
func TestSystemdTimerDocumentationIsASubsetOfItsService(t *testing.T) {
	t.Parallel()
	timers, err := filepath.Glob(filepath.Join(repoRoot(t), systemdTemplateDir, "*.timer.j2"))
	if err != nil || len(timers) == 0 {
		t.Fatalf("no timer templates under %s (err=%v)", systemdTemplateDir, err)
	}
	for _, timer := range timers {
		svc := strings.TrimSuffix(timer, ".timer.j2") + ".service.j2"
		svcBody, err := os.ReadFile(svc)
		if err != nil {
			continue // a timer that fires a differently-named unit
		}
		timerBody, err := os.ReadFile(timer)
		if err != nil {
			t.Fatalf("read %s: %v", timer, err)
		}
		have := map[string]bool{}
		for _, d := range unitDirectives(string(svcBody), "Documentation") {
			have[d] = true
		}
		for _, d := range unitDirectives(string(timerBody), "Documentation") {
			if !have[d] {
				t.Errorf("%s: Documentation=%s is not on %s — the twins disagree about where the operator should read",
					filepath.Base(timer), d, filepath.Base(svc))
			}
		}
	}
}
