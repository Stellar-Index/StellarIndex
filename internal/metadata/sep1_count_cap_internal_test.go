package metadata

import (
	"fmt"
	"strings"
	"testing"
)

// TestParseSEP1_CapsDocumentationFieldCount and
// TestParseSEP1_CapsCurrencyCount pin T192: the per-field rune caps
// bound the BYTES of one value, but nothing bounded the COUNT of
// DOCUMENTATION keys or CURRENCIES entries a hostile stellar.toml
// could carry. Each field/entry is stored and served verbatim, so an
// unbounded count is an unbounded-growth vector even under the 1 MiB
// whole-body cap.

// wantMaxDocumentationFields and wantMaxCurrencies mirror the caps
// this fix introduces (maxDocumentationFields, maxCurrencies) as
// literals, not symbol references — so this test still COMPILES
// against the unfixed code (which has no such bound) and fails on the
// actual unbounded count, rather than on a missing identifier.
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
