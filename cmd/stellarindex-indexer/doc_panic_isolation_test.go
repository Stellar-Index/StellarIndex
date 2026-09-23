package main

import (
	"os"
	"strings"
	"testing"
)

// The package doc must not claim blanket panic isolation for the
// dispatcher-to-Timescale sink goroutine (main.go's second
// go func()): it is deliberately unguarded (T117 / #368 M4), so a
// stale claim here would send an on-call responder looking for a
// recover() that does not exist.
func TestPackageDocDoesNotOverclaimPanicIsolation(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	pkgIdx := strings.Index(src, "\npackage main")
	if pkgIdx < 0 {
		t.Fatal("could not locate \"package main\" in main.go")
	}
	doc := src[:pkgIdx]

	if strings.Contains(doc, "with panic isolation") {
		t.Fatalf("package doc claims the sink goroutine drains events "+
			"\"with panic isolation\", but it is deliberately unguarded "+
			"(see the CRASH comment at its go func() literal): %q", doc)
	}
	if !strings.Contains(doc, "unguarded") {
		t.Fatalf("package doc should describe the sink goroutine's deliberate "+
			"crash-on-panic behavior (\"unguarded\"), got: %q", doc)
	}
}
