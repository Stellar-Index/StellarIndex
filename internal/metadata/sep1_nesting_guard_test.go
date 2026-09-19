package metadata

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// nestedInlineTables builds the hostile shape: `a = {b={b={…1…}}}`,
// depth levels deep. Four bytes per level, so depth 4000 is a 16 KB
// document — well inside the 1 MiB body cap, and measured at 1.8 GiB of
// decoder allocation before this guard existed.
func nestedInlineTables(depth int) []byte {
	var b strings.Builder
	b.WriteString("a = ")
	b.WriteString(strings.Repeat("{b=", depth))
	b.WriteString("1")
	b.WriteString(strings.Repeat("}", depth))
	return []byte(b.String())
}

// TestParseSEP1RefusesDeepNestingWithinBudget is the wedge (RSEC-Z1 /
// RLT-458) at the parser: a document the byte cap admits must not be
// allowed to spend gigabytes in the decoder.
//
// The assertion is on BOTH halves, because either alone is passable by
// a wrong fix: the document must be REFUSED with [ErrTOMLTooDeep], and
// the refusal must cost near-nothing — a fix that parsed first and
// judged afterwards would satisfy the error check while still being
// SIGKILLed by the unit's MemoryMax=2G.
func TestParseSEP1RefusesDeepNestingWithinBudget(t *testing.T) {
	body := nestedInlineTables(4000)
	if len(body) > maxBodyBytes {
		t.Fatalf("fixture is %d bytes, over the %d body cap — it would never reach the parser",
			len(body), maxBodyBytes)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	sep, err := parseSEP1(body)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrTOMLTooDeep) {
		t.Fatalf("parseSEP1(16 KB nested to depth 4000) = (%v, %v); want ErrTOMLTooDeep", sep, err)
	}
	// The pre-guard measurement for this exact input was 1.81 GiB. 16 MiB
	// is ~100x headroom over the guard's own cost and ~100x under the
	// unguarded cost, so it cannot be met by accident.
	const allocBudget = 16 << 20
	if spent := after.TotalAlloc - before.TotalAlloc; spent > allocBudget {
		t.Fatalf("refusing the document allocated %d bytes; want at most %d — the decoder ran",
			spent, allocBudget)
	}
}

// TestParseSEP1AdmitsRealDocuments pins the other side of the guard: it
// is a destructive refusal, so the shapes it must NOT fire on are
// asserted on real and realistic documents, not on a toy.
func TestParseSEP1AdmitsRealDocuments(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	fixture := filepath.Join(filepath.Dir(thisFile), "testdata", "wisdomtree-unterminated-accounts.toml")
	issuerDoc, err := os.ReadFile(fixture) //nolint:gosec // fixture path derived from this test's own location
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"real issuer document (recovered parse)", issuerDoc},
		{"array of inline tables", []byte(
			"[[CURRENCIES]]\ncode = \"USDC\"\nissuer = \"GA5Z\"\nextra = {a = {b = [1, 2, 3]}}\n")},
		{"brackets and braces inside strings", []byte(
			"[DOCUMENTATION]\nORG_NAME = \"" + strings.Repeat("{[", 40) + "\"\n" +
				"ORG_DESCRIPTION = '''\n" + strings.Repeat("[[[", 40) + "\n'''\n")},
		{"brackets inside a comment", []byte(
			"# " + strings.Repeat("{", 40) + "\nVERSION = \"2.0.0\"\n")},
		{"nesting right up to the bound", []byte("a = " +
			strings.Repeat("[", maxTOMLNestingDepth) + "1" + strings.Repeat("]", maxTOMLNestingDepth))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkTOMLNesting(tc.body); err != nil {
				t.Fatalf("checkTOMLNesting refused a legitimate document: %v", err)
			}
			if _, err := parseSEP1(tc.body); err != nil {
				t.Fatalf("parseSEP1 refused a legitimate document: %v", err)
			}
		})
	}
}

