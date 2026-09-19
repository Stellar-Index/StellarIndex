package clickhouse

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// TestTopic0Predicate_MatchesBothTopicEncodings is the fast default-suite
// guard for the lake half of F048 (the executing proof against a real
// ClickHouse is
// test/integration/contract_events_string_topic_prefilter_test.go).
//
// extract.go fills topic_0_sym from Topics[0].GetSym() — Symbol only — so the
// column is EMPTY for an ScvString topic[0]. A prefilter of
// `topic_0_sym IN (…)` alone therefore matched ZERO rows for phoenix's
// ("create","liquidity_pool") announcement, and every consumer keyed on
// creationSym (seed-protocol-contracts, the -ch gatedPrefilter walk) walked a
// lake full of those events and admitted nothing.
func TestTopic0Predicate_MatchesBothTopicEncodings(t *testing.T) {
	t.Parallel()
	pred := topic0Predicate([]string{"create"})

	// The Symbol arm every other gated source depends on must survive.
	if !strings.Contains(pred, "topic_0_sym IN ('create')") {
		t.Errorf("predicate dropped the topic_0_sym arm: %s", pred)
	}
	// The String arm must match the exact XDR blob the lake stores.
	wantB64 := scval.MustEncodeString("create")
	if !strings.Contains(pred, "topics_xdr[1] IN ('"+wantB64+"')") {
		t.Errorf("predicate does not match ScvString(\"create\") = %q in topics_xdr[1]: %s",
			wantB64, pred)
	}
	// ClickHouse arrays are 1-indexed; [0] would be an error, not topic[0].
	if strings.Contains(pred, "topics_xdr[0]") {
		t.Errorf("topics_xdr is 1-indexed in ClickHouse; [0] is not topic[0]: %s", pred)
	}
	// The two arms must be OR-ed inside their own parentheses, or the
	// surrounding `AND` binds tighter than the OR and the contract_id /
	// ledger-range predicates are silently voided by a matching topic.
	if !strings.HasPrefix(pred, "(") || !strings.HasSuffix(pred, ")") {
		t.Errorf("predicate must be parenthesised so a preceding AND binds correctly: %s", pred)
	}
	if !strings.Contains(pred, " OR ") {
		t.Errorf("predicate must accept EITHER encoding: %s", pred)
	}
}

// TestTopic0Predicate_SymbolOnlyNamesStillFilter pins that the widening does
// not turn the prefilter into a pass-through: a name that is not requested
// must not appear, in either encoding.
func TestTopic0Predicate_SymbolOnlyNamesStillFilter(t *testing.T) {
	t.Parallel()
	pred := topic0Predicate([]string{"deploy", "add_pool"})
	for _, want := range []string{
		"'deploy'", "'add_pool'",
		"'" + scval.MustEncodeString("deploy") + "'",
		"'" + scval.MustEncodeString("add_pool") + "'",
	} {
		if !strings.Contains(pred, want) {
			t.Errorf("predicate is missing %s: %s", want, pred)
		}
	}
	for _, unwanted := range []string{"'create'", "'transfer'"} {
		if strings.Contains(pred, unwanted) {
			t.Errorf("predicate admits unrequested name %s: %s", unwanted, pred)
		}
	}
}
