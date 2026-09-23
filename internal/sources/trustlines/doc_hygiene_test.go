package trustlines

import (
	"os"
	"strings"
	"testing"
)

// The LP-reserve observer (internal/sources/liquidity_pools) shipped as
// Task #55 PR 4/5 (commit ecb28f108); "Task #65" in the same internal
// Task namespace is unrelated work, so citing it misdirects readers.
func TestLPReserveObserverCitesItsTask(t *testing.T) {
	for _, name := range []string{"decode.go", "doc.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(src)
		if strings.Contains(text, "Task #65") {
			t.Errorf("%s cites Task #65 for the LP-reserve observer; it shipped as Task #55", name)
		}
		if !strings.Contains(text, "liquidity_pools, Task #55") {
			t.Errorf(`%s must credit the LP-reserve observer as "liquidity_pools, Task #55"`, name)
		}
	}
}
