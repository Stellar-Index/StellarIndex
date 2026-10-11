package pipeline

import (
	"os"
	"strings"
	"testing"
)

// TestPersistEventsGodocMatchesConcurrencyModel keeps the stale "One goroutine drains" language out of PersistEvents' godoc, which launches PersistWorkers (8) concurrent drains.
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
