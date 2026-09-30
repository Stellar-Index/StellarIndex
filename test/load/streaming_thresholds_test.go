package load

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// streamingSLA captures the body of the `streaming:` entry in lib/thresholds.js.
var streamingSLA = regexp.MustCompile(`(?s)\n\s*streaming:\s*\{(.*?)\n\s*\},`)

// TestStreamingSLAIsNotGatedOnRequestFailure fails when the SSE scenario is
// gated on http_req_failed: the stream never closes, so every subscribe ends
// in k6's client timeout and that gate reads 100 % failed on a healthy run.
func TestStreamingSLAIsNotGatedOnRequestFailure(t *testing.T) {
	thresholds, err := os.ReadFile(filepath.Join("scenarios", "lib", "thresholds.js"))
	if err != nil {
		t.Fatalf("read thresholds.js: %v", err)
	}
	m := streamingSLA.FindSubmatch(thresholds)
	// Known case: the block must carry the first-event gate, or the
	// regexp matched the wrong span and the assertions below are vacuous.
	if m == nil || !strings.Contains(string(m[1]), "'sse_first_event_ms'") {
		t.Fatalf("could not locate sla.streaming with its sse_first_event_ms gate in thresholds.js")
	}
	block := string(m[1])
	if strings.Contains(block, "'http_req_failed'") {
		t.Errorf("sla.streaming gates http_req_failed, which every SSE subscribe trips by timing out; " +
			"gate the scenario's own sse_subscribe_ok rate instead")
	}
	if !strings.Contains(block, "'sse_subscribe_ok'") {
		t.Errorf("sla.streaming has no sse_subscribe_ok gate; subscribe failures would go ungated")
	}

	scenario, err := os.ReadFile(filepath.Join("scenarios", "05-streaming.js"))
	if err != nil {
		t.Fatalf("read 05-streaming.js: %v", err)
	}
	if !strings.Contains(string(scenario), "new Rate('sse_subscribe_ok')") {
		t.Errorf("05-streaming.js does not emit the sse_subscribe_ok rate its threshold gates")
	}
	// On a timeout with no response byte k6 still reports waiting as the time
	// since the request was written, so only receiving proves a first event.
	if !strings.Contains(string(scenario), "const opened = r.timings.receiving > 0") {
		t.Errorf("05-streaming.js must gate a successful subscribe on r.timings.receiving > 0, " +
			"not on waiting, which a hung subscribe also fills")
	}
}
