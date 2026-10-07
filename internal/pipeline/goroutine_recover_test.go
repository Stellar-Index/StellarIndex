package pipeline

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// TestPipelineGoroutinesRecover is the package-wide guard:
// every `go` statement in this package's non-test files must defer
// worker.Recover/worker.Report, EXCEPT the sink's two crash-by-design
// drain goroutines (persistWorker and extBuf.run). Those are deliberately
// unguarded per cmd/stellarindex-indexer/main.go:12-19: a recover() there
// would be cosmetic (persistWorker's own writes happen in child
// goroutines a recover here cannot reach) and swallowing the panic would
// leave the process answering /metrics and /healthz with a frozen cursor
// instead of crashing and letting systemd restart from the last cursor.
//
// awssdk.go's stderr-filter goroutine is not exempt: it is a log-plumbing
// side channel, not part of the write path, so it recovers and this test
// subsumes a file-scoped guard on it.
//
// Proven red: adding any bare `go func(){}()` to a non-test file in this
// package fails this test — either as a new unguarded, non-exempt site,
// or (if named persistWorker/extBuf.run) it would need its own content
// match to be exempted at all.
func TestPipelineGoroutinesRecover(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanner := guardscan.NewScanner(guardscan.Config{Guards: []string{"worker.Recover", "worker.Report"}})

	var checked, exempt int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		sites, err := scanner.ScanFile(f)
		if err != nil {
			t.Fatalf("scan %s: %v", f, err)
		}
		for _, s := range sites {
			checked++

			// Content-checked CRASH exemption: the sink's two drain
			// goroutines must NOT recover (see doc comment above).
			if s.Calls("persistWorker") || s.Calls("extBuf.run") {
				exempt++
				if s.Recovers {
					t.Errorf("go %s at %s:%d is the crash-by-design sink drain goroutine but "+
						"registers a worker guard. cmd/stellarindex-indexer/main.go:12-19 documents "+
						"why it must not: recover() here cannot reach the writes (they happen in "+
						"persistWorker's own fanned-out goroutines), and swallowing the panic would "+
						"leave the process serving /metrics with a frozen cursor instead of crashing "+
						"and letting systemd restart from the last cursor.", s.Target, f, s.Line)
				}
				continue
			}

			if s.Kind == guardscan.KindUnresolved || !s.Recovers {
				t.Errorf("go %s at %s:%d does not defer worker.Recover/worker.Report — an "+
					"unrecovered panic here kills the whole indexer process, not just this "+
					"goroutine", s.Target, f, s.Line)
			}
		}
	}

	if checked < 3 {
		t.Errorf("discovered %d goroutine(s) in internal/pipeline, want at least 3 (awssdk.go's "+
			"stderr filter plus sink.go's two drain goroutines) — the scan has drifted from the code", checked)
	}
	if exempt != 2 {
		t.Errorf("found %d crash-by-design sink goroutine(s) (persistWorker/extBuf.run), want "+
			"exactly 2 — if the sink's goroutine shape moved, re-derive whether the crash-by-design "+
			"argument still holds", exempt)
	}
}
