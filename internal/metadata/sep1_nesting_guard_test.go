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

// nestedInlineTables builds the hostile shape `a = {b={b={…1…}}}`, depth levels deep at four bytes per level: depth
// 4000 is a 16 KB document within the 100 KiB body cap that cost 1.8 GiB of decoder allocation without the guard.
func nestedInlineTables(depth int) []byte {
	var b strings.Builder
	b.WriteString("a = ")
	b.WriteString(strings.Repeat("{b=", depth))
	b.WriteString("1")
	b.WriteString(strings.Repeat("}", depth))
	return []byte(b.String())
}

// TestParseSEP1RefusesDeepNestingWithinBudget guards the parser against a document the byte cap admits spending
// gigabytes in the decoder. Both halves are asserted: refusal with [ErrTOMLTooDeep], and a near-zero refusal cost,
// since parse-then-judge would still be SIGKILLed by MemoryMax=2G.
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

// TestParseSEP1RealDocumentStillRecovers pins that the guard leaves real parses unchanged: recovery still reads
// WisdomTree's eighteen currencies past its unterminated ACCOUNTS string.
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

// TestCheckTOMLNestingRawBoundHoldsWithoutTheLexer pins the divergence insurance: even if the string-aware scan
// believed the whole document is one string, the context-free bound still refuses it.
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

// TestCheckTOMLNestingRawBoundsRefuseLiteralText pins the accepted cost of the raw backstop (see
// [maxRawTOMLNestingDepth]): literal text alone trips it at the bound, though the string-aware scans see no nesting,
// and is admitted one step below.
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

// TestParseSEP1RefusesDeepDottedKeys pins that nesting spelled with dots is held to the same budget as brackets: every
// body has a bracket depth the older scans admit and a key path one segment past [maxTOMLNestingDepth].
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

// TestParseSEP1AdmitsRealisticDottedShapes runs the key-path scan over a realistic stellar.toml with every SEP-1 table
// shape, plus dotted data a scan could mistake for keys: URLs, versions, floats, fractional-second datetimes, dotted
// quoted keys, comments.
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

// TestParseSEP1RecoveredSectionIsCheckedStandalone covers recovery: an unclosed inline table makes the whole-body scan
// read the next table header as array content, but recovery hands that section to the decoder alone and it must be
// checked there.
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

// inlineChainAcrossLines builds a key path spread over lines: `levels` lines of `k.k.….k = { # \` (segs segments
// each), closed on the last. No line has more than segs-1 dots, but z's path is levels*segs+1 segments. prefix is the
// body's opening.
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

// TestParseSEP1RefusesDottedPathAcrossLines pins that a key path built over many lines of inline tables is refused
// before the decoder, after a multi-line string closed by extra quotes and after one closed by exactly three.
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

// TestCheckTOMLNestingRawPathHoldsWithoutTheLexer is the dotted counterpart of the raw bracket insurance: with the
// string-aware scans blinded, a key path accumulated across lines is still refused. No closer sits in data, since one
// would cancel an opener (see [maxRawTOMLNestingDepth]).
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

// TestCheckTOMLNestingResyncsAfterStrings pins that the string-aware scans end every string form where the decoder
// does: each body puts a key path one segment past the bound just after a string or comment edge, visible only to a
// scan in step with the decoder.
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

// TestParseSEP1RefusesPathAfterEscapedBackslashClose pins the multi-line basic close after an escaped backslash:
// `\\""""""` ends the string after all six quotes, leaving none to open a string hiding the line. The `# }` removes
// the raw scans, so only the string-aware scans can refuse it.
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

// TestSkipTOMLStringMatchesDecoder compares where skipTOMLString ends a string with the pinned decoder lexer,
// exhaustively over short bodies. Oracle: a document with the body as a value then a second key decodes only if the
// lexer read a string spanning exactly s, so skipTOMLString must end at len(s).
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

// TestParseSEP1_CapsDocumentationFieldCount and TestParseSEP1_CapsCurrencyCount: per-field rune caps bound the bytes
// of one value, not the count of DOCUMENTATION keys or CURRENCIES entries, which are stored and served verbatim, so an
// unbounded count is a growth vector under the 1 MiB body cap.

