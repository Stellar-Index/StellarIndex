// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// bareBackgroundCtx matches a subcommand's run context assigned straight from
// context.Background() — no signal, no deadline — as opposed to one wrapped
// in WithTimeout / WithCancel / NotifyContext / opsutil.SignalContext.
var bareBackgroundCtx = regexp.MustCompile(`^\s*\w*[cC]tx\w*\s*:?=\s*context\.Background\(\)\s*$`)

// TestOpsSubcommandsNeverRunOnABareBackgroundContext — an ops run context
// with neither signal handling nor a deadline cannot be cancelled on
// SIGTERM, so a wedged ClickHouse or archive read holds the process until it
// is killed with nothing flushed or closed. Every stellarindex-ops
// subcommand under internal/ops derives its context from
// opsutil.SignalContext or a bounded context instead.
func TestOpsSubcommandsNeverRunOnABareBackgroundContext(t *testing.T) {
	var hits []string
	scanned := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if bareBackgroundCtx.MatchString(line) {
				hits = append(hits, filepath.ToSlash(path)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned %d source files under internal/ops; the walk is not seeing the tree", scanned)
	}
	if len(hits) > 0 {
		t.Errorf("ops run context on a bare context.Background(); use opsutil.SignalContext or a bounded context:\n%s",
			strings.Join(hits, "\n"))
	}
}
