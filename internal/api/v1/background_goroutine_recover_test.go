// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// apiGoroutineGuards are the recovery markers a detached goroutine in this package tree may
// register.
//
//   - worker.Recover — the plain form, for a body with no cleanup of its own.
//   - worker.Report  — for a body that must also un-wedge something (clear a single-flight
//     marker, end a flight, mark a fold failed); it recovers itself and the counter still moves.
//   - recoverStreamProducer — the SSE producers' helper, which delegates to worker.Report.
//
// A bare recover() is deliberately not accepted: swallowing a panic without moving
// stellarindex_worker_panics_total turns a loud crash into a silent dead worker. [guardscan]
// enforces that: a deferred literal counts only when it calls recover() and one of these.
var apiGoroutineGuards = []string{"worker.Recover", "worker.Report", "recoverStreamProducer"}

// apiGoroutineFloor is a "did this test find anything?" floor, not an exact count: ordinary
// refactors that remove a goroutine pass, while a discovery walk that silently stops matching
// (the spawn idiom changes, the tree moves) fails.
const apiGoroutineFloor = 40

// TestAPIDetachedGoroutinesRecover: every detached goroutine under internal/api/v1, subpackages included, must register panic
// recovery, since an unrecovered panic kills the process and main.go's guard cannot see the API's refreshers, fillers and SSE
// producers. It is an AST guard so the next one added without the defer is caught; no exemption list.
func TestAPIDetachedGoroutinesRecover(t *testing.T) {
	var checked int
	var unguarded []string
	scanner := guardscan.NewScanner(guardscan.Config{Guards: apiGoroutineGuards})

	for _, path := range apiPackageGoFiles(t) {
		sites, err := scanner.ScanFile(path)
		if err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		for _, s := range sites {
			checked++
			if s.Kind == guardscan.KindUnresolved {
				t.Errorf("go %s at %s:%d could not be resolved to a function body (%s). "+
					"Wrap it in `go func(){ defer worker.Recover(logger, \"<name>\"); … }()` "+
					"so the guard is visible where the goroutine starts — a site this "+
					"scanner cannot read is a site nobody can prove is safe.",
					s.Target, path, s.Line, s.Reason)
				continue
			}
			if !s.Recovers {
				unguarded = append(unguarded, path+":"+itoa(s.Line)+" (go "+s.Target+", body at "+s.Origin+")")
			}
		}
	}

	sort.Strings(unguarded)
	for _, u := range unguarded {
		t.Errorf("detached goroutine at %s does not register panic recovery. "+
			"An unrecovered panic in a goroutine terminates the WHOLE process, "+
			"taking the API down over one degraded read. Add "+
			"`defer worker.Recover(logger, \"<stable-worker-name>\")` — or, if the "+
			"body owns a single-flight marker or a flight that must be released, a "+
			"deferred literal that recovers, calls worker.Report and then runs that "+
			"same release, so the entry is not left permanently wedged.", u)
	}

	if checked < apiGoroutineFloor {
		t.Errorf("only %d detached goroutine(s) discovered under internal/api/v1, expected "+
			"at least %d — the discovery in this test has drifted from the code and is "+
			"no longer protecting anything", checked, apiGoroutineFloor)
	}
}

// apiPackageGoFiles lists every non-test .go file in this package tree, in deterministic order.
// The walk is rooted at "." (a test's working directory is its package directory), so a new
// subpackage is covered the day it lands without being named.
func apiPackageGoFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk package tree: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("walked the package tree and found no .go files — the walk is broken")
	}
	sort.Strings(out)
	return out
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
