package discovery_test

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// validContractID is a real, valid C-strkey reused across tests.
const validContractID = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

// makeEvent builds an events.Event whose topic[0] is the SCVal
// encoding of `symbol`. Other fields default to populated stubs so
// the sniffer's preconditions pass.
func makeEvent(t *testing.T, symbol, contractID string) events.Event {
	t.Helper()
	topic0 := scval.MustEncodeSymbol(symbol)
	return events.Event{
		Type:           "contract",
		ContractID:     contractID,
		Ledger:         50_000_000,
		LedgerClosedAt: "2026-04-28T12:00:00Z",
		Topic:          []string{topic0, "ignored", "ignored"},
		Value:          "AAAAAA==", // empty body — sniffer doesn't read it
	}
}

// TestSniff_RecognisesAllFourSEP41Events — happy path: each of the
// four SEP-41 event topics maps to its enum value.
func TestSniff_RecognisesAllFourSEP41Events(t *testing.T) {
	tests := []struct {
		symbol string
		want   discovery.SEP41EventType
	}{
		{"transfer", discovery.EventTransfer},
		{"mint", discovery.EventMint},
		{"burn", discovery.EventBurn},
		{"clawback", discovery.EventClawback},
	}
	for _, tc := range tests {
		t.Run(tc.symbol, func(t *testing.T) {
			ev := makeEvent(t, tc.symbol, validContractID)
			hit, ok := discovery.Sniff(ev)
			if !ok {
				t.Fatalf("Sniff returned ok=false for %q", tc.symbol)
			}
			if hit.EventType != tc.want {
				t.Errorf("EventType = %q, want %q", hit.EventType, tc.want)
			}
			if hit.ContractID != validContractID {
				t.Errorf("ContractID = %q, want %q", hit.ContractID, validContractID)
			}
			if hit.Ledger != 50_000_000 {
				t.Errorf("Ledger = %d, want 50_000_000", hit.Ledger)
			}
			if hit.ObservedAtRFC3339 != "2026-04-28T12:00:00Z" {
				t.Errorf("ObservedAtRFC3339 = %q", hit.ObservedAtRFC3339)
			}
		})
	}
}

// TestSniff_RejectsNonSEP41Symbols — DEX-specific symbols (swap,
// sync, deposit, …) and arbitrary unknown symbols MUST NOT trip the
// sniffer. Without this guard discovery would record every
// contract-event-emitting contract on pubnet.
func TestSniff_RejectsNonSEP41Symbols(t *testing.T) {
	for _, sym := range []string{"swap", "sync", "deposit", "withdraw", "rebalance", "REDSTONE", "POOL"} {
		t.Run(sym, func(t *testing.T) {
			ev := makeEvent(t, sym, validContractID)
			if _, ok := discovery.Sniff(ev); ok {
				t.Errorf("Sniff returned ok=true for non-SEP-41 symbol %q", sym)
			}
		})
	}
}

// TestSniff_RejectsNonContractEvents — system + diagnostic events
// have the wrong wire shape; skip them at the type check before
// SCVal parsing.
func TestSniff_RejectsNonContractEvents(t *testing.T) {
	for _, eventType := range []string{"system", "diagnostic", ""} {
		t.Run("type="+eventType, func(t *testing.T) {
			ev := makeEvent(t, "transfer", validContractID)
			ev.Type = eventType
			if _, ok := discovery.Sniff(ev); ok {
				t.Errorf("Sniff accepted Type=%q event", eventType)
			}
		})
	}
}

// TestSniff_RejectsEmptyContractID — defensive: an event without a
// contract id is malformed; surface as ok=false so the recorder
// never sees an empty key.
func TestSniff_RejectsEmptyContractID(t *testing.T) {
	ev := makeEvent(t, "transfer", validContractID)
	ev.ContractID = ""
	if _, ok := discovery.Sniff(ev); ok {
		t.Error("Sniff accepted event with empty ContractID")
	}
}

