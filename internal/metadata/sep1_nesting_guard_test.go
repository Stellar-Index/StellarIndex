package metadata

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// nestedInlineTables builds the hostile shape: `a = {b={b={…1…}}}`,
// depth levels deep. Four bytes per level, so depth 4000 is a 16 KB
// document — well inside the 100 KiB body cap, and measured at 1.8 GiB of
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
		{"repeated array of tables", []byte(strings.Repeat(
			"[[CURRENCIES]]\ncode = \"USDC\"\nissuer = \"GA5Z\"\n", 3))},
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

// TestCheckTOMLNestingRawBoundsRefuseLiteralText pins the accepted cost
// of the raw backstop (see [maxRawTOMLNestingDepth]): literal text alone
// trips it at the bound while the string-aware scans see no nesting, and
// admits it one step below.
func TestCheckTOMLNestingRawBoundsRefuseLiteralText(t *testing.T) {
	cases := map[string]string{
		"unclosed brackets": "[",
		"dots on one line":  "Sentence. ",
	}
	for name, unit := range cases {
		t.Run(name, func(t *testing.T) {
			doc := func(n int) []byte {
				return []byte("ORG_DESCRIPTION = '" + strings.Repeat(unit, n) + "'\nORG_NAME = \"x\"\n")
			}
			below := doc(maxRawTOMLNestingDepth - 1)
			if _, err := parseSEP1(below); err != nil {
				t.Errorf("parseSEP1 one below the raw bound = %v; want nil", err)
			}
			at := doc(maxRawTOMLNestingDepth)
			if d, p := tomlNestingDepth(at), tomlKeyPathDepth(at); d != 0 || p != 1 {
				t.Fatalf("precondition: string-aware depth %d, path %d; want 0, 1", d, p)
			}
			if err := checkTOMLNesting(at); !errors.Is(err, ErrTOMLTooDeep) {
				t.Errorf("checkTOMLNesting at the raw bound = %v; want ErrTOMLTooDeep", err)
			}
		})
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
		{"multi-line basic closed by five quotes", "a = \"\"\"{{{\"\"\"\"\"\nb = {c = 1}", 1},
		{"triple quotes and brackets in a multi-line literal", "a = '''\"\"\"{{{[[['''\nb = {c = 1}", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tomlNestingDepth([]byte(tc.body)); got != tc.want {
				t.Errorf("tomlNestingDepth(%q) = %d; want %d", tc.body, got, tc.want)
			}
		})
	}
}

// dottedKey returns a key of n bare segments: `a.a.….a`.
func dottedKey(n int) string {
	return strings.TrimSuffix(strings.Repeat("a.", n), ".")
}

// TestParseSEP1RefusesDeepDottedKeys pins that nesting spelled with dots
// is held to the same budget as nesting spelled with brackets. Every
// body here has a bracket depth the older scans admit, and a key path
// one segment past [maxTOMLNestingDepth].
func TestParseSEP1RefusesDeepDottedKeys(t *testing.T) {
	const n = maxTOMLNestingDepth
	cases := []struct {
		name string
		body string
	}{
		{"dotted key", dottedKey(n+1) + " = 1\n"},
		{"dotted table header", "[" + dottedKey(n+1) + "]\n"},
		{"dotted array-of-tables header", "[[" + dottedKey(n+1) + "]]\n"},
		{"header plus key", "[" + dottedKey(n/2) + "]\n" + dottedKey(n/2+1) + " = 1\n"},
		{"key inside inline table", dottedKey(n/2) + " = {" + dottedKey(n/2+1) + " = 1}\n"},
		{"key inside inline table in array", dottedKey(n/2) + " = [{" + dottedKey(n/2+1) + " = 1}]\n"},
		{"inline table across lines", dottedKey(n/2) + " = {\n" + dottedKey(n/2+1) + " = 1\n}\n"},
		{"single key of 16,000 segments", dottedKey(16000) + " = 1\n"},
		{"dotted key, CRLF", dottedKey(n+1) + " = 1\r\n"},
		{"dotted table header, CRLF", "[" + dottedKey(n+1) + "]\r\nx = 1\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if len(body) > maxBodyBytes {
				t.Fatalf("fixture is %d bytes, over the body cap — it would never reach the parser", len(body))
			}
			if d := tomlNestingDepth(body); d > maxTOMLNestingDepth {
				t.Fatalf("precondition: bracket depth %d already trips the older bound", d)
			}
			if sep, err := parseSEP1(body); !errors.Is(err, ErrTOMLTooDeep) {
				t.Fatalf("parseSEP1 = (%v, %v); want ErrTOMLTooDeep", sep, err)
			}
		})
	}
}

