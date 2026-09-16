// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The document under test is WisdomTree's real stellar.toml, captured
// from stellar.wisdomtree.com on 2026-09-16. Line 20 ends its ACCOUNTS
// array with an unterminated basic string, so the whole file is invalid
// TOML — and every one of its eighteen [[CURRENCIES]] tables is
// well-formed. Thirteen declare an RWA anchor class and carry
// 7,023,543 tokens across roughly 30,000 trustlines each.
//
// Before section recovery this index read NOTHING from it. A missing
// quotation mark in a table we do not even consult was silencing an
// issuer's whole declaration.
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
