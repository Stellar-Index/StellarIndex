package defindex

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Golden real-lake decode-VALUE tests.
//
// CONVENTION (shared with aquarius and soroswap_router): a golden test captures the EXACT
// on-chain bytes of one real event (topics_xdr + data_xdr, base64 as stored in
// stellar.contract_events), builds the production events.Event, drives the REAL decoder through
// its two production seams
//
//	Decoder.Matches(ev)   // ADR-0035 contract-identity gate
//	Decoder.Decode(ev)    // classify + body decode + consumer.Event wrap
//
// and asserts the EXACT decoded FIELD VALUES. ADR-0033's recognition axis (does Matches claim
// it?) and projection axis (does the row COUNT match?) both miss wrong values: a decoder can
// Match and emit the right number of rows with WRONG amounts, addresses or directions.
//
// Rules for a fixture here:
//   - REAL bytes only, citing the ledger_seq, tx_hash, op_index and event_index it came from.
//   - Drive the production Decoder (NewDecoder()), not decodeFlow/decodeAdminEvent: the value is
//     proving the WHOLE path.
//   - Assert exact values (strkeys, amount strings, hex hashes) and prove non-vacuousness:
//     mutating any pinned constant must fail the test (see the mutation-guard note on each).

// TestGolden_defindexStrategyHarvest_ledger57485721 pins the decode of a real BlendStrategy
// `harvest` event. Real harvests carry a decodeFlow-compatible body, and dropping them
// under-counts vault NAV by the full harvested yield.
//
// Real bytes from r1's ClickHouse lake (stellar.contract_events):
//
//	ledger_seq   57,485,721
//	tx_hash      bb84120494620c8145d21bcbf031689bd8cb67f8ccc0924ea386f3add291b9e3
//	op_index 0, event_index 2
//	contract_id  CDPWNUW7… (a curated MainnetStrategy → passes the gate)
//	topics       [ScvString("BlendStrategy"), ScvSymbol("harvest")]
//	data         Map{ amount: i128=2, from: Address(G…), price_per_share: i128=999999999975 }
//
// This is a genuine DUST harvest (amount = 2). It also exercises the decode-by-name contract: the
// third field `price_per_share` is NOT read by decodeFlow and must be ignored, not mistaken for
// `amount`. Amount is pinned to 2, not the far larger price_per_share, which is what an
// arity/positional bug would get wrong.
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
	// amount: the DUST value 2, decoded by name, not price_per_share. Mutating wantAmount or wantFrom must fail this test.
	if got := fe.Flow.Amount.String(); got != wantAmount {
		t.Errorf("Amount = %q, want %q (decode-by-name must ignore price_per_share)", got, wantAmount)
	}
	// header fields survive the whole path.
	if fe.Flow.Ledger != 57485721 || fe.Flow.TxHash != ev.TxHash {
		t.Errorf("header not preserved: ledger=%d tx=%q", fe.Flow.Ledger, fe.Flow.TxHash)
	}
}