// TestParseSEP1AdmitsDottedKeysAtTheBound pins the accepting side of the
// bound exactly, for each way a path is assembled.
func TestParseSEP1AdmitsDottedKeysAtTheBound(t *testing.T) {
	const n = maxTOMLNestingDepth
	cases := []struct {
		name string
		body string
	}{
		{"dotted key", dottedKey(n) + " = 1\n"},
		{"dotted table header", "[" + dottedKey(n) + "]\n"},
		{"header plus key", "[" + dottedKey(n/2) + "]\n" + dottedKey(n/2) + " = 1\n"},
		{"key inside inline table in array", dottedKey(n/2) + " = [{" + dottedKey(n/2) + " = 1}]\n"},
		{"dotted key, CRLF", dottedKey(n) + " = 1\r\n"},
		{"dotted table header, CRLF", "[" + dottedKey(n) + "]\r\n"},
		{"sibling keys do not accumulate", "x = {" + dottedKey(n-1) + " = 1, b." + dottedKey(n-2) + " = 2}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tomlKeyPathDepth([]byte(tc.body)); got != n {
				t.Fatalf("tomlKeyPathDepth = %d; want %d", got, n)
			}
			if _, err := parseSEP1([]byte(tc.body)); err != nil {
				t.Fatalf("parseSEP1 refused a document at the bound: %v", err)
			}
		})
	}
}

// TestParseSEP1AdmitsRealisticDottedShapes runs the key-path scan over a
// realistic stellar.toml carrying every SEP-1 table shape, plus the
// dotted data a scan could mistake for keys: URLs, version strings,
// floats, fractional-second datetimes, dotted quoted keys and comments.
func TestParseSEP1AdmitsRealisticDottedShapes(t *testing.T) {
	floats := strings.TrimSuffix(strings.Repeat("1.5, ", 64), ", ")
	body := `VERSION = "2.7.0"
NETWORK_PASSPHRASE = "Public Global Stellar Network ; September 2015"
FEDERATION_SERVER = "https://api.example.co.uk/federation"
ACCOUNTS = [
  "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
]
# Docs at https://www.example.co.uk/a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p.q.r.s.t.u.v.w.x.y.z.a.b.c.d.e.f.g
[DOCUMENTATION]
ORG_NAME = "Example Ltd."
ORG_URL = "https://www.example.co.uk"
ORG_DESCRIPTION = "` + strings.Repeat("One sentence. ", 100) + `"

[[PRINCIPALS]]
name = "A. N. Other"
email = "a.n.other@example.co.uk"

[[CURRENCIES]]
code = "USDX"
issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
display_decimals = 2
image = "https://cdn.example.co.uk/img/usdx.v2.png"
"collateral.addresses" = ["1.2.3.4"]
fixed_number = 1.25e3
weights = [` + floats + `]
listed = 2024-01-02T03:04:05.123456Z
extra = {a = {b = [1.5, 2.5]}}

[[VALIDATORS]]
ALIAS = "ex-1"
HOST = "core.live.example.co.uk:11625"
`
	if got := tomlKeyPathDepth([]byte(body)); got != 4 {
		t.Errorf("tomlKeyPathDepth(realistic) = %d; want 4 (CURRENCIES.extra.a.b)", got)
	}
	sep, err := parseSEP1([]byte(body))
	if err != nil {
		t.Fatalf("parseSEP1(realistic) = %v", err)
	}
	if len(sep.Currencies) != 1 || sep.Currencies[0].Code != "USDX" {
		t.Fatalf("parseSEP1(realistic) currencies = %+v; want one USDX", sep.Currencies)
	}
}

