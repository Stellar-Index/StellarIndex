package defindex

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── Golden real-lake decode-VALUE tests (DAT-15) ───────────────────
//
// CONVENTION (shared with internal/sources/aquarius/golden_realbytes_test.go
// and mirrored on soroswap_router/real_bytes_test.go):
//
// A golden real-lake test captures the EXACT on-chain bytes of one
// real event (topics_xdr + data_xdr, base64 as the r1 ClickHouse lake
// stores them in stellar.contract_events), builds the production
// events.Event from them, drives the REAL decoder end-to-end through
// its two production seams —
//
//	Decoder.Matches(ev)   // ADR-0035 contract-identity gate
//	Decoder.Decode(ev)    // classify + body decode + consumer.Event wrap
//
// — and asserts the EXACT decoded FIELD VALUES of the emitted
// consumer.Event. This closes the gap ADR-0033's recognition axis
// (does Matches claim it?) and projection axis (does the row COUNT
// match?) both miss: neither checks that the decoded field VALUES
// (amounts, addresses, directions) are correct. A decoder can Match an
// event and emit the right NUMBER of rows while putting WRONG values in
// them, and both axes still pass. Only a golden value assertion catches
// that.
//
// Rules for a fixture here:
//   - REAL bytes only. Each fixture cites the ledger_seq, tx_hash,
//     op_index, event_index it was captured from, so it can be re-pulled
//     and re-verified against the lake. No hand-encoded SCVals.
//   - Drive the production Decoder (NewDecoder()), NOT the internal
//     decodeFlow/decodeAdminEvent helpers — those are already covered by
//     decode_test.go / decode_admin_test.go. The value here is proving
//     the WHOLE path (gate + classify + decode + wrap) agrees with the
//     real chain.
//   - Assert exact values (strkeys, amount strings, hex hashes), and
//     prove non-vacuousness: mutating any pinned constant must fail the
//     test (see the mutation-guard note on each test).

// TestGolden_defindexStrategyHarvest_ledger57485721 pins the decode of a
// real BlendStrategy `harvest` event (the just-validated #91 surface:
// harvest was recognise-and-dropped until audit 2026-08-04 finding 4
// proved 1,018 real harvests carry a decodeFlow-compatible body, whose
// omission under-counted vault NAV by the full harvested yield).
//
// Real bytes captured from r1's ClickHouse lake (stellar.contract_events)
// 2026-08-17:
//
//	ledger_seq   57,485,721  (closed 2025-06-10 19:26:07 UTC)
//	tx_hash      bb84120494620c8145d21bcbf031689bd8cb67f8ccc0924ea386f3add291b9e3
//	op_index 0, event_index 2
//	contract_id  CDPWNUW7… (a curated MainnetStrategy → passes the gate)
//	topics       [ScvString("BlendStrategy"), ScvSymbol("harvest")]
//	data         Map{ amount: i128=2, from: Address(G…), price_per_share: i128=999999999975 }
//
// This is a genuine DUST harvest (amount = 2). It also exercises the
// decode-by-name contract: the body's third field `price_per_share` is
// NOT read by decodeFlow, and the decode must ignore it (not choke on
// the extra field, not confuse it for `amount`). The value pinned for
// Amount is 2 — NOT the far larger price_per_share — which is exactly
// what an arity/positional bug would get wrong.
func TestGolden_defindexStrategyHarvest_ledger57485721(t *testing.T) {
	t.Parallel()

	const (
		wantFrom   = "GC7LSNNCRW4PNLLHIXE53RB7N5BKSQVXTGWYHIYBA7LAWY7XYPC4JGCP"
		wantAmount = "2"
		wantKind   = "defindex.strategy.harvest"
	)

	ev := events.Event{
		Type:           "contract",
		ContractID:     "CDPWNUW7UMCSVO36VAJSQHQECISPJLCVPDASKHRC5SEROAAZDUQ5DG2Z",
		Ledger:         57485721,
		LedgerClosedAt: "2025-06-10T19:26:07Z",
		TxHash:         "bb84120494620c8145d21bcbf031689bd8cb67f8ccc0924ea386f3add291b9e3",
		OperationIndex: 0,
		EventIndex:     2,
		Topic: []string{
			"AAAADgAAAA1CbGVuZFN0cmF0ZWd5AAAA", // ScvString("BlendStrategy")
			"AAAADwAAAAdoYXJ2ZXN0AA==",         // ScvSymbol("harvest")
		},
		Value: "AAAAEQAAAAEAAAADAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAAAAAAACAAAADwAAAARmcm9tAAAAEgAAAAAAAAAAvrk1oo249q1nRcndxD9vQqlCt5mtg6MBB9YLY/fDxcQAAAAPAAAAD3ByaWNlX3Blcl9zaGFyZQAAAAAKAAAAAAAAAAAAAADo1dz95w==",
	}

	// Production seam 1: the ADR-0035 gate must admit this curated
	// strategy for its own BlendStrategy flow topic.
	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatal("Matches(real harvest) = false — a curated MainnetStrategy's own harvest must pass the gate")
	}

	// Production seam 2: classify + decode + consumer.Event wrap.
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode(real harvest) err = %v, want a decoded flow", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want exactly 1", len(out))
	}
	fe, ok := out[0].(Event)
	if !ok {
		t.Fatalf("Decode emitted %T, want defindex.Event", out[0])
	}

	// layer = strategy AND direction = harvest, in one EventKind pin.
	if got := fe.EventKind(); got != wantKind {
		t.Errorf("EventKind = %q, want %q (layer+direction)", got, wantKind)
	}
	if fe.Flow.Direction != DirectionHarvest {
		t.Errorf("Direction = %q, want %q", fe.Flow.Direction, DirectionHarvest)
	}
	// actor: the strategy `from` (a G-strkey here — the keeper/harvester).
	if fe.Flow.From != wantFrom {
		t.Errorf("From = %q, want %q", fe.Flow.From, wantFrom)
	}
	// amount: the DUST value 2, decoded-by-name — NOT price_per_share.
	// Mutation guard (non-vacuous): change wantAmount to "3" (or wantFrom
	// by one char) and this test FAILS — the value is pinned, not merely
	// asserted non-empty. Demonstrated by the DAT-15 build.
	if got := fe.Flow.Amount.String(); got != wantAmount {
		t.Errorf("Amount = %q, want %q (decode-by-name must ignore price_per_share)", got, wantAmount)
	}
	// header fields survive the whole path.
	if fe.Flow.Ledger != 57485721 || fe.Flow.TxHash != ev.TxHash {
		t.Errorf("header not preserved: ledger=%d tx=%q", fe.Flow.Ledger, fe.Flow.TxHash)
	}
}