// TestParseSEP1RealDocumentStillRecovers proves the guard did not change
// what a real document parses TO — the recovery path still reads
// WisdomTree's eighteen currencies past its unterminated ACCOUNTS
// string.
func TestParseSEP1RealDocumentStillRecovers(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	fixture := filepath.Join(filepath.Dir(thisFile), "testdata", "wisdomtree-unterminated-accounts.toml")
	body, err := os.ReadFile(fixture) //nolint:gosec // fixture path derived from this test's own location
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sep, err := parseSEP1(body)
	if err != nil {
		t.Fatalf("parseSEP1(real fixture) = %v", err)
	}
	if len(sep.Currencies) == 0 {
		t.Fatal("parseSEP1(real fixture) read no currencies; the guard broke section recovery")
	}
	if len(sep.RecoveredSections) == 0 {
		t.Fatal("parseSEP1(real fixture) reported no recovered sections; expected the ACCOUNTS table")
	}
}

// TestCheckTOMLNestingBoundary pins the bound exactly, so a later
// change to [maxTOMLNestingDepth] is a deliberate act rather than a
// side effect.
func TestCheckTOMLNestingBoundary(t *testing.T) {
	at := []byte("a = " + strings.Repeat("[", maxTOMLNestingDepth) + "1" +
		strings.Repeat("]", maxTOMLNestingDepth))
	if err := checkTOMLNesting(at); err != nil {
		t.Errorf("checkTOMLNesting at depth %d = %v; want nil", maxTOMLNestingDepth, err)
	}
	over := []byte("a = " + strings.Repeat("[", maxTOMLNestingDepth+1) + "1" +
		strings.Repeat("]", maxTOMLNestingDepth+1))
	if err := checkTOMLNesting(over); !errors.Is(err, ErrTOMLTooDeep) {
		t.Errorf("checkTOMLNesting at depth %d = %v; want ErrTOMLTooDeep", maxTOMLNestingDepth+1, err)
	}
}

// TestCheckTOMLNestingRawBoundHoldsWithoutTheLexer proves the
// divergence insurance: even if the string-aware scan were fooled into
// believing the whole document is one string, the context-free bound
// still refuses it.
func TestCheckTOMLNestingRawBoundHoldsWithoutTheLexer(t *testing.T) {
	// An unterminated basic string makes tomlNestingDepth skip to the
	// newline and see nothing structural at all.
	body := []byte("a = \"" + strings.Repeat("{", maxRawTOMLNestingDepth+1) + "\n")
	if got := tomlNestingDepth(body); got != 0 {
		t.Fatalf("precondition: tomlNestingDepth = %d; want 0 (the lexical scan must be blind here)", got)
	}
	if err := checkTOMLNesting(body); !errors.Is(err, ErrTOMLTooDeep) {
		t.Errorf("checkTOMLNesting = %v; want ErrTOMLTooDeep from the raw bound", err)
	}
}

func TestTOMLNestingDepth(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"flat", "VERSION = \"2.0.0\"\n", 0},
		{"one table header", "[DOCUMENTATION]\nORG_NAME = \"x\"\n", 1},
		{"array of tables", "[[CURRENCIES]]\ncode = \"X\"\n", 2},
		{"inline table in array", "a = [{b = 1}]", 2},
		{"brace in basic string", "a = \"{{{\"\nb = 1", 0},
		{"brace in literal string", "a = '{{{'\nb = 1", 0},
		{"brace in multi-line literal", "a = '''\n{{{\n'''\nb = 1", 0},
		{"escaped quote does not end the string", `a = "\"{{{"` + "\nb = 1", 0},
		{"brace after a closed string still counts", "a = \"x\"\nb = {c = 1}", 1},
		{"comment braces ignored", "# {{{\na = 1", 0},
		{"stray close does not go negative", "}}}\na = {b = 1}", 1},
		{"multi-line basic string", "a = \"\"\"\n{{{\n\"\"\"\nb = {c = 1}", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tomlNestingDepth([]byte(tc.body)); got != tc.want {
				t.Errorf("tomlNestingDepth(%q) = %d; want %d", tc.body, got, tc.want)
			}
		})
	}
}
