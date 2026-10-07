package trustlines

import (
	"os"
	"strings"
	"testing"
)

// Internal task numbers do not resolve outside the private tracker.
func TestLPReserveObserverCitesNoTask(t *testing.T) {
	for _, name := range []string{"decode.go", "doc.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, stale := range []string{"Task #55", "Task #65"} {
			if strings.Contains(string(src), stale) {
				t.Errorf("%s cites %q for the LP-reserve observer", name, stale)
			}
		}
	}
}