// wantMaxDocumentationFields and wantMaxCurrencies mirror maxDocumentationFields and
// maxCurrencies as literals, not symbol references, so the test fails on an unbounded count
// rather than on a missing identifier.
const (
	wantMaxDocumentationFields = 64
	wantMaxCurrencies          = 1000
)

func TestParseSEP1_CapsDocumentationFieldCount(t *testing.T) {
	var b strings.Builder
	b.WriteString("VERSION=\"1.0.0\"\n[DOCUMENTATION]\n")
	const fieldCount = 500
	for i := 0; i < fieldCount; i++ {
		fmt.Fprintf(&b, "CUSTOM_FIELD_%04d=\"v\"\n", i)
	}

	sep, err := parseSEP1([]byte(b.String()))
	if err != nil {
		t.Fatalf("parseSEP1: %v", err)
	}
	if len(sep.Documentation) > wantMaxDocumentationFields {
		t.Fatalf("Documentation has %d fields (from %d in the document), want at most %d",
			len(sep.Documentation), fieldCount, wantMaxDocumentationFields)
	}
}

func TestParseSEP1_CapsCurrencyCount(t *testing.T) {
	var b strings.Builder
	b.WriteString("VERSION=\"1.0.0\"\n")
	const currencyCount = 1500
	for i := 0; i < currencyCount; i++ {
		fmt.Fprintf(&b, "[[CURRENCIES]]\ncode=\"C%04d\"\nissuer=\"GABC\"\n", i)
	}

	sep, err := parseSEP1([]byte(b.String()))
	if err != nil {
		t.Fatalf("parseSEP1: %v", err)
	}
	if len(sep.Currencies) > wantMaxCurrencies {
		t.Fatalf("Currencies has %d entries (from %d in the document), want at most %d",
			len(sep.Currencies), currencyCount, wantMaxCurrencies)
	}
}

// The standing and backing keys of a [[CURRENCIES]] entry must survive the
// parse, and an omitted boolean must stay distinguishable from a declared
// false: "not anchored" and "did not say" are different claims.
func TestParseCurrency_StandingAndBackingDeclarations(t *testing.T) {
	body := []byte(`
[[CURRENCIES]]
code="BOND"
issuer="GAAA"
status="live"
is_asset_anchored=true
anchor_asset_type="bond"
attestation_of_reserve="https://example.com/reserves.pdf"
redemption_instructions="Redeem through the issuer's portal."
regulated=true
approval_server="https://example.com/tx_approve"
approval_criteria="KYC required."

[[CURRENCIES]]
code="MEME"
issuer="GAAA"
is_asset_anchored=false
regulated=false

[[CURRENCIES]]
code="BARE"
issuer="GAAA"
`)
	sep, err := parseSEP1(body)
	if err != nil {
		t.Fatalf("parseSEP1: %v", err)
	}
	if len(sep.Currencies) != 3 {
		t.Fatalf("currencies = %d, want 3", len(sep.Currencies))
	}

	full := sep.Currencies[0]
	if full.Status != "live" {
		t.Errorf("Status = %q, want live", full.Status)
	}
	if full.IsAssetAnchored == nil || !*full.IsAssetAnchored {
		t.Errorf("IsAssetAnchored = %v, want true", full.IsAssetAnchored)
	}
	if full.AttestationOfReserve != "https://example.com/reserves.pdf" {
		t.Errorf("AttestationOfReserve = %q", full.AttestationOfReserve)
	}
	if full.RedemptionInstructions != "Redeem through the issuer's portal." {
		t.Errorf("RedemptionInstructions = %q", full.RedemptionInstructions)
	}
	if full.Regulated == nil || !*full.Regulated {
		t.Errorf("Regulated = %v, want true", full.Regulated)
	}
	if full.ApprovalServer != "https://example.com/tx_approve" {
		t.Errorf("ApprovalServer = %q", full.ApprovalServer)
	}
	if full.ApprovalCriteria != "KYC required." {
		t.Errorf("ApprovalCriteria = %q", full.ApprovalCriteria)
	}

	declaredFalse := sep.Currencies[1]
	if declaredFalse.IsAssetAnchored == nil || *declaredFalse.IsAssetAnchored {
		t.Errorf("declared is_asset_anchored=false read as %v, want a non-nil false", declaredFalse.IsAssetAnchored)
	}
	if declaredFalse.Regulated == nil || *declaredFalse.Regulated {
		t.Errorf("declared regulated=false read as %v, want a non-nil false", declaredFalse.Regulated)
	}

	bare := sep.Currencies[2]
	if bare.IsAssetAnchored != nil || bare.Regulated != nil {
		t.Errorf("omitted booleans read as declared: is_asset_anchored=%v regulated=%v", bare.IsAssetAnchored, bare.Regulated)
	}
}