// TestParseSEP1RecoveredSectionIsCheckedStandalone covers the recovery
// path: an unclosed inline table makes the whole-body scan read the
// table header below it as array content, but section recovery hands
// that header's section to the decoder on its own.
func TestParseSEP1RecoveredSectionIsCheckedStandalone(t *testing.T) {
	deep := "[" + dottedKey(maxTOMLNestingDepth+8) + "]"
	body := []byte("broken = {\n" + deep + "\nx = 1\n[DOCUMENTATION]\nORG_NAME = \"Example\"\n")
	if got := tomlKeyPathDepth(body); got > maxTOMLNestingDepth {
		t.Fatalf("precondition: whole-body scan saw depth %d; the fixture must get past it", got)
	}
	sep, err := parseSEP1(body)
	if err != nil {
		t.Fatalf("parseSEP1 = %v; want the DOCUMENTATION section recovered", err)
	}
	var refused bool
	for _, s := range sep.RecoveredSections {
		if s.Header == deep && s.Err == ErrTOMLTooDeep.Error() {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("RecoveredSections = %+v; want %q skipped with ErrTOMLTooDeep", sep.RecoveredSections, deep)
	}
	if sep.Documentation["ORG_NAME"] != "Example" {
		t.Errorf("Documentation.OrgName = %q; want the healthy section kept", sep.Documentation["ORG_NAME"])
	}
}

// inlineChainAcrossLines builds a key path spread over lines: `levels`
// lines of `k.k.….k = { # \` (segs segments each), closed on the last
// line. No one line carries more than segs-1 dots, but the path of `z`
// is levels*segs+1 segments. prefix is the body's opening.
func inlineChainAcrossLines(prefix string, levels, segs int) []byte {
	var b strings.Builder
	b.WriteString(prefix)
	key := strings.TrimSuffix(strings.Repeat("k.", segs), ".")
	for range levels {
		b.WriteString(key + " = { # \\\n")
	}
	b.WriteString("z = 1 " + strings.Repeat("}", levels) + "\n")
	return []byte(b.String())
}

// TestParseSEP1RefusesDottedPathAcrossLines pins that a key path built up
// over many lines of inline tables is refused before the decoder, both
// after a multi-line string closed by extra quotes and after one closed
// by exactly three.
func TestParseSEP1RefusesDottedPathAcrossLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close string
	}{
		{"string closed by four quotes", `""""`},
		{"string closed by three quotes", `"""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := inlineChainAcrossLines("VERSION = \"2.0.0\"\na = \"\"\"x"+tc.close+" # \\\n", 32, 250)
			if len(body) > maxBodyBytes {
				t.Fatalf("fixture is %d bytes, over the body cap — it would never reach the parser", len(body))
			}
			if err := checkTOMLNesting(body); !errors.Is(err, ErrTOMLTooDeep) {
				t.Fatalf("checkTOMLNesting = %v; want ErrTOMLTooDeep", err)
			}
			if sep, err := parseSEP1(body); !errors.Is(err, ErrTOMLTooDeep) {
				t.Fatalf("parseSEP1 = (%v, %v); want ErrTOMLTooDeep", sep, err)
			}
		})
	}
}

// TestCheckTOMLNestingRawPathHoldsWithoutTheLexer is the dotted
// counterpart of the raw bracket insurance: with the string-aware scans
// blinded, a key path the raw scan sees only by accumulating it across
// lines is still refused. No closer sits in data here: one would cancel
// an opener in the raw count (see [maxRawTOMLNestingDepth]).
func TestCheckTOMLNestingRawPathHoldsWithoutTheLexer(t *testing.T) {
	// An unterminated multi-line string hides the rest of the body from
	// the string-aware scans.
	body := inlineChainAcrossLines("a = \"\"\"\n", 32, 250)
	if got := tomlKeyPathDepth(body); got > maxTOMLNestingDepth {
		t.Fatalf("precondition: tomlKeyPathDepth = %d; the open string must blind it", got)
	}
	if got := tomlNestingDepth(body); got > maxTOMLNestingDepth {
		t.Fatalf("precondition: tomlNestingDepth = %d; the open string must blind it", got)
	}
	if err := checkTOMLNesting(body); !errors.Is(err, ErrTOMLTooDeep) {
		t.Errorf("checkTOMLNesting = %v; want ErrTOMLTooDeep from the raw path bound", err)
	}
}

// TestCheckTOMLNestingResyncsAfterStrings pins that the string-aware
// scans end every string form where the decoder does. Each body puts a
// key path one segment past the bound just after a string or comment
// edge, where only a scan still in step with the decoder sees it.
func TestCheckTOMLNestingResyncsAfterStrings(t *testing.T) {
	deep := dottedKey(maxTOMLNestingDepth) + " = 1"
	cases := []struct {
		name  string
		body  string
		valid bool // the decoder itself accepts the body
	}{
		{"multi-line basic closed by four quotes", `t = {s = """x"""", ` + deep + "}\n", true},
		{"multi-line basic closed by five quotes", `t = {s = """x""""", ` + deep + "}\n", true},
		{"multi-line literal closed by four quotes", `t = {s = '''x'''', ` + deep + "}\n", true},
		{"multi-line literal closed by five quotes", `t = {s = '''x''''', ` + deep + "}\n", true},
		{"multi-line basic with line-ending backslash", "a = \"\"\"x\\\n  y\"\"\"\nt." + deep + "\n", true},
		{"comment with backslash after an inline table", "a = {b = 1} # \\\nt." + deep + "\n", true},
		{"single-line basic ending in backslash", "a = \"x\\\nt." + deep + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if err := checkTOMLNesting(body); !errors.Is(err, ErrTOMLTooDeep) {
				t.Fatalf("checkTOMLNesting(%q) = %v; want ErrTOMLTooDeep", body, err)
			}
			if !tc.valid {
				return
			}
			if sep, err := parseSEP1(body); !errors.Is(err, ErrTOMLTooDeep) {
				t.Fatalf("parseSEP1(%q) = (%v, %v); want ErrTOMLTooDeep", body, sep, err)
			}
		})
	}
}

