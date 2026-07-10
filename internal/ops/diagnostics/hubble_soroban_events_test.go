package diagnostics

import (
	"strings"
	"testing"
)

// TestBuildSorobanEventsQuery_NoTopicFilter exercises the SQL
// generator with the minimum required inputs (range + contracts,
// no topic filters). Confirms parameter binding and the absence
// of optional WHERE clauses.
func TestBuildSorobanEventsQuery_NoTopicFilter(t *testing.T) {
	q, params := buildSorobanEventsQuery(100, 200, []string{"CONTRACT_A", "CONTRACT_B"}, "", "")

	if !strings.Contains(q, "ledger_sequence BETWEEN @from AND @to") {
		t.Errorf("query missing range predicate: %s", q)
	}
	if !strings.Contains(q, "contract_id IN UNNEST(@contracts)") {
		t.Errorf("query missing contract filter: %s", q)
	}
	if strings.Contains(q, "topic_1 =") || strings.Contains(q, "topic_2 =") {
		t.Errorf("query has topic filter despite empty filter args: %s", q)
	}
	if !strings.Contains(q, "GROUP BY closed_at, ledger_sequence") {
		t.Errorf("query missing GROUP BY: %s", q)
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
// are operator-supplied.
func TestBuildSorobanEventsQuery_WithTopic0(t *testing.T) {
	q, params := buildSorobanEventsQuery(100, 200, []string{"X"}, "swap", "")

	if !strings.Contains(q, "topic_1 = @topic0") {
		t.Errorf("query missing topic[0] filter: %s", q)
	}
	if strings.Contains(q, "topic_2 =") {
		t.Errorf("query has topic[1] filter despite empty: %s", q)
	}
	if len(params) != 4 {
		t.Errorf("expected 4 params (from/to/contracts/topic0), got %d", len(params))
	}
	// Find the topic0 parameter by name.
	var topic0Bound bool
	for _, p := range params {
		if p.Name == "topic0" && p.Value == "swap" {
			topic0Bound = true
		}
	}
	if !topic0Bound {
		t.Errorf("topic0 parameter not bound to expected value 'swap': %+v", params)
	}
}

// TestBuildSorobanEventsQuery_WithBothTopics exercises the
// Phoenix-style filter: topic[0]+topic[1] both supplied. Both
// AND clauses should appear; both parameters should bind.
func TestBuildSorobanEventsQuery_WithBothTopics(t *testing.T) {
	q, params := buildSorobanEventsQuery(100, 200, []string{"X"}, "swap", "offer_amount")

	if !strings.Contains(q, "topic_1 = @topic0") {
		t.Errorf("query missing topic[0] filter: %s", q)
	}
	if !strings.Contains(q, "topic_2 = @topic1") {
		t.Errorf("query missing topic[1] filter: %s", q)
	}
	if len(params) != 5 {
		t.Errorf("expected 5 params, got %d: %+v", len(params), params)
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
