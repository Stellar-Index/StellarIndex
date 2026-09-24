package archive

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestVerifyArchiveArchivist_PassesVerify pins Tier E's argv. A bare
// `stellar-archivist scan` only checks that files exist; --verify is what
// recomputes every bucket's sha256, the property Tier E is counted on for.
func TestVerifyArchiveArchivist_PassesVerify(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "stellar-archivist")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argvFile + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	const url = "file:///srv/history-archive"
	if err := verifyArchiveArchivist(bin, url, time.Minute); err != nil {
		t.Fatalf("verifyArchiveArchivist: %v", err)
	}
	b, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(b))
	want := []string{"scan", "--verify", url}
	if !slices.Equal(got, want) {
		t.Fatalf("archivist argv = %v, want %v", got, want)
	}
}

// tierFlagByADRLetter maps ADR-0016's tier letters to verify-archive's -tier
// values. A letter missing here is a tier verify-archive does not implement.
var tierFlagByADRLetter = map[string]string{
	"A": "chain",
	"B": "checkpoint",
	"D": "peers",
	"E": "archivist",
}

// TestVerifyArchiveTiers_ADRIntegrityLeaderTiersAreScheduled: ADR-0016 makes
// R1 the integrity leader on the strength of its PERIODIC tiers, and R2/R3
// delegate B + E to it. Every tier that sentence names must be invoked by a
// shipped unit or cron, or the guarantee is a claim nothing runs.
func TestVerifyArchiveTiers_ADRIntegrityLeaderTiersAreScheduled(t *testing.T) {
	t.Parallel()
	adr := readRepoFile(t, "docs/adr/0016-per-region-storage-strategy.md")
	m := regexp.MustCompile(`integrity leader\*: its periodic\s+Tier ((?:[A-E](?:\s*\+\s*)?)+)`).FindStringSubmatch(adr)
	if m == nil {
		t.Fatal("ADR-0016 no longer states R1's periodic tiers as `integrity leader*: its periodic Tier …`; re-derive this test")
	}
	letters := regexp.MustCompile(`[A-E]`).FindAllString(m[1], -1)
	if len(letters) == 0 {
		t.Fatalf("parsed no tier letters from %q", m[0])
	}

	scheduled := scheduledVerifyArchiveText(t)
	for _, letter := range letters {
		flag, ok := tierFlagByADRLetter[letter]
		if !ok {
			t.Errorf("ADR-0016 counts Tier %s for R1, which verify-archive has no -tier for", letter)
			continue
		}
		if !regexp.MustCompile(`-tier ` + flag + `(\s|\\|$)`).MatchString(scheduled) {
			t.Errorf("ADR-0016 counts Tier %s (-tier %s) in R1's periodic integrity guarantee, "+
				"but no unit template or ansible task in the archival-node role runs it", letter, flag)
		}
	}
}

// scheduledVerifyArchiveText is the non-comment text of every systemd unit
// template and task file in the archival-node role: where a scheduled
// verify-archive run can be defined.
func scheduledVerifyArchiveText(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, pattern := range []string{
		"configs/ansible/roles/archival-node/templates/systemd/*.j2",
		"configs/ansible/roles/archival-node/tasks/*.yml",
	} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatal("found no archival-node unit templates or task files")
	}
	var b strings.Builder
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(readRepoFile(t, rel), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