// TestSniff_RejectsEmptyTopic — no topic = nothing to classify; the
// sniffer must not nil-index Topic[0].
func TestSniff_RejectsEmptyTopic(t *testing.T) {
	ev := makeEvent(t, "transfer", validContractID)
	ev.Topic = nil
	if _, ok := discovery.Sniff(ev); ok {
		t.Error("Sniff accepted event with nil Topic")
	}
	ev.Topic = []string{}
	if _, ok := discovery.Sniff(ev); ok {
		t.Error("Sniff accepted event with empty Topic")
	}
}

// TestSniff_RejectsMalformedTopic0 — a topic[0] that doesn't decode
// as SCVal symbol falls through with ok=false.
func TestSniff_RejectsMalformedTopic0(t *testing.T) {
	ev := makeEvent(t, "transfer", validContractID)
	ev.Topic[0] = "not-base64!!!"
	if _, ok := discovery.Sniff(ev); ok {
		t.Error("Sniff accepted event with unparseable topic[0]")
	}
}

// TestInMemoryRecorder_RoundTrip — Record then IsKnown returns true;
// a never-seen contract returns false.
func TestInMemoryRecorder_RoundTrip(t *testing.T) {
	r := discovery.NewInMemoryRecorder()
	ctx := context.Background()

	// Unknown contract first.
	known, err := r.IsKnown(ctx, validContractID)
	if err != nil {
		t.Fatalf("IsKnown: %v", err)
	}
	if known {
		t.Error("IsKnown=true for never-recorded contract")
	}

	hit := discovery.Hit{
		ContractID:        validContractID,
		EventType:         discovery.EventTransfer,
		Ledger:            50_000_000,
		ObservedAtRFC3339: "2026-04-28T12:00:00Z",
	}
	if err := r.Record(ctx, hit); err != nil {
		t.Fatalf("Record: %v", err)
	}

	known, _ = r.IsKnown(ctx, validContractID)
	if !known {
		t.Error("IsKnown=false after Record")
	}
}

// TestInMemoryRecorder_FirstWriteWinsOnMetadata — the first Record
// for a contract preserves first_seen_event + first_seen_ledger;
// subsequent Records only bump the counter. Mirrors the Postgres
// ON CONFLICT DO UPDATE pattern.
func TestInMemoryRecorder_FirstWriteWinsOnMetadata(t *testing.T) {
	r := discovery.NewInMemoryRecorder()
	ctx := context.Background()

	first := discovery.Hit{
		ContractID:        validContractID,
		EventType:         discovery.EventMint,
		Ledger:            10_000_000,
		ObservedAtRFC3339: "2026-01-01T00:00:00Z",
	}
	second := discovery.Hit{
		ContractID:        validContractID,
		EventType:         discovery.EventTransfer, // different event
		Ledger:            20_000_000,              // later ledger
		ObservedAtRFC3339: "2026-04-28T12:00:00Z",
	}
	if err := r.Record(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(ctx, second); err != nil {
		t.Fatal(err)
	}

	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot returned %d entries, want 1 (de-duped on contract id)", len(snap))
	}
	if snap[0].EventType != discovery.EventMint {
		t.Errorf("EventType = %q, want %q (first-write-wins)", snap[0].EventType, discovery.EventMint)
	}
	if snap[0].Ledger != 10_000_000 {
		t.Errorf("Ledger = %d, want 10_000_000 (first-write-wins)", snap[0].Ledger)
	}
	if got := r.Count(validContractID); got != 2 {
		t.Errorf("Count = %d, want 2 (both Records counted)", got)
	}
}

// TestInMemoryRecorder_RejectsEmptyContractID — guard against a
// malformed Hit reaching the recorder (would otherwise allocate an
// "" key in the map).
func TestInMemoryRecorder_RejectsEmptyContractID(t *testing.T) {
	r := discovery.NewInMemoryRecorder()
	if err := r.Record(context.Background(), discovery.Hit{}); err == nil {
		t.Error("expected error on empty-ContractID Hit; got nil")
	}
}

// ─── SniffOracleEvent ──────────────────────────────────────────────
//
// Exercises the broader oracle-suggestive topic[0] sniffer added per
// docs/architecture/generic-oracle-sep-onboarding.md §3(b)(1).

