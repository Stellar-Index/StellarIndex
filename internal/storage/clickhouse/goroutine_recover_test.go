package clickhouse

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// clickhouseGoroutineFloor is a "did this test find anything?" floor, not
// an exact count: account_state_cache.go's refresh, ttl_liveness_cache.go's
// refresh and accounts_wealth_cache.go's refresh existed when this guard
// was written.
const clickhouseGoroutineFloor = 3

// TestClickhouseGoroutinesRecover is the package-wide guard for this
// package's detached goroutines (GH-1017): every `go` statement in a
// non-test file under internal/storage/clickhouse must defer
// worker.Recover or worker.Report. main.go-scoped guards cannot see these
// — they are started from ExplorerReader's cache-refresh paths, not from
// any binary's main().
//
// Why it matters here specifically: the refreshed account is
// attacker-chosen (a fabricated G-address hits refreshAccountState), so
// these goroutines are reachable with arbitrary input, not just
// operator-controlled data.
//
// Proven red: removing any of the worker.Recover/worker.Report defers this
// guards fails it by line.
func TestClickhouseGoroutinesRecover(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanner := guardscan.NewScanner(guardscan.Config{Guards: []string{"worker.Recover", "worker.Report"}})
	checked := 0
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
			if s.Kind == guardscan.KindUnresolved || !s.Recovers {
				t.Errorf("go %s at %s:%d does not defer worker.Recover/worker.Report — a panic "+
					"there kills the whole process, not just the one cached read", s.Target, f, s.Line)
			}
		}
	}
	if checked < clickhouseGoroutineFloor {
		t.Errorf("discovered %d goroutine(s) in internal/storage/clickhouse, want at least %d — "+
			"the scan has drifted from the code", checked, clickhouseGoroutineFloor)
	}
}
