package v1

import "testing"

// TestRWAContractCandidateDrops_AnUnknownReasonIsNotAnOperatorDecision: a refusal reason outside the known order is still
// counted, under the definition that emitted it rather than as something an operator chose.
func TestRWAContractCandidateDrops_AnUnknownReasonIsNotAnOperatorDecision(t *testing.T) {
	drops := rwaContractCandidateDrops(rwaMembership{contractRefusals: map[string]int{"some_future_reason": 2}})
	if len(drops) != 1 || drops[0].Reason != "some_future_reason" || drops[0].Count != 2 {
		t.Fatalf("drops = %+v, want the unknown reason counted once", drops)
	}
	if drops[0].Actor != rwaActorDefinition {
		t.Errorf("actor = %q, want %q", drops[0].Actor, rwaActorDefinition)
	}
}
