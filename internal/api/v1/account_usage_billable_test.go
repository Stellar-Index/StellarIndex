package v1_test

import (
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestAccountUsage_BillableSameMeaningOnBothShapes pins GH-1278 at the
// handler: `billable` carries the rollup's quota-counted units, and a
// legacy row — the rollup-gap backfill inside a per-endpoint response
// and the whole-response fallback alike — reports its billable total
// as `billable`, so summing one column reconciles on either shape.
func TestAccountUsage_BillableSameMeaningOnBothShapes(t *testing.T) {
	subject := auth.Subject{Identifier: "owner-b", KeyID: "kid_b", Tier: auth.TierAPIKey}
	legacy := &fakeUsageReader{days: []v1.UsageDay{
		{Date: "2026-07-01", Requests: 30},
		{Date: "2026-07-02", Requests: 999},
	}}
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-02", Endpoint: "/v1/price", Requests: 17, Billable: 12, Errors: 7, Throttled: 3},
	}}

	got := map[string]v1.UsageRow{}
	for _, r := range getUsageRows(t, newUsageTestServer(t, subject, rollup, legacy)) {
		got[r.Date+"|"+r.Endpoint] = r
	}
	if r := got["2026-07-02|/v1/price"]; r.Billable != 12 || r.Requests != 17 {
		t.Errorf("rollup row = %+v, want billable 12 / requests 17", r)
	}
	if r := got["2026-07-01|"]; r.Billable != 30 {
		t.Errorf("backfilled legacy row = %+v, want billable 30", r)
	}

	rows := getUsageRows(t, newUsageTestServer(t, subject, &fakeUsageRollupReader{}, legacy))
	if len(rows) != 2 || rows[0].Billable != 30 || rows[1].Billable != 999 {
		t.Errorf("legacy fallback rows = %+v, want billable 30 and 999", rows)
	}
}
