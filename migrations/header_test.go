package migrations

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestMigration0150HeaderNamesPopulatedNodeApply guards 0150's header
// against dropping the operator step for its bare in-transaction
// trades_signer_idx build: the up body is immutable (README "Amending a
// shipped migration"), so the header and register row are the only place
// a populated-node operator learns that a pre-build fails the migration (T444).
func TestMigration0150HeaderNamesPopulatedNodeApply(t *testing.T) {
	header := upHeader(t, "0150_add_trades_signer.up.sql")
	for _, want := range []string{
		"APPLYING TO AN ALREADY-POPULATED DATABASE",
		"trades_signer_idx",
		"no `IF NOT EXISTS`",
		"Do NOT pre-build",
		"CONCURRENTLY",
		"transaction_per_chunk",
		"pg_stat_activity",
		"ingest writers stopped",
	} {
		if !strings.Contains(header, want) {
			t.Errorf("0150 up header missing %q — the populated-node apply note for trades_signer_idx is incomplete", want)
		}
	}
	row := extractRow(t, readReadme(t), "0150")
	for _, want := range []string{"Do NOT pre-build", "ingest writers stopped", "pg_stat_activity"} {
		if !strings.Contains(row, want) {
			t.Errorf("README 0150 register row missing %q — it must agree with the file header", want)
		}
	}
}

// numberedHeaderRe matches the "-- NNNN up" / "-- NNNN down" first-line
// convention used by most migrations (some older files use other
// prose and are skipped, not a target of this check).
var numberedHeaderRe = regexp.MustCompile(`^-- (\d{4}) (up|down)\b`)

// TestMigrationHeaderNumberMatchesFilename catches a copy-paste header
// left over from the previous migration in the sequence (T417): the
// leading "-- NNNN up"/"-- NNNN down" comment must name the same number
// as the file it lives in, for every migration using that convention.
// A stale number silently misdescribes the file to anyone reading the
// migration in isolation, and .up.sql/.down.sql are edited independently
// so one twin can be fixed while the other is missed.
func TestMigrationHeaderNumberMatchesFilename(t *testing.T) {
	files, err := filepath.Glob("*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	checked := 0
	for _, path := range files {
		base := filepath.Base(path)
		fileNum := base[:4]
		if _, err := strconv.Atoi(fileNum); err != nil {
			continue // not a NNNN_name.{up,down}.sql file
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		firstLine := strings.SplitN(string(raw), "\n", 2)[0]
		m := numberedHeaderRe.FindStringSubmatch(firstLine)
		if m == nil {
			continue // header doesn't use the numbered convention
		}
		checked++
		if m[1] != fileNum {
			t.Errorf("%s: header reads %q but the filename number is %s", base, firstLine, fileNum)
		}
		wantDir := "up"
		if strings.HasSuffix(base, ".down.sql") {
			wantDir = "down"
		}
		if m[2] != wantDir {
			t.Errorf("%s: header says %q but filename is a .%s.sql", base, firstLine, wantDir)
		}
	}
	if checked == 0 {
		t.Fatal("no numbered-convention headers found; the check would pass vacuously")
	}
	t.Logf("checked %d migration headers using the numbered convention", checked)
}

// upHeader returns the comment block above a migration's first `BEGIN;`.
func upHeader(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	idx := strings.Index(string(raw), "\nBEGIN;")
	if idx == -1 {
		t.Fatalf("%s has no BEGIN; line — update this test's anchor", name)
	}
	return string(raw[:idx])
}