// TestSniffOracleEvent_RecognisesEveryOracleSuggestiveSymbol — the
// exact symbol set from the investigation's 2026-07-10 ClickHouse
// census must all trip SniffOracleEvent with Kind=KindOracleEvent
// and Symbol == the matched string.
func TestSniffOracleEvent_RecognisesEveryOracleSuggestiveSymbol(t *testing.T) {
	symbols := []string{
		"price", "prices", "lastprice", "last_price", "x_last_price",
		"set_price", "update_price", "price_update", "new_price",
		"oracle", "Oracle", "ORACLE", "feed", "PriceData", "resolution",
		"write_prices", "relay", "force_relay", "REFLECTOR", "REDSTONE",
		"rate", "rates", "set_rate", "symbol_rates", "StandardReference",
		"update", "base", "decimals", "assets",
	}
	for _, sym := range symbols {
		t.Run(sym, func(t *testing.T) {
			ev := makeEvent(t, sym, validContractID)
			hit, ok := discovery.SniffOracleEvent(ev)
			if !ok {
				t.Fatalf("SniffOracleEvent returned ok=false for %q", sym)
			}
			if hit.Kind != discovery.KindOracleEvent {
				t.Errorf("Kind = %q, want %q", hit.Kind, discovery.KindOracleEvent)
			}
			if hit.Symbol != sym {
				t.Errorf("Symbol = %q, want %q", hit.Symbol, sym)
			}
			if hit.ContractID != validContractID {
				t.Errorf("ContractID = %q, want %q", hit.ContractID, validContractID)
			}
			if hit.Ledger != 50_000_000 {
				t.Errorf("Ledger = %d, want 50_000_000", hit.Ledger)
			}
			if hit.ObservedAtRFC3339 != "2026-04-28T12:00:00Z" {
				t.Errorf("ObservedAtRFC3339 = %q", hit.ObservedAtRFC3339)
			}
		})
	}
}

// TestSniffOracleEvent_RejectsNonOracleSymbols — SEP-41 symbols
// (transfer/mint/burn/clawback) and arbitrary DEX symbols must NOT
// trip the oracle-event sniffer. This is the negative case proving
// the two event-path sniffers are disjoint.
func TestSniffOracleEvent_RejectsNonOracleSymbols(t *testing.T) {
	for _, sym := range []string{"transfer", "mint", "burn", "clawback", "swap", "sync", "deposit", "withdraw", "rebalance", "POOL"} {
		t.Run(sym, func(t *testing.T) {
			ev := makeEvent(t, sym, validContractID)
			if _, ok := discovery.SniffOracleEvent(ev); ok {
				t.Errorf("SniffOracleEvent returned ok=true for non-oracle symbol %q", sym)
			}
		})
	}
}

// TestSniffOracleEvent_RejectsNonContractEvents — same defensive
// guard as Sniff: non-contract event types never match.
func TestSniffOracleEvent_RejectsNonContractEvents(t *testing.T) {
	for _, eventType := range []string{"system", "diagnostic", ""} {
		t.Run("type="+eventType, func(t *testing.T) {
			ev := makeEvent(t, "oracle", validContractID)
			ev.Type = eventType
			if _, ok := discovery.SniffOracleEvent(ev); ok {
				t.Errorf("SniffOracleEvent accepted Type=%q event", eventType)
			}
		})
	}
}

// TestSniffOracleEvent_RejectsEmptyContractIDOrTopic — defensive
// guards mirror Sniff's.
func TestSniffOracleEvent_RejectsEmptyContractIDOrTopic(t *testing.T) {
	ev := makeEvent(t, "oracle", validContractID)
	ev.ContractID = ""
	if _, ok := discovery.SniffOracleEvent(ev); ok {
		t.Error("SniffOracleEvent accepted event with empty ContractID")
	}

	ev2 := makeEvent(t, "oracle", validContractID)
	ev2.Topic = nil
	if _, ok := discovery.SniffOracleEvent(ev2); ok {
		t.Error("SniffOracleEvent accepted event with nil Topic")
	}
}

