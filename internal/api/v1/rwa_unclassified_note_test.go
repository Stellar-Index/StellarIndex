// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"strings"
	"testing"
)

// `unclassified` is the largest group on this breakdown after the
// contract funds — on 2026-09-16 it carried $971,860,304.89 of a
// $2,535,764,187.91 total, which is BENJI and USDY. The word reads as a
// gap in OUR data. It is a statement about the ISSUERS': Franklin
// Templeton's own SEP-1 for BENJI declares `anchor_asset_type = "other"`
// and Ondo declares none for USDY, so publishing a class would
// contradict one issuer on their own asset and invent one for the other.
//
// A reader comparing this page with a third party's breakdown — which
// classifies everything, because it is not reading the issuers — would
// take the biggest number on it for missing work. The group carries the
// reason with it.
func TestUnclassifiedGroupCarriesItsReason(t *testing.T) {
	groups := rwaByClass([]RWAAsset{
		{Code: "BENJI", AnchorClass: ""},
		{Code: "USDY", AnchorClass: ""},
		{Code: "EUTBL", AnchorClass: "bond"},
	})

	var unclassified, bond *RWAGroupTotal
	for i := range groups {
		switch groups[i].Class {
		case rwaUnclassified:
			unclassified = &groups[i]
		case "bond":
			bond = &groups[i]
		}
	}
	if unclassified == nil {
		t.Fatalf("no unclassified group over rows with no anchor class; groups = %+v", groups)
	}
	if unclassified.Note == "" {
		t.Error("the unclassified group carries no note — the word is left to read as a gap in this index's data")
	}
	// The note has to say whose statement it is about. A note that only
	// said "no class available" would be the same ambiguity in longer form.
	for _, want := range []string{"oracle", "declared"} {
		if !strings.Contains(unclassified.Note, want) {
			t.Errorf("note does not mention %q; got %q", want, unclassified.Note)
		}
	}

	// Every group that named itself must stay silent. A note on all of
	// them is a note nobody reads.
	if bond == nil {
		t.Fatal("no bond group")
	}
	if bond.Note != "" {
		t.Errorf("the bond group carries a note (%q); only a class that cannot speak for itself should", bond.Note)
	}
}
