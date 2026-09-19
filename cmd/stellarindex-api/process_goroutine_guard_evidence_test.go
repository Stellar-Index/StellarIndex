//go:build k012evidence

package main

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// K012 — the legs that are still open.
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
// Both are green. Neither covers the rest of the code that is LINKED INTO
// the stellarindex-api process, and that is where the finding survives: the
// explorer's ClickHouse-side stale-while-revalidate refreshers are spelled
// exactly like the internal/api/v1 ones that were fixed, run in the same
// process, are kicked from request paths on attacker-chosen keys — and
// recover nothing. A PASS over a narrow slice reads identical to a PASS over
// everything, which is why this walk derives its package set from the
// linker's answer (`go list -deps .`) rather than from a tree root someone
// chose.
//
// Census at the time of writing: 91 `go` statements across the 76 in-module
// packages the API binary links; 13 of them register no recovery. Grouped by
// what a panic costs:
//
//   - internal/storage/clickhouse/ttl_liveness_cache.go:185 — the worst.
//     It is a process-kill AND a wedge: the body sets c.flight = nil and
//     close(fl.done) as ordinary trailing statements, not from a defer, so
//     the moment the panic is merely CONTAINED (which is what a bare
//     worker.Recover would do) coldFill's waiters block on a done channel
//     that will never close and kickRefresh keeps handing out the same dead
//     flight for the life of the process. The fix has to release from a
//     defer in the same change, exactly as #368's seventeen flight-owning
//     sites did.
//   - internal/storage/clickhouse/account_state_cache.go:180 and
//     accounts_wealth_cache.go:253 — SWR refreshers reached from
//     /v1/explorer account reads; both already release from a defer, so
//     they need the guard only.
//   - internal/storage/clickhouse/live_sink.go:122,
//     internal/canonical/discovery/sink.go:118,
//     internal/sources/sorobanevents/dispatcher_adapter.go:196 and :439,
//     internal/sources/external/chainlink/poller.go:137 and :166,
//     internal/divergence/compare.go:166 — long-running sinks, pollers and
//     fan-outs in the same process.
//   - internal/storage/timescale/trades_bulk.go:363, :371 and :429 — a
//     bounded fan-out whose members are joined by a WaitGroup. Joined is not
//     protected: the panic still takes the process down, and the waiter is
//     never released. This file is a protected path, so whoever fixes it
//     owns that review.
//
// The HTTP listener in this binary is the one deliberate exemption and is
// excluded BY ITS CONTENT, on the same argument main.go's own guard makes:
// recovering the accept loop leaves a live process serving nothing.
//
// Build-tagged because it is RED until those sites land — none of them is in
// the file set this unit was fenced to, so it is committed as evidence and
// as the acceptance test for the follow-up:
//
//	go test -tags k012evidence ./cmd/stellarindex-api/ -run TestK012 -v
//
// When it is green, drop the tag and delete this note: it is then the class
// guard for the whole process, and it subsumes — without replacing — the
// two narrower walks, because a new package linked into the API is covered
// the day it lands rather than the day someone remembers to widen a root.
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
	var unguarded []string

	for _, path := range apiProcessGoFiles(t) {
		sites, err := guardscan.ScanFile(path, guardscan.Config{Guards: guards})
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
