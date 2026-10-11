package main

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// Every goroutine in the stellarindex-api process recovers.
//
// An unrecovered panic in any goroutine kills the whole process. The narrower per-binary and
// internal/api/v1 walks cover only their own trees, so this one derives its package set from the
// linker (go list -deps .) and a newly linked package is covered the day it lands.
//
// Containment alone is not enough: each recovery must pair with the release it owns. A flight
// marker ends from a defer, a drain worker keeps close(done) outermost, and a fan-out member
// records failure in the slot its joiner reads. Each has its own behavioural test next to the code.
//
// Only the shared internal/worker guards are accepted, because they move
// stellarindex_worker_panics_total, which stellarindex_worker_panicked pages on. A bare recover()
// would turn a loud crash into a silent dead worker.
//
// The HTTP listener is the one exemption, excluded by its content: recovering the accept loop
// leaves a live process serving nothing. Argue any other fatal site by content, not a name list.
func TestAPIProcess_EveryGoroutineRecovers(t *testing.T) {
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
// The set comes from go list -deps . because only the linker's dependency closure answers what
// can panic inside this process. Packages outside the module are excluded.
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