// ─── SniffOracleCall ───────────────────────────────────────────────
//
// Exercises the event-less-oracle ContractCallContext-path sniffer
// added per docs/architecture/generic-oracle-sep-onboarding.md
// §3(b)(2) — the Band pattern generalized.

// TestSniffOracleCall_RecognisesEveryCallCandidate — the exact
// function-name allow-list the investigation named for the call
// path.
func TestSniffOracleCall_RecognisesEveryCallCandidate(t *testing.T) {
	fns := []string{"lastprice", "price", "prices", "relay", "force_relay", "write_prices", "x_last_price"}
	for _, fn := range fns {
		t.Run(fn, func(t *testing.T) {
			hit, ok := discovery.SniffOracleCall(discovery.OracleCallInput{
				ContractID:        validContractID,
				FunctionName:      fn,
				Ledger:            50_000_000,
				ObservedAtRFC3339: "2026-04-28T12:00:00Z",
			})
			if !ok {
				t.Fatalf("SniffOracleCall returned ok=false for %q", fn)
			}
			if hit.Kind != discovery.KindOracleCall {
				t.Errorf("Kind = %q, want %q", hit.Kind, discovery.KindOracleCall)
			}
			if hit.Symbol != fn {
				t.Errorf("Symbol = %q, want %q", hit.Symbol, fn)
			}
			if hit.ContractID != validContractID {
				t.Errorf("ContractID = %q, want %q", hit.ContractID, validContractID)
			}
			if hit.Ledger != 50_000_000 {
				t.Errorf("Ledger = %d, want 50_000_000", hit.Ledger)
			}
			if hit.ObservedAtRFC3339 != "2026-04-28T12:00:00Z" {
				t.Errorf("ObservedAtRFC3339 = %q", hit.ObservedAtRFC3339)
			}
		})
	}
}

// TestSniffOracleCall_RejectsNonOracleFunctionNames — negative case:
// DEX / unrelated function names (swap, transfer, deposit, …) must
// NOT trip the call-path sniffer.
func TestSniffOracleCall_RejectsNonOracleFunctionNames(t *testing.T) {
	for _, fn := range []string{"swap", "transfer", "deposit", "withdraw", "mint", "claim", ""} {
		t.Run("fn="+fn, func(t *testing.T) {
			_, ok := discovery.SniffOracleCall(discovery.OracleCallInput{
				ContractID:   validContractID,
				FunctionName: fn,
			})
			if ok {
				t.Errorf("SniffOracleCall returned ok=true for non-oracle function %q", fn)
			}
		})
	}
}

// TestSniffOracleCall_RejectsEmptyContractID — defensive: a call
// without a resolvable contract id is malformed; surface as
// ok=false so the recorder never sees an empty key.
func TestSniffOracleCall_RejectsEmptyContractID(t *testing.T) {
	_, ok := discovery.SniffOracleCall(discovery.OracleCallInput{
		ContractID:   "",
		FunctionName: "relay",
	})
	if ok {
		t.Error("SniffOracleCall accepted empty ContractID")
	}
}

// ─── Sniff (KindSEP41) now also sets Kind + Symbol ────────────────

// TestSniff_SetsKindAndSymbol — the broadened Hit shape: Sniff must
// still classify exactly as before AND now also stamp
// Kind=KindSEP41 and Symbol==string(EventType), so a single Recorder
// code path can read "what got sighted" without branching.
func TestSniff_SetsKindAndSymbol(t *testing.T) {
	ev := makeEvent(t, "mint", validContractID)
	hit, ok := discovery.Sniff(ev)
	if !ok {
		t.Fatal("Sniff returned ok=false")
	}
	if hit.Kind != discovery.KindSEP41 {
		t.Errorf("Kind = %q, want %q", hit.Kind, discovery.KindSEP41)
	}
	if hit.Symbol != string(discovery.EventMint) {
		t.Errorf("Symbol = %q, want %q", hit.Symbol, discovery.EventMint)
	}
}
