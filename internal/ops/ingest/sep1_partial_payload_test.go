// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/metadata"
)

// The parser reports which tables it could not read. If that record
// stops at the storage boundary, every consumer downstream of the
// issuers row reads "the issuer declared nothing here" where the truth
// is "we could not read the table that would have said" — which is the
// absent-versus-zero mistake, one layer down.
func TestPayloadCarriesTheTablesThatCouldNotBeRead(t *testing.T) {
	sep := &metadata.SEP1{
		OrgName:   "WisdomTree, Inc.",
		FetchedAt: time.Now().UTC(),
		Documentation: map[string]string{
			"ORG_NAME": "WisdomTree, Inc.",
		},
		Currencies: []metadata.Currency{{Code: "WTGX", Issuer: "GDMBNMFJ3TRFLASJ6UGETFME3PJPNKPU24C7KFDBEBPQFG2CI6UC3JG6"}},
		RecoveredSections: []metadata.SkippedSection{
			{Header: "(top-level keys)", Err: "toml: line 20: strings cannot contain newlines"},
		},
	}

	b, err := marshalSep1Payload(sep, true)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	raw, ok := got["RecoveredSections"]
	if !ok {
		t.Fatal("the stored payload does not carry RecoveredSections — a partial read is indistinguishable from a complete one once written")
	}
	list, ok := raw.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("RecoveredSections = %#v, want one entry", raw)
	}
	entry, _ := list[0].(map[string]any)
	if entry["Header"] != "(top-level keys)" {
		t.Errorf("Header = %v, want the skipped table named", entry["Header"])
	}
	if entry["Err"] == "" || entry["Err"] == nil {
		t.Error("Err is empty — the reason the table was skipped did not survive")
	}
}

// A document that parsed whole must not grow a key. Almost every row is
// one of these, and a field present-but-empty on all of them is how a
// reader learns to stop looking at it.
func TestWholeDocumentPayloadCarriesNoRecoveryKey(t *testing.T) {
	sep := &metadata.SEP1{
		OrgName:       "Example",
		FetchedAt:     time.Now().UTC(),
		Documentation: map[string]string{"ORG_NAME": "Example"},
		Currencies:    []metadata.Currency{{Code: "AAA", Issuer: "GAAA"}},
	}
	b, err := marshalSep1Payload(sep, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["RecoveredSections"]; ok {
		t.Errorf("a whole-document parse wrote RecoveredSections; payload = %s", b)
	}
}
