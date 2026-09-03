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

// apiGoroutineGuards are the recovery markers a detached goroutine in this
// package tree may register.
//
//   - worker.Recover — the plain form, for a body with no cleanup of its own.
//   - worker.Report  — for a body that must ALSO un-wedge something (clear a
//     single-flight marker, end a flight, mark a fold failed) before it
//     returns; it recovers itself and hands the value here so the same
//     counter still moves.
//   - recoverStreamProducer — the SSE producers' shared helper, which
//     delegates to worker.Report with the stream name as the worker label.
//
// A bare recover() is deliberately NOT accepted: swallowing a panic without
// moving stellarindex_worker_panics_total converts a loud crash into a
// silent dead worker, which is the failure this guard exists to prevent.
// [guardscan] enforces that — a deferred literal counts only when it calls
// recover() AND one of these.
var apiGoroutineGuards = []string{"worker.Recover", "worker.Report", "recoverStreamProducer"}

// apiGoroutineFloor is a "did this test find anything?" floor, not an exact
// count. 45 detached goroutines existed under internal/api/v1 when this
// guard was written; the floor sits just below so ordinary refactors that
// remove one do not fail the build, while a discovery walk that silently
// stops matching (the spawn idiom changes, the tree moves) does.
const apiGoroutineFloor = 40

// TestAPIDetachedGoroutinesRecover is the package-tree twin of each
// binary's main.go guard test: EVERY detached goroutine started anywhere
// under internal/api/v1 — this package and every subpackage — must register
// panic recovery.
//
// Why it has to exist separately from the binaries' guards. An unrecovered
// panic in ANY goroutine terminates the whole Go process; it is not confined
// to the goroutine that panicked. cmd/stellarindex-api's guard covers the
// workers main() starts, but the API's real fleet of detached goroutines is
// started HERE — the stale-while-revalidate cache refreshers, the snapshot
// fillers, the per-row fan-outs, the SSE producers — and no main.go-scoped
// walk can see any of them. At the time this was written 37 of the 45 sites
// in this tree recovered nothing at all, so a nil map, a bad type assertion
// or an index into an empty slice from a degraded read took the entire API
// down along with every healthy request in flight (#368 M1 residual).
//
// Why an AST guard rather than a behavioural test: the failure mode that
// bites is the NEXT refresher someone adds without the defer. A behavioural
// test would have to drive each existing goroutine to panic through a real
// dependency and would still only cover the ones that exist today. So this
// derives the goroutine set from the source itself and fails if any of them
// lacks recovery — find every call site of the thing being guarded, not a
// sample. Same discipline as cmd/stellarindex-api's
// TestBackgroundWorkersRecover and this package's
// TestSSEProducerGoroutinesRecover, which it subsumes but does not replace
// (that one additionally pins the producers by NAME).
//
// There is no exemption list. main.go's guard exempts the HTTP listener
// because a dead listener means a live process serving nothing; nothing in
// this tree has that property — every goroutine here is a refresher, a
// filler or a fan-out whose death should degrade one response, not the
// process.
//
// Proven red: adding an unguarded `go func(){}()` anywhere in the tree, or
// deleting any one of the guards this fixed, fails this test naming the
// file and line.
func TestAPIDetachedGoroutinesRecover(t *testing.T) {
	var checked int
	var unguarded []string

	for _, path := range apiPackageGoFiles(t) {
		sites, err := guardscan.ScanFile(path, guardscan.Config{Guards: apiGoroutineGuards})
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

// apiPackageGoFiles lists every non-test .go file in this package tree, in
// deterministic order. The walk is rooted at "." — a test's working
// directory is its own package directory — so it covers this package and
// every subpackage (explorer, dashboardauth, middleware, …) without naming
// them, which is the point: a new subpackage is covered the day it lands.
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
