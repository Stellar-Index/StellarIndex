package entryhistory

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func spEvent(ledger uint32, event string) clickhouse.SponsorshipEventRow {
	return clickhouse.SponsorshipEventRow{Ledger: ledger, TxHash: "t", Event: event}
}

func TestSponsorshipEpisodes(t *testing.T) {
	type ep struct{ start, end uint32 }
	cases := []struct {
		name    string
		events  []clickhouse.SponsorshipEventRow
		want    []ep
		inForce bool
	}{
		{"none", nil, nil, false},
		{"open", []clickhouse.SponsorshipEventRow{spEvent(10, SponsorshipStarted)}, []ep{{10, 0}}, true},
		{"revoked then restarted", []clickhouse.SponsorshipEventRow{
			spEvent(10, SponsorshipStarted), spEvent(20, SponsorshipRevoked), spEvent(30, SponsorshipStarted),
		}, []ep{{10, 20}, {30, 0}}, true},
		{"ended below the floor", []clickhouse.SponsorshipEventRow{spEvent(20, SponsorshipEnded)}, []ep{{0, 20}}, false},
		{"start while open stays open", []clickhouse.SponsorshipEventRow{
			spEvent(10, SponsorshipStarted), spEvent(20, SponsorshipStarted), spEvent(30, SponsorshipEnded),
		}, []ep{{10, 0}, {20, 30}}, false},
		{"unknown event ignored", []clickhouse.SponsorshipEventRow{spEvent(10, "observed")}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, inForce := SponsorshipEpisodes(tc.events)
			if inForce != tc.inForce || len(got) != len(tc.want) {
				t.Fatalf("episodes = %+v in force %v, want %+v in force %v", got, inForce, tc.want, tc.inForce)
			}
			for i, w := range tc.want {
				var start, end uint32
				if got[i].Start != nil {
					start = got[i].Start.Ledger
				}
				if got[i].End != nil {
					end = got[i].End.Ledger
				}
				if start != w.start || end != w.end {
					t.Fatalf("episode %d = %d..%d, want %d..%d", i, start, end, w.start, w.end)
				}
			}
		})
	}
}

func TestProveSponsorships(t *testing.T) {
	entry := clickhouse.AccountState{Exists: true, NumSponsoring: 3, NumSponsored: 2, LastModifiedLedger: 100}
	sponsoring := clickhouse.SponsorshipTotals{InForce: 2, InForceReserves: 3}
	sponsored := clickhouse.SponsorshipTotals{InForce: 1, InForceReserves: 2}
	want := func(name string, p SponsorshipProof, match *bool) {
		t.Helper()
		switch {
		case match == nil && p.Match != nil:
			t.Fatalf("%s: match = %v, want not comparable", name, *p.Match)
		case match != nil && (p.Match == nil || *p.Match != *match):
			t.Fatalf("%s: match = %v, want %v", name, p.Match, *match)
		}
	}
	yes, no := true, false
	want("agrees", ProveSponsorships(entry, 100, sponsoring, sponsored), &yes)
	want("sponsor side short", ProveSponsorships(entry, 100, clickhouse.SponsorshipTotals{InForceReserves: 2}, sponsored), &no)
	want("sponsored side over", ProveSponsorships(entry, 100, sponsoring, clickhouse.SponsorshipTotals{InForceReserves: 4}), &no)
	want("modified after the watermark", ProveSponsorships(entry, 99, sponsoring, sponsored), nil)
	want("nothing derived", ProveSponsorships(entry, 0, sponsoring, sponsored), nil)
	want("absent account", ProveSponsorships(clickhouse.AccountState{}, 100, sponsoring, sponsored), nil)
	if p := ProveSponsorships(entry, 100, sponsoring, sponsored); p.DerivedSponsoring != 3 || p.DerivedSponsored != 2 || p.ThroughLedger != 100 {
		t.Fatalf("proof numbers = %+v", p)
	}
}
