package v1_test

import (
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestAccountUsage_RollupBackfillsMissingDay — Q160. A day the
// rollup worker never produced a row for at all (an outage gap, not
// a legitimate zero-traffic day) must be filled in from the legacy
// per-day reader rather than silently dropped from the trailing
// 30-day window. Pre-fix, readUsageRollup returned ok=true as soon
// as len(days) > 0 and handleAccountUsage never consulted the legacy
// reader again, so a gap day just vanished from the response.
func TestAccountUsage_RollupBackfillsMissingDay(t *testing.T) {
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-03", Endpoint: "/v1/price", Requests: 40, Errors: 1, Throttled: 7},
	}}
	legacy := &fakeUsageReader{days: []v1.UsageDay{
		{Date: "2026-07-01", Requests: 12},  // rollup worker outage — no rollup row for this day
		{Date: "2026-07-03", Requests: 999}, // rollup already covers this day; must NOT leak
	}}
	ts := newUsageTestServer(t, auth.Subject{
		Identifier: "owner-9",
		Tier:       auth.TierAPIKey,
	}, rollup, legacy)

	rows := getUsageRows(t, ts)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (1 rollup row + 1 backfilled legacy day)", rows)
	}
	var gotBackfill bool
	for _, r := range rows {
		switch r.Date {
		case "2026-07-01":
			gotBackfill = true
			want := v1.UsageRow{Date: "2026-07-01", Requests: 12}
			if r != want {
				t.Errorf("backfilled row = %+v, want %+v", r, want)
			}
		case "2026-07-03":
			if r.Requests == 999 {
				t.Error("legacy total leaked over a day the rollup reader already covered")
			}
		}
	}
	if !gotBackfill {
		t.Errorf("missing rollup day 2026-07-01 was not backfilled from the legacy reader; rows = %+v", rows)
	}
}

// TestAccountUsage_RollupBackfill_LegacyUnwired — no legacy reader
// wired: the rollup rows still return (unwired backfill degrades to
// no backfill, not an error), same posture as the rollup path itself.
func TestAccountUsage_RollupBackfill_LegacyUnwired(t *testing.T) {
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-03", Endpoint: "/v1/price", Requests: 40},
	}}
	ts := newUsageTestServer(t, auth.Subject{
		Identifier: "owner-9",
		Tier:       auth.TierAPIKey,
	}, rollup, nil)

	rows := getUsageRows(t, ts)
	if len(rows) != 1 || rows[0].Date != "2026-07-03" {
		t.Errorf("rows = %+v, want the single rollup row unchanged", rows)
	}
}
