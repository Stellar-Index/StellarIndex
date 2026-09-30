package diagnostics

import (
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// Base64 ScVal XDR of Symbol("transfer") and Symbol("swap") as Hubble
// stores them in history_contract_events.topics; the first is copied
// from a real row.
const (
	transferSymbolXDR = "AAAADwAAAAh0cmFuc2Zlcg=="
	swapSymbolXDR     = "AAAADwAAAARzd2Fw"
)

func mustBuildSorobanEventsQuery(t *testing.T, contracts []string, topic0, topic1 string) (string, []bigquery.QueryParameter) {
	t.Helper()
	q, params, err := buildSorobanEventsQuery(100, 200, contracts, topic0, topic1)
	if err != nil {
		t.Fatalf("buildSorobanEventsQuery: %v", err)
	}
	return q, params
}

func boundValues(params []bigquery.QueryParameter, name string) []string {
	for _, p := range params {
		if p.Name == name {
			v, _ := p.Value.([]string)
			return v
		}
	}
	return nil
}

// TestBuildSorobanEventsQuery_NoTopicFilter exercises the SQL
// generator with the minimum required inputs (range + contracts,
// no topic filters). Confirms parameter binding and the absence
// of optional WHERE clauses.
func TestBuildSorobanEventsQuery_NoTopicFilter(t *testing.T) {
	q, params := mustBuildSorobanEventsQuery(t, []string{"CONTRACT_A", "CONTRACT_B"}, "", "")

	if !strings.Contains(q, "ledger_sequence BETWEEN @from AND @to") {
		t.Errorf("query missing range predicate: %s", q)
	}
	if !strings.Contains(q, "contract_id IN UNNEST(@contracts)") {
		t.Errorf("query missing contract filter: %s", q)
	}
	if strings.Contains(q, "topics") || strings.Contains(q, "topic_1") || strings.Contains(q, "topic_2") {
		t.Errorf("query has topic filter despite empty filter args: %s", q)
	}
	// The caller keys its map on ledger alone; any wider GROUP BY key
	// (closed_at was one) yields several rows per ledger that overwrite.
	if !strings.Contains(q, "GROUP BY ledger_sequence ORDER BY") {
		t.Errorf("query must GROUP BY ledger_sequence alone: %s", q)
	}
	if strings.Contains(q, "closed_at") {
		t.Errorf("query selects or groups by closed_at, which the consumer drops: %s", q)
	}

	// Parameters: @from, @to, @contracts only.
	if len(params) != 3 {
		t.Errorf("expected 3 params, got %d: %+v", len(params), params)
	}
}

// TestBuildSorobanEventsQuery_WithTopic0 confirms that supplying
// topic0 adds a single AND clause + one extra parameter. We bind
// to @topic0 (named) so an injection-shaped contract ID can't
// rewrite the SQL — defence in depth even though the filter values
// are operator-supplied. history_contract_events has no topic_N
// columns; the filter must index the real `topics` JSON array.
func TestBuildSorobanEventsQuery_WithTopic0(t *testing.T) {
	q, params := mustBuildSorobanEventsQuery(t, []string{"X"}, "swap", "")

	if !strings.Contains(q, "JSON_VALUE(topics, '$[0]') IN UNNEST(@topic0)") {
		t.Errorf("query missing topic[0] filter: %s", q)
	}
	if strings.Contains(q, "$[1]") || strings.Contains(q, "@topic1") {
		t.Errorf("query has topic[1] filter despite empty: %s", q)
	}
	if strings.Contains(q, "topic_1") || strings.Contains(q, "topic_2") {
		t.Errorf("query references nonexistent topic_N columns: %s", q)
	}
	if len(params) != 4 {
		t.Errorf("expected 4 params (from/to/contracts/topic0), got %d", len(params))
	}
	if got := boundValues(params, "topic0"); !slices.Contains(got, swapSymbolXDR) {
		t.Errorf("topic0 not bound to Symbol(\"swap\") XDR %q: %+v", swapSymbolXDR, params)
	}
}

// TestBuildSorobanEventsQuery_WithBothTopics exercises the
// Phoenix-style filter: topic[0]+topic[1] both supplied. Both
// AND clauses should appear; both parameters should bind.
func TestBuildSorobanEventsQuery_WithBothTopics(t *testing.T) {
	q, params := mustBuildSorobanEventsQuery(t, []string{"X"}, "swap", "offer_amount")

	if !strings.Contains(q, "JSON_VALUE(topics, '$[0]') IN UNNEST(@topic0)") {
		t.Errorf("query missing topic[0] filter: %s", q)
	}
	if !strings.Contains(q, "JSON_VALUE(topics, '$[1]') IN UNNEST(@topic1)") {
		t.Errorf("query missing topic[1] filter: %s", q)
	}
	if len(params) != 5 {
		t.Errorf("expected 5 params, got %d: %+v", len(params), params)
	}
	if len(boundValues(params, "topic1")) == 0 {
		t.Errorf("topic1 parameter not bound: %+v", params)
	}
}

// TestTopicFilterEncodings pins the encoding against a real Hubble row and
// checks both ScVal types are matched: Soroswap's topic[0] is a String.
func TestTopicFilterEncodings(t *testing.T) {
	got, err := topicFilterEncodings("transfer")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, transferSymbolXDR) {
		t.Errorf("Symbol(\"transfer\") XDR %q missing from %v", transferSymbolXDR, got)
	}
	if len(got) != 2 {
		t.Errorf("want Symbol + String encodings, got %v", got)
	}

	const pairStringXDR = "AAAADgAAAAxTb3Jvc3dhcFBhaXI=" // String("SoroswapPair")
	got, err = topicFilterEncodings("SoroswapPair")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, pairStringXDR) {
		t.Errorf("String(\"SoroswapPair\") XDR %q missing from %v", pairStringXDR, got)
	}

	// Longer than SCSYMBOL_LIMIT: can only be a String.
	got, err = topicFilterEncodings(strings.Repeat("a", 33))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("over-limit topic should encode only as String, got %v", got)
	}
}

// TestHubbleSorobanEvents_FlagValidation locks down the argv
// guards. We don't load config or hit BigQuery here — same shape
// as TestHubbleCheck_FlagValidation; consistency in failure modes
// across the two subcommands matters because operators write
// scripts that wrap both.
func TestHubbleSorobanEvents_FlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing-from", []string{"-to", "200", "-bigquery-project", "p", "-contracts", "X"}, "-from must be > 0"},
		{"to-equals-from", []string{"-from", "100", "-to", "100", "-bigquery-project", "p", "-contracts", "X"}, "must be > -from"},
		{"missing-project", []string{"-from", "100", "-to", "200", "-contracts", "X"}, "-bigquery-project required"},
		{"missing-contracts", []string{"-from", "100", "-to", "200", "-bigquery-project", "p"}, "-contracts required"},
		{"empty-contracts", []string{"-from", "100", "-to", "200", "-bigquery-project", "p", "-contracts", ", , ,"}, "empty list"},
		{"bad-output-format", []string{"-from", "100", "-to", "200", "-bigquery-project", "p", "-contracts", "X", "-output", "yaml"}, "json|total|csv"},
		{"uncapped-bytes", []string{"-from", "100", "-to", "200", "-bigquery-project", "p", "-contracts", "X", "-max-bytes-billed", "0"}, "-max-bytes-billed must be > 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := hubbleSorobanEvents(tc.args)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}
