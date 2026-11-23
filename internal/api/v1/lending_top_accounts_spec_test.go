package v1

import (
	"slices"
	"testing"
)

// The top-accounts route's exclusion reasons are the union of its own constants and the ones
// it passes through from the earnings USD and rate helpers; the schema must declare exactly them.
func TestTopAccountsExclusionReasonEnumMatchesHandler(t *testing.T) {
	want := []string{
		topAccountsReasonTruncated,
		topAccountsReasonUnreadable,
		topAccountsReasonList,
		topAccountsReasonOvercounted,
		earningsReasonRateUnread,
		earningsUSDWithheld,
		earningsUSDUnpriced,
	}
	got := specEnumAt(t, loadSpecDoc(t), "components", "schemas", "LendingTopAccountsExclusion", "properties", "reason")
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("LendingTopAccountsExclusion.reason enum = %v, want every reason the handler emits %v", got, want)
	}
}
