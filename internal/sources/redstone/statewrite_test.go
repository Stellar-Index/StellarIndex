package redstone

import (
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ── Subset attribution from state-write keys (exact path) ──────────────
//
// Real lake fixture: the REDSTONE event at ledger 62,056,824 (tx
// 40758bde24a9…), captured from stellar.contract_events +
// stellar.ledger_entry_changes on r1, 2026-07-31. This is the FIRST of
// the 15 ledgers the payload-median rule provably cannot attribute: the
// op args request feed_ids [PYUSD, iBENJI_ETHEREUM_FUNDAMENTAL,
// BENJI_ETHEREUM_FUNDAMENTAL, USTRY], the adapter's freshness verifier
// kept exactly ONE feed, and the surviving price 100000000 (1.00000000
// at 8 decimals) equals BOTH BENJI twins' payload medians at the entry's
// package_timestamp — two order-preserving alignments, ErrAmbiguousSubset.
//
// The transaction's ledger-entry changes settle it: write_prices STORED
// exactly one PriceData under the adapter's contract-data key
// ScString("iBENJI_ETHEREUM_FUNDAMENTAL") (change_type=updated,
// change_index 5 — the change_index-4 `state` row is the pre-image and
// is excluded on both plumbing paths). With that key plumbed through
// events.Event.StateWriteKeys the accepted subset is exact and the
// event attributes with zero heuristics.

const stateWriteFixtureTx = "40758bde24a9afcfe5d4ed2e22fe166c3508566e38bb62619aaa2e86227c8842"

// stateWriteFixtureKey is the tx's single written adapter contract-data
// LedgerKey — ContractData{CA526Y2N…, ScString("iBENJI_ETHEREUM_FUNDAMENTAL"),
// persistent} — verbatim from stellar.ledger_entry_changes.key_xdr.
const stateWriteFixtureKey = "AAAABgAAAAE7r2NNhY1q1h+JSvMDLKe8+WE7oLTJ1BXmafczoXPOOwAAAA4AAAAbaUJFTkpJX0VUSEVSRVVNX0ZVTkRBTUVOVEFMAAAAAAE="

func stateWriteFixtureEvent() *events.Event {
	return &events.Event{
		ContractID: "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG",
		Ledger:     62056824,
		TxHash:     stateWriteFixtureTx,
		Topic:      []string{TopicSymbolRedstone},
		Value:      "AAAADQAAAPgAAAARAAAAAQAAAAIAAAAPAAAADXVwZGF0ZWRfZmVlZHMAAAAAAAAQAAAAAQAAAAEAAAARAAAAAQAAAAMAAAAPAAAAEXBhY2thZ2VfdGltZXN0YW1wAAAAAAAABQAAAZ139fEgAAAADwAAAAVwcmljZQAAAAAAAAsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfXhAAAAAA8AAAAPd3JpdGVfdGltZXN0YW1wAAAAAAUAAAGdd/YcGAAAAA8AAAAHdXBkYXRlcgAAAAASAAAAAAAAAAAk1wP6EQQ6Z6YlFesVDZAkQ3o7tjIdDJoRh0/hHzC1Bw==",
		OpArgs: []string{
			"AAAAEgAAAAAAAAAAJNcD+hEEOmemJRXrFQ2QJEN6O7YyHQyaEYdP4R8wtQc=",
			"AAAAEAAAAAEAAAAEAAAADgAAAAVQWVVTRAAAAAAAAA4AAAAbaUJFTkpJX0VUSEVSRVVNX0ZVTkRBTUVOVEFMAAAAAA4AAAAaQkVOSklfRVRIRVJFVU1fRlVOREFNRU5UQUwAAAAAAA4AAAAFVVNUUlkAAAA=",
			"AAAADQAABttQWVVTRAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAF9flmAZ139fEgAAAAIAAAAf+hkFMYm81HjMDFABoGLhdTooXTVls+MvrTjvrPX7c/Hh0oXSnHDMf8EBoJNu+2iFilRaiwf7eqGtu+xUlBk+4cUFlVU0QAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfX5ZgGdd/XxIAAAACAAAAF17wZMv6rgNKl0o4I322pBBc+g1w82i8IEtNPSF/wUIGeAkO+DXMTwTnSInj+pedV5ouQtAD7luTGZsh6MQBUMG1BZVVNEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAX19IQBnXf18SAAAAAgAAABvhmHFZcwVmALb5kVgAbvxvBmLqvRee7w+GF7j+l5zVdUgHswoGBGrOd6d1z9k+O3zqQNkyr6g6062YZwuWNiwBtpQkVOSklfRVRIRVJFVU1fRlVOREFNRU5UQUwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAF9eEAAZ139fEgAAAAIAAAARMBWcBQktAfMhXL9Lnjr1lEm3S6Yz7oh6IeJ74OBMIMAvOHEhnnhxEDo6nExLnaphcomF4Kx6jzO4dB/dgokyobaUJFTkpJX0VUSEVSRVVNX0ZVTkRBTUVOVEFMAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfXhAAGdd/XxIAAAACAAAAFnGyJYyI09RnFUQImlJS+As6nsIBV9qbVoFi9kIvP4bgWfya0pvocyGLDSXSHfxDQeRzy0eE8VCmGG9qzXXRIfHGlCRU5KSV9FVEhFUkVVTV9GVU5EQU1FTlRBTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAX14QABnXf18SAAAAAgAAABqius7l19GCsZahgcwXxcQ2tJ2mdw45n6W2Q7zxYZtWQxMyqxRY6679UjGzjRyZFroh6mogempe9ABh6Ymd7Y7RtCRU5KSV9FVEhFUkVVTV9GVU5EQU1FTlRBTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAF9eEAAZ139fEgAAAAIAAAAR2FvoFDxhz5uGxzKNCfiYMu3yxagJ2r/WaZH/IQdIHRVY6oXQrgpOilVeZJoZWOZc4+W5t3PeTr8Q9hCmffIjYcQkVOSklfRVRIRVJFVU1fRlVOREFNRU5UQUwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABfXhAAGdd/XxIAAAACAAAAHhBOyi08dcTrR9qUVLs8KCBA1ikDM7q2Xd6mUoIcHdFytHIAw9+jjlgzsVR7EDeiDXfs5XF3dFXFVIPRKayRbbG0JFTkpJX0VUSEVSRVVNX0ZVTkRBTUVOVEFMAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAX14QABnXf18SAAAAAgAAAB/0TVOKEpLW98aW0bDsvWQxeciARgxgDTarAUwXE7SZtDGFY2PBMMVW+SCihkOJuL4EzCgyYclDV69xBavLPJYxtVU1RSWQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGVIRYAZ139fEgAAAAIAAAAQ93BXp5DvrtZ3U3nLbR1rTaAZk+0QKNesUfZ3Qtqxc7apwBpYpaA1UVfPm9fQ2xPumZMidAuH92sNyIXE3LU1AcVVNUUlkAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABlSEWAGdd/XxIAAAACAAAAGxLAco/fMyYOwf4zGlwFPX5CwvFnE1/8V177MyPbOWsQu70PQsmxmN7YS2iGSG/jLoIWboYPC17xavQaYAM8mCHFVTVFJZAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAZUhFgBnXf18SAAAAAgAAABgdbOy3E0yNlgxXpTv7r5ddNfBGdmATEyv/Yk/gUvof9sCiVIG30eTVRLCu32VOOX0DocoejkY1YqLy3IDXzClxwADDE3NzU4MzQxMDg5ODEjMC45LjAjc3RlbGxhci1jb25uZWN0b3IAACUAAALtVwEeAAAA",
		},
		StateWriteKeys: []string{stateWriteFixtureKey},
	}
}

func TestDecode_SubsetFilteredBatch_AttributedViaStateWriteKeys(t *testing.T) {
	out, err := decodeWritePrices(stateWriteFixtureEvent(), time.Unix(1775834111, 0))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d updates, want 1", len(out))
	}
	u := out[0]
	wantAsset, aerr := canonical.NewRWAAsset("iBENJI")
	if aerr != nil {
		t.Fatal(aerr)
	}
	if u.Asset != wantAsset {
		t.Errorf("attributed to %s, want %s (the written key names iBENJI; BENJI shares the median)", u.Asset, wantAsset)
	}
	if u.Price.String() != "100000000" {
		t.Errorf("price = %s, want 100000000", u.Price)
	}
	if got := u.Timestamp.UnixMilli(); got != 1775834100000 {
		t.Errorf("timestamp = %d, want package_timestamp 1775834100000", got)
	}
}

