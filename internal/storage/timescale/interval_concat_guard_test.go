package timescale

import (
	"os"
	"strings"
	"testing"
)

// TestNoFragileIntervalConcat guards the pgx int→text encode trap that
// 500'd BOTH /v1/divergence (ListDivergenceLatest/Series) and
// /v1/anomalies (FreezeReasonCounts/FreezeDailyReasonCounts): writing an
// interval bound as `($N || ' days')::interval` makes Postgres infer $N
// as text (OID 25), but every Go caller passes an int, and pgx v5 has no
// int→text encode plan — so the query fails before executing and the
// endpoint 500s on every request.
//
// The fix is make_interval(days => $N) / make_interval(secs => $N),
// which types $N as an integer/double. This test forbids the fragile
// concat form anywhere in the package so the bug cannot be reintroduced
// a third time. (Cheap source scan — no DB needed; it catches the shape
// the canned-driver tests can't exercise end-to-end.)
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
