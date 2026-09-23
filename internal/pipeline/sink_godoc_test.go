package pipeline

import (
	"os"
	"strings"
	"testing"
)

// TestPersistEventsGodocMatchesConcurrencyModel guards T415: sink.go's
// PersistEvents godoc once claimed, in the same comment block, both that
// "One goroutine drains; per-event work is sequential" and that
// PersistEvents "launches [PersistWorkers] concurrent drain goroutines"
// (PersistWorkers == 8). The single-goroutine language was stale --
// confirm it stays gone rather than silently reappearing.
func TestPersistEventsGodocMatchesConcurrencyModel(t *testing.T) {
	src, err := os.ReadFile("sink.go")
	if err != nil {
		t.Fatalf("read sink.go: %v", err)
	}
	text := string(src)

	stale := []string{
		"One goroutine drains; per-event work is sequential",
		"a small worker pool of 4",
	}
	for _, s := range stale {
		if strings.Contains(text, s) {
			t.Errorf("sink.go godoc still contains stale single-goroutine claim %q, contradicting PersistWorkers = %d", s, PersistWorkers)
		}
	}

	if PersistWorkers != 8 {
		t.Fatalf("PersistWorkers changed to %d; update this test's expectations", PersistWorkers)
	}
	if !strings.Contains(text, "launches [PersistWorkers] concurrent drain") {
		t.Error("sink.go godoc no longer documents PersistEvents as launching PersistWorkers concurrent drain goroutines")
	}
}
