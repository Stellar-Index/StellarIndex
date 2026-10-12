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

// TestSessionsTableComment_DoesNotClaimUnimplementedAlert pins the corrected migration 0027 header on `sessions`: no "new login from a new country" alert exists, so the schema must not imply one.
// It is a `--` header comment, so the fix follows migrations/README.md#amending-a-shipped-migration, not a new migration.
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
	// Check only the comment block immediately above the CREATE TABLE, not the whole file, so a false claim elsewhere cannot hide
	// behind this check.
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