func TestTOMLKeyPathDepth(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bare key", "a = 1", 1},
		{"dotted key", "a.b.c = 1", 3},
		{"whitespace around dots", "a . b\t. c = 1", 3},
		{"quoted segment dots are data", `"a.b.c".d = 1`, 2},
		{"header then key", "[a.b]\nc = 1", 3},
		{"array-of-tables header", "[[a.b]]\nc = 1", 3},
		{"later header replaces earlier", "[a.b.c.d]\n[e]\nf = 1", 4},
		{"float value", "a = 3.14159", 1},
		{"floats in an array", "a = [1.1, 2.2, 3.3, 4.4]", 1},
		{"datetime value", "a = 1979-05-27T07:32:00.999", 1},
		{"string value", `a = "x.y.z.w"`, 1},
		{"comment", "# a.b.c.d = 1\ne = 1", 1},
		{"inline table", "a.b = {c.d = 1}", 4},
		{"inline table after array element", "a = [1.5, {b = 1}]", 2},
		{"closed inline table pops", "a.b = {c = 1}\nd = 1", 3},
		{"multi-line array", "a.b = [\n  {c = 1},\n]\nd = 1", 3},
		{"stray close does not go negative", "}]\na.b = 1", 2},
		{"triple quotes in a multi-line literal", "a = '''\"\"\"x.y.z'''\nb.c = 1", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tomlKeyPathDepth([]byte(tc.body)); got != tc.want {
				t.Errorf("tomlKeyPathDepth(%q) = %d; want %d", tc.body, got, tc.want)
			}
		})
	}
}

