package main

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// K012 — every goroutine in the stellarindex-api process recovers.
//
// An unrecovered panic in ANY goroutine terminates the whole Go process; it
// is not confined to the goroutine that panicked. #368 closed that hole in
// two places and guarded each with an AST walk:
//
//   - cmd/*/main.go, via each binary's TestBackgroundWorkersRecover
//     (guardscan.ScanFile("main.go", …) — ONE file); and
//   - internal/api/v1 and its subpackages, via
//     TestAPIDetachedGoroutinesRecover, whose walk is rooted at "." and so
//     reaches exactly its own package tree.
//
// Both are narrow BY CONSTRUCTION, and a PASS over a narrow slice reads
// identical to a PASS over everything: thirteen goroutines elsewhere in the
// linked binary — the explorer's ClickHouse-side stale-while-revalidate
// refreshers, the lake and discovery sinks, the Chainlink poller's fan-out,
// the divergence reference fan-out and the SDEX bulk writer's two
// WaitGroup-joined pools — recovered nothing at all. So this walk derives
// its package set from the LINKER's answer (`go list -deps .`) rather than
// from a tree root someone chose, and a package newly linked into the API is
// covered the day it lands rather than the day someone remembers to widen a
// root. It subsumes the two narrower walks without replacing them.
//
// It does not merely require that a panic be CONTAINED. Containment without
// release is worse than the crash it replaces — the crash at least ends with
// a fresh process — so each of those sites pairs its recovery with the
// release it owns, and each has its own behavioural test next to the code:
//
//   - a flight or single-flight marker is ended from a DEFER, or the waiters
//     block on a channel nobody closes and the cache hands out the same dead
//     flight for the life of the process
//     (internal/storage/clickhouse/detached_refresh_panic_test.go);
//   - a drain worker keeps close(done) as its outermost defer, or Stop()
//     never returns;
//   - a fan-out member records its failure in the slot its joiner reads, or
//     a panicked partition reads as one that landed
//     (internal/storage/timescale/trades_bulk_panic_guard_test.go).
//
// The guards below are the shared internal/worker helpers, so every recovered
// panic moves stellarindex_worker_panics_total — the series
// stellarindex_worker_panicked pages on. A bare recover() is deliberately NOT
// accepted: swallowing a panic without moving that counter turns a loud crash
// into a silent dead worker, which is the failure this guard exists to
// prevent.
//
// The HTTP listener in this binary is the one deliberate exemption and is
// excluded BY ITS CONTENT, on the same argument main.go's own guard makes:
// recovering the accept loop leaves a live process serving nothing. If
// another site should be fatal, argue it here in content the same way — do
// not add a name to a list.
func TestK012_EveryGoroutineInTheAPIProcessRecovers(t *testing.T) {
	// The guards any body in this process may register. A bare recover()
	// is deliberately not among them: swallowing a panic without moving
	// stellarindex_worker_panics_total turns a loud crash into a silent
	// dead worker, which is the failure the guard exists to prevent.
	guards := []string{
		"worker.Recover", "worker.Report",
		"recoverBackgroundWorker", "recoverStreamProducer",
	}

	var checked, exemptListener int
	scanner := guardscan.NewScanner(guardscan.Config{Guards: guards})
	var unguarded []string

	for _, path := range apiProcessGoFiles(t) {
		sites, err := scanner.ScanFile(path)
		if err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		for _, s := range sites {
			if s.Calls("ListenAndServe") || s.Calls("Serve") {
				exemptListener++
				continue
			}
			checked++
			switch {
			case s.Kind == guardscan.KindUnresolved:
				unguarded = append(unguarded,
					path+":"+itoaK012(s.Line)+" (go "+s.Target+" — UNRESOLVED: "+s.Reason+")")
			case !s.Recovers:
				unguarded = append(unguarded,
					path+":"+itoaK012(s.Line)+" (go "+s.Target+", body at "+s.Origin+")")
			}
		}
	}

	sort.Strings(unguarded)
	for _, u := range unguarded {
		t.Errorf("goroutine at %s registers no panic recovery. An unrecovered panic in "+
			"ANY goroutine terminates the WHOLE stellarindex-api process, taking every "+
			"healthy request in flight down with one degraded background read. Add "+
			"`defer worker.Recover(logger, \"<stable-worker-name>\")` — or, where the body "+
			"owns a single-flight marker, a flight or a WaitGroup, a deferred literal that "+
			"recovers, calls worker.Report and then performs that same release, so "+
			"containing the panic does not wedge the cache instead.", u)
	}

	// Floors, so a walk that silently stops matching cannot pass as a
	// clean bill of health. 91 sites across 76 in-module packages at the
	// time of writing; one listener.
	if checked < 80 {
		t.Errorf("only %d goroutine site(s) discovered across the API binary's in-module "+
			"packages, expected at least 80 — the discovery has drifted and this test is "+
			"no longer protecting anything", checked)
	}
	if exemptListener != 1 {
		t.Errorf("found %d listener goroutine(s), want exactly 1 — if the listener was "+
			"restructured, re-derive whether the crash-by-design argument still holds "+
			"before widening this exemption", exemptListener)
	}
}

// apiProcessGoFiles lists every non-test .go file in every package of THIS
// module that the stellarindex-api binary links, in deterministic order.
//
// The set comes from `go list -deps .` rather than a hand-picked tree root:
// the question this test asks is "what can panic inside this process", and
// only the linker's dependency closure answers it. Packages outside the
// module are excluded — a third-party goroutine is not ours to guard.
func apiProcessGoFiles(t *testing.T) []string {
	t.Helper()
	const modPath = "github.com/Stellar-Index/StellarIndex"
	cmd := exec.Command("go", "list", "-deps",
		"-f", "{{if .Module}}{{if eq .Module.Path \""+modPath+"\"}}{{.Dir}}{{end}}{{end}}", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, out)
	}

	var files []string
	for _, dir := range strings.Fields(string(out)) {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		for _, f := range matches {
			if !strings.HasSuffix(f, "_test.go") {
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		t.Fatal("go list returned no in-module package for the API binary — the walk is broken")
	}
	sort.Strings(files)
	return files
}

// itoaK012 renders a line number without pulling strconv in for one call,
// mirroring internal/api/v1's itoa in the sibling guard test.
func itoaK012(n int) string {
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