// Free-text keys an issuer controls are capped like every other field.
func TestParseCurrency_CapsRedemptionInstructions(t *testing.T) {
	long := strings.Repeat("x", maxLongFieldRunes+100)
	sep, err := parseSEP1([]byte("[[CURRENCIES]]\ncode=\"A\"\nredemption_instructions=\"" + long + "\"\n"))
	if err != nil {
		t.Fatalf("parseSEP1: %v", err)
	}
	if got := len(sep.Currencies[0].RedemptionInstructions); got != maxLongFieldRunes {
		t.Errorf("redemption_instructions length = %d, want capped at %d", got, maxLongFieldRunes)
	}
}

// The document is a real stellar.toml whose ACCOUNTS array ends in an unterminated basic string, so the file is
// invalid TOML while every [[CURRENCIES]] table is well-formed. Section recovery must still read them: an unconsulted
// table must not silence an issuer's declaration.
func realWisdomTreeTOML(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "wisdomtree-unterminated-accounts.toml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestPartialParse_ReadsCurrenciesPastABrokenTable(t *testing.T) {
	body := realWisdomTreeTOML(t)

	// The premise: this really is invalid TOML, not a fixture that
	// quietly got fixed. If a future capture parses whole, this test is
	// asserting nothing and should be told so.
	if _, err := parseStrictForTest(body); err == nil {
		t.Fatal("the fixture parses whole — it no longer exercises section recovery; recapture a broken document or delete this test")
	}

	sep, err := parseSEP1(body)
	if err != nil {
		t.Fatalf("section recovery refused a document with eighteen well-formed currency tables: %v", err)
	}
	if len(sep.Currencies) != 18 {
		t.Errorf("read %d currencies, want 18", len(sep.Currencies))
	}

	// The section that was actually broken must be NAMED, not silently
	// absent. A caller reading ACCOUNTS off this result would otherwise
	// conclude the issuer declared none.
	if len(sep.RecoveredSections) == 0 {
		t.Fatal("a partial read reported no skipped section — the partiality is invisible to the caller")
	}
	var sawPreamble bool
	for _, s := range sep.RecoveredSections {
		if s.Header == "(top-level keys)" {
			sawPreamble = true
		}
		if s.Err == "" {
			t.Errorf("skipped section %q carries no parser error", s.Header)
		}
	}
	if !sawPreamble {
		t.Errorf("skipped sections = %+v, want the top-level block (which holds the broken ACCOUNTS array)", sep.RecoveredSections)
	}

	// And the sections that were fine must have come through whole,
	// with their values intact rather than approximated.
	byCode := map[string]Currency{}
	for _, c := range sep.Currencies {
		byCode[c.Code] = c
	}
	for code, wantType := range map[string]string{
		"WTGX": "bond", "TIPS": "bond", "SPXU": "stock", "GOLD": "commodity", "USD": "fiat",
	} {
		c, ok := byCode[code]
		if !ok {
			t.Errorf("currency %s missing from a recovered parse", code)
			continue
		}
		if c.AnchorAssetType != wantType {
			t.Errorf("%s anchor_asset_type = %q, want %q", code, c.AnchorAssetType, wantType)
		}
		if !strings.HasPrefix(c.Issuer, "G") || len(c.Issuer) != 56 {
			t.Errorf("%s issuer = %q, want a 56-char G-address", code, c.Issuer)
		}
	}
	// DOCUMENTATION is its own table and parses, so it must survive.
	if got := sep.Documentation["ORG_NAME"]; got != "WisdomTree, Inc." {
		t.Errorf("ORG_NAME = %q, want the value from the intact [DOCUMENTATION] table", got)
	}
}

