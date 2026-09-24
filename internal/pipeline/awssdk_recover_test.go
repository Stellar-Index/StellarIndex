package pipeline

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// TestStderrFilterGoroutineRecovers is the regression guard for the
// detached goroutine installStderrFilterTo starts to drain the stderr
// pipe. GH-1017's guard campaign was proven at file/package granularity
// elsewhere (internal/projector, internal/api/v1) but never reached this
// site: an unrecovered panic here — e.g. from a caller-supplied consume
// body — kills the whole indexer or ops process, not just the log filter.
//
// Proven red: removing the worker.Recover defer in installStderrFilterTo's
// goroutine fails this test by line.
func TestStderrFilterGoroutineRecovers(t *testing.T) {
	scanner := guardscan.NewScanner(guardscan.Config{Guards: []string{"worker.Recover", "worker.Report"}})
	sites, err := scanner.ScanFile("awssdk.go")
	if err != nil {
		t.Fatalf("scan awssdk.go: %v", err)
	}

	var checked int
	for _, s := range sites {
		checked++
		if s.Kind == guardscan.KindUnresolved || !s.Recovers {
			t.Errorf("go %s at awssdk.go:%d does not defer worker.Recover — a panic there kills "+
				"the whole process instead of just the stderr filter", s.Target, s.Line)
		}
	}
	if checked < 1 {
		t.Errorf("discovered %d goroutine(s) in awssdk.go, want at least 1 (installStderrFilterTo's "+
			"consumer) — the scan has drifted from the code", checked)
	}
}
