// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package opsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every stellarindex-ops subcommand must take its shutdown context from
// SignalContext, the one place that restores the default action after the
// first signal; a hand-rolled signal.Notify/NotifyContext swallows the second.
func TestOpsSubcommandsUseSignalContext(t *testing.T) {
	root := filepath.Join("..")
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "opsutil" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if strings.Contains(string(src), "signal.Notify") {
			t.Errorf("%s: registers its own signal handler; use opsutil.SignalContext()", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files under %s; guard is not looking at internal/ops", scanned, root)
	}
}
