// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package postgresstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleRoot walks up from the test's working directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find module root (go.mod) above test working directory")
	return ""
}

// TestSessionsTableComment_DoesNotClaimUnimplementedAlert — Q187
// (audit-2026-09-18). 0027's header comment on `sessions` claimed
// "Geo + IP fields drive the 'new login from a new country' email
// alert", but no such alert exists: internal/obs/metrics.go declares
// exactly two notify templates (magic-link, signup-verify), and
// neither dashboardauth.mintSession nor postgresstore.UserStore
// compares geo_first_seen/geo_last_seen or sends a notification on
// mismatch. A reader of the schema would believe a security control
// exists that does not. This pins the corrected comment, which is a
// `--` header comment (not executed, not stored in the database), so
// correcting it is the header-comment path in
// migrations/README.md#amending-a-shipped-migration, not a new
// migration.
func TestSessionsTableComment_DoesNotClaimUnimplementedAlert(t *testing.T) {
	root := moduleRoot(t)

	mig, err := os.ReadFile(filepath.Join(root, "migrations", "0027_platform_v1_schema.up.sql")) //nolint:gosec // repo-relative path resolved above
	if err != nil {
		t.Fatalf("read migration 0027: %v", err)
	}
	text := string(mig)

	const falseClaim = `new login from a new country`
	if strings.Contains(text, falseClaim) {
		t.Errorf("0027's sessions comment still claims a %q email alert exists; no such alert is implemented", falseClaim)
	}

	const idx = "sessions_user_active_idx"
	pos := strings.Index(text, idx)
	if pos == -1 {
		t.Fatal("migration 0027 no longer declares sessions_user_active_idx")
	}
	// The sessions table's header comment block precedes its CREATE TABLE,
	// which precedes this index. Look at the comment block immediately
	// above the CREATE TABLE, not the whole file, so a false claim
	// elsewhere in the file can't hide behind this check.
	createPos := strings.Index(text, "CREATE TABLE sessions")
	if createPos == -1 || createPos > pos {
		t.Fatal("migration 0027 sessions table structure not found where expected")
	}
	commentBlock := text[:createPos]
	commentStart := strings.LastIndex(commentBlock, "─── 3. sessions")
	if commentStart == -1 {
		t.Fatal("migration 0027 sessions section header not found")
	}
	sessionsComment := commentBlock[commentStart:]

	const wantPhrase = "no alert is fired"
	if !strings.Contains(sessionsComment, wantPhrase) {
		t.Errorf("sessions header comment = %q, want it to state that no alert is fired (matches actual behaviour)", sessionsComment)
	}
}