// TestRawKeyPathDepth pins the raw path estimate. It may exceed the true
// path (header lines count their own bracket; dots in data count) but
// must not fall below it.
func TestRawKeyPathDepth(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"table header, no dots", "a = 1\n[[CURRENCIES]]\ncode = \"X\"\n", 2},
		{"dotted key", "a.b.c = 1", 3},
		{"header then key", "[a.b]\nc.d = 1", 4},
		{"later header at depth 0 replaces", "[a.b.c.d]\n[e]\nf.g = 1", 5},
		{"header inside an open bracket only raises", "[a.b.c.d]\nx = [\n[e]\nf.g = 1", 7},
		{"inline table on one line", "a.b = {c.d = 1}", 5},
		{"inline table across lines", "a.b = {\nc.d = 1\n}", 4},
		{"closed table pops", "a.b = {c = 1}\nd.e = 1", 4},
		{"dots in strings and comments count", "a = \"x.y\" # z.w\n", 3},
		{"CRLF", "a.b = {\r\nc.d = 1\r\n}\r\n", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawKeyPathDepth([]byte(tc.body)); got != tc.want {
				t.Errorf("rawKeyPathDepth(%q) = %d; want %d", tc.body, got, tc.want)
			}
		})
	}

	const n = maxRawTOMLNestingDepth
	if got := rawKeyPathDepth([]byte(dottedKey(n) + " = 1\n")); got != n {
		t.Errorf("rawKeyPathDepth(%d-segment key) = %d; want %d", n, got, n)
	}
	if err := checkTOMLNesting([]byte("a = \"\"\"\n" + dottedKey(n+1) + " = 1\n")); !errors.Is(err, ErrTOMLTooDeep) {
		t.Errorf("checkTOMLNesting(hidden %d-segment key) = %v; want ErrTOMLTooDeep", n+1, err)
	}
}

// bomHeaderBody is a table header of hsegs segments followed by as many
// ksegs-segment keys as fit in maxBytes, behind the given byte-order mark.
func bomHeaderBody(bom string, hsegs, ksegs, maxBytes int) []byte {
	var b strings.Builder
	b.WriteString(bom + "[" + dottedKey(hsegs) + "]\n")
	tail := strings.Repeat(".k", ksegs-1) + " = 1\n"
	for i := 0; ; i++ {
		ln := "a" + strconv.Itoa(i) + tail
		if b.Len()+len(ln) > maxBytes {
			break
		}
		b.WriteString(ln)
	}
	return []byte(b.String())
}

// TestParseSEP1RefusesDeepPathBehindBOM pins that a byte-order mark, which
// the decoder strips before lexing, does not hide the first line's table
// header from the scans.
func TestParseSEP1RefusesDeepPathBehindBOM(t *testing.T) {
	for _, tc := range []struct{ name, bom string }{
		{"no BOM", ""},
		{"UTF-8 BOM", "\xef\xbb\xbf"},
		{"UTF-16 BE BOM", "\xfe\xff"},
		{"UTF-16 LE BOM", "\xff\xfe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bomHeaderBody(tc.bom, 250, 31, maxBodyBytes)
			if err := checkTOMLNesting(body); !errors.Is(err, ErrTOMLTooDeep) {
				t.Fatalf("checkTOMLNesting = %v; want ErrTOMLTooDeep", err)
			}
			if sep, err := parseSEP1(body); !errors.Is(err, ErrTOMLTooDeep) {
				t.Fatalf("parseSEP1 = (%v, %v); want ErrTOMLTooDeep", sep, err)
			}
		})
	}
	// Past the mark, the header still starts a line.
	if err := checkTOMLNesting([]byte("\xef\xbb\xbf[" + dottedKey(maxTOMLNestingDepth+1) + "]\nx = 1\n")); !errors.Is(err, ErrTOMLTooDeep) {
		t.Errorf("checkTOMLNesting(BOM + %d-segment header) = %v; want ErrTOMLTooDeep", maxTOMLNestingDepth+1, err)
	}
}

