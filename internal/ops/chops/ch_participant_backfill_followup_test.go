package chops

import (
	"strings"
	"testing"
)

func TestParticipantBackfillFollowUp_NamesTheRangeAndTheStep(t *testing.T) {
	got := participantBackfillFollowUp(2, 61_000_000)
	for _, want := range []string{"FOLLOW-UP OWED", "2..61000000", "account_op_types.sql", "Step 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("follow-up line %q does not name %q", got, want)
		}
	}
}
