package phoenix

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Phoenix admin rotations have 0 mainnet occurrences (verified in the
// lake), so this is a SYNTHETIC test of the defensive decoder: it builds
// each of the four rotation phrases with a valid Address body (reusing
// the real Address ScVal from the initialize fixture) and asserts the
// slug mapping + admin-address decode. The trailing constant is
// concatenation-split only to dodge a gitleaks false positive.
func TestDecodeAdmin_synthetic(t *testing.T) {
	d := NewDecoder()
	const adminAddr = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" // decodes from realInitTokenABody
	for _, tc := range []struct {
		phrase, wantSlug string
	}{
		{TopicAdminReplaceRequested, AdminActionReplaceRequested},
		{TopicAdminReplaceSet, AdminActionReplaceSet},
		{TopicAdminUndo, AdminActionUndo},
		{TopicAdminAccepted, AdminActionAccepted},
	} {
		t.Run(tc.wantSlug, func(t *testing.T) {
			ev := events.Event{
				ContractID:     "CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S",
				Ledger:         60_000_000,
				TxHash:         "admintx",
				OperationIndex: 0,
				EventIndex:     0,
				LedgerClosedAt: "2026-04-23T12:00:00Z",
				Topic:          []string{TopicSymbolAdmin, tc.phrase},
				Value:          realInitTokenABody, // a valid Address ScVal
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("want 1 event, got %d", len(out))
			}
			ae, ok := out[0].(AdminEvent)
			if !ok {
				t.Fatalf("want AdminEvent, got %T", out[0])
			}
			if ae.AdminAction != tc.wantSlug {
				t.Errorf("admin_action = %q, want %q", ae.AdminAction, tc.wantSlug)
			}
			if ae.Admin != adminAddr {
				t.Errorf("admin = %q, want %q", ae.Admin, adminAddr)
			}
		})
	}
}

// A void (or non-Address) body is tolerated — the action + identity
// still record, with an empty admin. Defensive, since no lake sample
// confirms the body shape.
func TestDecodeAdmin_voidBodyTolerated(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID:     "CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S",
		Ledger:         60_000_000,
		TxHash:         "admintx2",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
		Topic:          []string{TopicSymbolAdmin, TopicAdminAccepted},
		Value:          "AAAAAQ==", // SCV_VOID
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 event, got %d", len(out))
	}
	ae := out[0].(AdminEvent)
	if ae.AdminAction != AdminActionAccepted {
		t.Errorf("admin_action = %q", ae.AdminAction)
	}
	if ae.Admin != "" {
		t.Errorf("admin = %q, want empty (void body)", ae.Admin)
	}
}
