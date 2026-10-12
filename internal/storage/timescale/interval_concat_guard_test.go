package timescale

import (
	"os"
	"strings"
	"testing"
)

// TestNoFragileIntervalConcat guards the pgx trap where `($N || ' days')::interval` makes Postgres infer $N
// as text, but pgx v5 cannot encode an int as text, so the endpoint 500s on every request. Use
// make_interval(days => $N) or make_interval(secs => $N).
func TestNoFragileIntervalConcat(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// The `$N || ' <unit>'` interval-concatenation shapes, with and
	// without the space before `||`.
	forbidden := []string{
		"|| ' day", "|| ' second", "|| ' hour", "|| ' min", "|| ' week",
		"||' day", "||' second", "||' hour", "||' min", "||' week",
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		src := string(b)
		for _, bad := range forbidden {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains fragile interval concat %q — use make_interval(days => $N) "+
					"instead; the `$N || ' unit'` form types $N as text and pgx cannot encode the "+
					"int arg, 500ing the endpoint", n, bad)
			}
		}
	}
}
