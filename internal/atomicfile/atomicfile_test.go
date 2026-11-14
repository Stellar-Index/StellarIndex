// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package atomicfile_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/atomicfile"
)

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// A symlink planted at the legacy fixed temp name must not be followed.
func TestWriteBytes_doesNotFollowPlantedTempSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m.prom")
	if err := os.Symlink(victim, path+".tmp"); err != nil {
		t.Fatal(err)
	}

	if err := atomicfile.WriteBytes(path, 0o644, []byte("new")); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("victim overwritten through planted symlink: %q", got)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("target = %q, want %q", got, "new")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", fi.Mode().Perm())
	}
}

// A symlink at the destination is replaced, not written through.
func TestWriteBytes_replacesSymlinkAtDestination(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m.prom")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.WriteBytes(path, 0o644, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("victim overwritten through destination symlink: %q", got)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("destination is still a symlink (err=%v)", err)
	}
}

func TestWrite_bodyErrorLeavesNoTempAndKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.prom")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := atomicfile.Write(path, 0o644, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Fatalf("target = %q, want the old content", got)
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Fatalf("temp file left behind: %v", names)
	}
}

func TestOpenLock_refusesSymlinkAndHardlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(dir, "sym.lock")
	if err := os.Symlink(victim, sym); err != nil {
		t.Fatal(err)
	}
	if f, err := atomicfile.OpenLock(sym, 0o600); err == nil {
		_ = f.Close()
		t.Fatal("OpenLock followed a symlink")
	}
	hard := filepath.Join(dir, "hard.lock")
	if err := os.Link(victim, hard); err != nil {
		t.Fatal(err)
	}
	if f, err := atomicfile.OpenLock(hard, 0o600); err == nil {
		_ = f.Close()
		t.Fatal("OpenLock accepted a hard link to another file")
	}
	f, err := atomicfile.OpenLock(filepath.Join(dir, "fresh.lock"), 0o600)
	if err != nil {
		t.Fatalf("fresh lock: %v", err)
	}
	_ = f.Close()
}
