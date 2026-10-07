// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// TestArchiveCompletenessFix_DryRunByDefault pins that `fix` and
// `verify` registered neither -write nor -dry-run and fetched over HTTP,
// then os.Create/os.Rename/os.Chown'd into -archive-root unconditionally
// — the daily systemd timer runs `verify` unattended, with no preview and
// no confirmation, so a stale -archive-root default or a mis-templated
// mount got checkpoints written into the wrong tree with nobody looking.
//
// Without -write, `fix` must report the residual-missing exit (1 missing
// checkpoint in a 1-checkpoint range) WITHOUT constructing a filler or
// touching the filesystem — asserted here by the archive root staying
// completely empty, which also proves no HTTP fetch was attempted (this
// repo forbids tests that need network access).
func TestArchiveCompletenessFix_DryRunByDefault(t *testing.T) {
	root := t.TempDir()

	err := Run([]string{
		"archive-completeness", "fix",
		"-archive-root", root,
		"-from", "2", "-to", "63", // exactly one checkpoint position (seq=63)
	})
	if !errors.Is(err, opsutil.ErrExitSilently) {
		t.Fatalf("err = %v, want opsutil.ErrExitSilently (1 residual missing checkpoint)", err)
	}

	entries := treeEntries(t, root)
	if len(entries) != 0 {
		t.Errorf("archive root gained %d entries in dry-run mode (no -write): %v — the fill must not run without -write", len(entries), entries)
	}
}

// TestArchiveCompletenessVerify_DryRunByDefault is fix's sibling assertion
// for the mode the systemd timer actually fires.
func TestArchiveCompletenessVerify_DryRunByDefault(t *testing.T) {
	root := t.TempDir()

	err := Run([]string{
		"archive-completeness", "verify",
		"-archive-root", root,
		"-from", "2", "-to", "63",
	})
	if !errors.Is(err, opsutil.ErrExitSilently) {
		t.Fatalf("err = %v, want opsutil.ErrExitSilently (1 residual missing checkpoint)", err)
	}

	entries := treeEntries(t, root)
	if len(entries) != 0 {
		t.Errorf("archive root gained %d entries in dry-run mode (no -write): %v — the fill must not run without -write", len(entries), entries)
	}
}

// TestArchiveCompletenessFix_NothingMissingIgnoresWriteGate: an
// already-complete range must return cleanly regardless of -write —
// there is nothing to gate.
func TestArchiveCompletenessFix_NothingMissingIgnoresWriteGate(t *testing.T) {
	root := t.TempDir()
	// seq=63 -> ledger/00/00/00/ledger-0000003f.xdr.gz
	cpPath := filepath.Join(root, "ledger", "00", "00", "00", "ledger-0000003f.xdr.gz")
	if err := os.MkdirAll(filepath.Dir(cpPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cpPath, gzipBytes(t, "checkpoint fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Run([]string{
		"archive-completeness", "fix",
		"-archive-root", root,
		"-from", "2", "-to", "63",
	})
	if err != nil {
		t.Fatalf("err = %v, want nil — the range is already complete", err)
	}
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func treeEntries(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || d.IsDir() {
			return nil
		}
		got = append(got, path)
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return got
}