// Without the state-write keys the same event MUST stay honest-blind —
// pins that the exact path is what resolves it, and that the payload
// fallback's refusal semantics are unchanged.
func TestDecode_SubsetFilteredBatch_StillAmbiguousWithoutStateWriteKeys(t *testing.T) {
	ev := stateWriteFixtureEvent()
	ev.StateWriteKeys = nil
	_, err := decodeWritePrices(ev, time.Unix(1775834111, 0))
	if !errors.Is(err, ErrAmbiguousSubset) {
		t.Fatalf("err = %v, want ErrAmbiguousSubset (both BENJI twins share the surviving median)", err)
	}
}

// A written-key set whose feed intersection disagrees with updated_feeds'
// arity must NOT be trusted — the decoder falls back to payload-median
// alignment, which refuses this event as ambiguous. Fail-safe: keys can
// cause a fallback, never a misattribution.
func TestDecode_SubsetFilteredBatch_ArityMismatchFallsBackToPayload(t *testing.T) {
	ev := stateWriteFixtureEvent()
	// Both BENJI twins' keys "written": intersection arity 2 != 1 price.
	ev.StateWriteKeys = []string{
		stateWriteFixtureKey,
		"AAAABgAAAAE7r2NNhY1q1h+JSvMDLKe8+WE7oLTJ1BXmafczoXPOOwAAAA4AAAAaQkVOSklfRVRIRVJFVU1fRlVOREFNRU5UQUwAAAAAAAE=",
	}
	_, err := decodeWritePrices(ev, time.Unix(1775834111, 0))
	if !errors.Is(err, ErrAmbiguousSubset) {
		t.Fatalf("err = %v, want ErrAmbiguousSubset via the payload fallback", err)
	}
}

// Keys owned by a DIFFERENT contract are ignored (defence-in-depth: the
// plumbing already filters by the event's contract).
func TestSubsetFromStateWrites_ForeignAndMalformedKeysIgnored(t *testing.T) {
	feedIDs := []string{"PYUSD", "iBENJI_ETHEREUM_FUNDAMENTAL", "BENJI_ETHEREUM_FUNDAMENTAL", "USTRY"}
	sub := subsetFromStateWrites(feedIDs, []string{
		"not-base64!!",       // unparseable — skipped
		stateWriteFixtureKey, // adapter's iBENJI key — kept
	}, "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG")
	if len(sub) != 1 || sub[0] != "iBENJI_ETHEREUM_FUNDAMENTAL" {
		t.Fatalf("sub = %v, want [iBENJI_ETHEREUM_FUNDAMENTAL]", sub)
	}
	// Same key, wrong contract: contributes nothing.
	if sub := subsetFromStateWrites(feedIDs, []string{stateWriteFixtureKey}, "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"); sub != nil {
		t.Fatalf("foreign-contract sub = %v, want nil", sub)
	}
}