// Recovery must never be reachable for a document where a table header
// could be data instead. A triple-quoted literal is the only way that
// happens, and the gate is on the whole document for that reason.
func TestPartialParse_RefusesWhenTableHeadersCouldBeText(t *testing.T) {
	for name, doc := range map[string]string{
		"basic multi-line":   "VERSION=\"1\"\nNOTE=\"\"\"\n[[CURRENCIES]]\ncode=\"EVIL\"\n\"\"\"\nbroken=\n",
		"literal multi-line": "VERSION=\"1\"\nNOTE='''\n[[CURRENCIES]]\ncode=\"EVIL\"\n'''\nbroken=\n",
	} {
		t.Run(name, func(t *testing.T) {
			sep, err := parseSEP1([]byte(doc))
			if err == nil {
				t.Fatalf("recovered a document whose table headers may be text: %d currencies read", len(sep.Currencies))
			}
			if !strings.Contains(err.Error(), "parse TOML") {
				t.Errorf("error = %v, want the original strict-parse failure", err)
			}
		})
	}
}

// A valid document must take the strict path and report nothing
// recovered — otherwise every caller would have to treat the recovery
// list as noise and would stop reading it.
func TestPartialParse_ValidDocumentIsUntouched(t *testing.T) {
	doc := "VERSION=\"2.0.0\"\n\n[DOCUMENTATION]\nORG_NAME=\"Example\"\n\n[[CURRENCIES]]\ncode=\"AAA\"\nissuer=\"GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\n"
	sep, err := parseSEP1([]byte(doc))
	if err != nil {
		t.Fatalf("valid document refused: %v", err)
	}
	if len(sep.RecoveredSections) != 0 {
		t.Errorf("valid document reported %d recovered sections, want none", len(sep.RecoveredSections))
	}
	if len(sep.Currencies) != 1 || sep.Currencies[0].Code != "AAA" {
		t.Errorf("currencies = %+v, want the single AAA entry", sep.Currencies)
	}
}

// Recovery may only ever produce a SUBSET. A section that does not
// parse is dropped, never approximated — so a currency table with a
// syntax error inside it must NOT appear in the result.
func TestPartialParse_BrokenCurrencyTableIsDroppedNotGuessed(t *testing.T) {
	doc := strings.Join([]string{
		`bad=`,
		``,
		`[[CURRENCIES]]`,
		`code="GOOD"`,
		`issuer="GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`,
		``,
		`[[CURRENCIES]]`,
		`code="BROKEN`,
		`issuer="GBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`,
		``,
	}, "\n")
	sep, err := parseSEP1([]byte(doc))
	if err != nil {
		t.Fatalf("recovery refused a document with one intact currency table: %v", err)
	}
	if len(sep.Currencies) != 1 || sep.Currencies[0].Code != "GOOD" {
		t.Fatalf("currencies = %+v, want only the table that parsed", sep.Currencies)
	}
	var named bool
	for _, s := range sep.RecoveredSections {
		if s.Header == "[[CURRENCIES]]" {
			named = true
		}
	}
	if !named {
		t.Errorf("skipped sections = %+v, want the broken [[CURRENCIES]] table named", sep.RecoveredSections)
	}
}

// parseStrictForTest is the whole-document parse, so a test can assert
// that a fixture really is broken rather than assuming it.
func parseStrictForTest(body []byte) (map[string]any, error) {
	raw := map[string]any{}
	err := toml.Unmarshal(body, &raw)
	return raw, err
}

// ─── normaliseNumeric ─────────────────────────────────────────

func TestNormaliseNumeric(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string passes through", "100000000", "100000000"},
		{"int formatted", int(42), "42"},
		{"int64 formatted", int64(1_000_000_000), "1000000000"},
		{"nil → empty", nil, ""},
		{"unsupported type → empty", true, ""},
		{"float64 → empty (TOML lib doesn't emit floats here)", 3.14, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normaliseNumeric(tc.in); got != tc.want {
				t.Errorf("normaliseNumeric(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
