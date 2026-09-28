// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
)

// The methodology pages are hand-written and are what a reader uses to
// re-derive a published figure, so a status, provenance or count they
// state has to be the one the code emits. These tests pin the
// vocabularies and counts that drifted before against the constants the
// read paths actually use.

const methodologyDir = "../../../docs/methodology"

func readMethodologyDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(methodologyDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// mdSection returns the body under the first heading line equal to
// heading, up to the next heading of the same or a higher level.
func mdSection(t *testing.T, doc, heading string) string {
	t.Helper()
	level := strings.IndexFunc(heading, func(r rune) bool { return r != '#' })
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != heading {
			continue
		}
		var out []string
		for _, m := range lines[i+1:] {
			if lvl := strings.IndexFunc(m, func(r rune) bool { return r != '#' }); lvl > 0 && lvl <= level && strings.HasPrefix(m[lvl:], " ") {
				break
			}
			out = append(out, m)
		}
		return strings.Join(out, "\n")
	}
	t.Fatalf("heading %q not found", heading)
	return ""
}

var mdFirstCellCode = regexp.MustCompile("(?m)^\\|\\s*`([a-z_]+)`\\s*\\|")

func mdFirstCellCodes(section string) []string {
	var out []string
	for _, m := range mdFirstCellCode.FindAllStringSubmatch(section, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in ...string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

var mdNumberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
}

func TestRWADefinitionDocValuationStatusTableMatchesCode(t *testing.T) {
	doc := readMethodologyDoc(t, "rwa-definition.md")
	got := mdFirstCellCodes(mdSection(t, doc, "## Valuation — the market basis"))
	want := sortedStrings(RWAValuationPublished, RWAValuationIssuerFlagged, RWAValuationUnpriced,
		RWAValuationLowLiquidity, RWAValuationNoSupply, RWAValuationDecimalsUnknown)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("valuation.status table lists %v, the code emits %v", got, want)
	}
}

func TestRWADefinitionDocProvenanceTableMatchesCode(t *testing.T) {
	doc := readMethodologyDoc(t, "rwa-definition.md")
	var got []string
	for _, c := range mdFirstCellCodes(mdSection(t, doc, "#### The one price a contract row can carry")) {
		if c != "provenance" { // the header cell
			got = append(got, c)
		}
	}
	want := sortedStrings(RWAReferenceOracleNAV, RWAReferenceListingPrice,
		RWAReferenceProspectusCNAV, RWAReferenceCuratorPrice)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("provenance table lists %v, the code issues %v", got, want)
	}
}

// Every status the reference path can refuse with is named on the page,
// and the "other N rules" heading counts the rows of its own table.
func TestRWADefinitionDocNamesEveryReferenceRefusal(t *testing.T) {
	doc := readMethodologyDoc(t, "rwa-definition.md")
	for _, st := range rwaReferenceRefusalOrder {
		if !strings.Contains(doc, "`"+st+"`") {
			t.Errorf("reference refusal %q is emitted by the code and named nowhere on the page", st)
		}
	}
	m := regexp.MustCompile(`(?m)^### The other (\w+) rules$`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatal(`no "### The other N rules" heading`)
	}
	rows := regexp.MustCompile("(?m)^\\|.*\\|\\s*`([a-z_]+)`\\s*\\|$").
		FindAllStringSubmatch(mdSection(t, doc, m[0]), -1)
	if mdNumberWords[m[1]] != len(rows) {
		t.Errorf("heading says %q rules, its table has %d rows", m[1], len(rows))
	}
}

func TestRWADefinitionDocContractBindingCountMatchesCode(t *testing.T) {
	doc := readMethodologyDoc(t, "rwa-definition.md")
	c4 := mdSection(t, doc, "### C4 — Real-world instrument")
	bindings := rwa.ContractInstrumentBindings()
	m := regexp.MustCompile(`It now holds (\w+) bindings`).FindStringSubmatch(c4)
	if m == nil {
		t.Fatal(`C4 does not state "It now holds N bindings"`)
	}
	if mdNumberWords[m[1]] != len(bindings) {
		t.Errorf("C4 says %q bindings, internal/rwa/contract.go holds %d", m[1], len(bindings))
	}
	for _, b := range bindings {
		if !strings.Contains(c4, "`"+b.Class+"`") {
			t.Errorf("C4 never names class %q, which %s is bound as", b.Class, b.Instrument)
		}
	}
}

// membership.built_at is on the wire; the page may not say it is not.
func TestRWADefinitionDocDoesNotDenyTheBuildTimestamp(t *testing.T) {
	f, ok := reflect.TypeOf(RWAMembershipSet{}).FieldByName("BuiltAt")
	if !ok || f.Tag.Get("json") != "built_at" {
		t.Fatal("RWAMembershipSet.BuiltAt with json built_at is the premise of this test")
	}
	doc := readMethodologyDoc(t, "rwa-definition.md")
	if strings.Contains(doc, "no build timestamp") {
		t.Error(`page claims the response "carries no build timestamp" while membership.built_at is served`)
	}
}

// `issuers` has two writers — the trade-path registerIssuerSeen and the
// holdings-path insertIssuersBatch (migration 0158). A page naming one as
// the only writer states the root cause of the coverage gap wrongly.
func TestRWADocsNameBothIssuersWriters(t *testing.T) {
	for _, name := range []string{"rwa-definition.md", "rwa-coverage-reconciliation.md"} {
		doc := readMethodologyDoc(t, name)
		if strings.Contains(doc, "registerIssuerSeen") && !strings.Contains(doc, "insertIssuersBatch") {
			t.Errorf("%s names registerIssuerSeen as the issuers writer and omits insertIssuersBatch", name)
		}
	}
}