// TestParseSEP1RefusesPathAfterEscapedBackslashClose pins the multi-line
// basic close the decoder takes after an escaped backslash: `\\""""""`
// ends the string after all six quotes, so no quote is left over to open
// a string that hides the rest of the line. The `# }` on each line takes
// the raw scans out of play, so only the string-aware scans can refuse it.
func TestParseSEP1RefusesPathAfterEscapedBackslashClose(t *testing.T) {
	var b strings.Builder
	b.WriteString("VERSION = \"2.0.0\"\nr = {\n")
	key := strings.TrimSuffix(strings.Repeat("k.", 250), ".")
	for range 32 {
		b.WriteString(`s = """\\"""""", ` + key + " = { # }\n")
	}
	b.WriteString("z = 1 " + strings.Repeat("}", 33) + "\n")
	body := []byte(b.String())
	if err := checkTOMLNesting(body); !errors.Is(err, ErrTOMLTooDeep) {
		t.Fatalf("checkTOMLNesting = %v; want ErrTOMLTooDeep", err)
	}
	if sep, err := parseSEP1(body); !errors.Is(err, ErrTOMLTooDeep) {
		t.Fatalf("parseSEP1 = (%v, %v); want ErrTOMLTooDeep", sep, err)
	}
	if got := skipTOMLString([]byte(`"""\\"""""" x`), 0); got != 11 {
		t.Errorf(`skipTOMLString("""\\"""""") = %d; want 11 (after the sixth quote)`, got)
	}
}

// TestSkipTOMLStringMatchesDecoder compares where skipTOMLString ends a
// string with where the pinned decoder's lexer ends it, exhaustively over
// short string bodies.
//
// Oracle: for a body s, the document `k = <s>\nz = 1\n` decodes with both
// keys only if the lexer read a string spanning exactly s (anything left
// on the line after a value is an error), give or take trailing
// newlines. Every accepted document must then have skipTOMLString end at
// len(s). A rejected document carries no claim: the decoder stops at its
// error, and every string the lexer does close is itself enumerated as a
// body cut at that close, whose next byte is never a quote either.
func TestSkipTOMLStringMatchesDecoder(t *testing.T) {
	forms := []struct {
		open  string
		alpha string
		max   int
	}{
		{`"`, "\"'\\x\n", 7},
		{`'`, "\"'\\x\n", 7},
		{`"""`, "\"'\\x\n", 7},
		{`'''`, "\"'\\x\n", 7},
		// Eight bytes reach `\\""""""`; the other quote is inert here.
		{`"""`, "\"\\x\n", 8},
		{`'''`, "'\\x\n", 8},
	}
	for _, f := range forms {
		t.Run(fmt.Sprintf("%s len<=%d", f.open, f.max), func(t *testing.T) {
			t.Parallel()
			accepted := 0
			var walk func(s []byte)
			walk = func(s []byte) {
				doc := append(append([]byte("k = "), s...), "\nz = 1\n"...)
				var m map[string]any
				if toml.Unmarshal(doc, &m) == nil && m["z"] != nil {
					accepted++
					want := 4 + len(s)
					got := skipTOMLString(doc, 4)
					if got > want || strings.Trim(string(doc[got:want]), "\n") != "" {
						t.Errorf("skipTOMLString(%q) ends at %d; the decoder ends it at %d", s, got-4, len(s))
					}
				}
				if len(s)-len(f.open) >= f.max {
					return
				}
				for i := range len(f.alpha) {
					walk(append(s[:len(s):len(s)], f.alpha[i]))
				}
			}
			walk([]byte(f.open))
			if accepted == 0 {
				t.Fatal("the oracle accepted no document; it compared nothing")
			}
			t.Logf("%d accepted documents compared", accepted)
		})
	}
}
