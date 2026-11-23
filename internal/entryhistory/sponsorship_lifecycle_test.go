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
	entry := clickhouse.AccountState{Exists: true, NumSponsoring: 3, NumSponsored: 2, LastModifiedLedger: 100, AsOfLedger: 100}
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
	const g = GenesisDeriveFloor
	want("agrees", ProveSponsorships(entry, g, 100, sponsoring, sponsored), &yes)
	want("derived from ledger 1", ProveSponsorships(entry, 1, 100, sponsoring, sponsored), &yes)
	want("sponsor side short", ProveSponsorships(entry, g, 100, clickhouse.SponsorshipTotals{InForceReserves: 2}, sponsored), &no)
	want("sponsored side over", ProveSponsorships(entry, g, 100, sponsoring, clickhouse.SponsorshipTotals{InForceReserves: 4}), &no)
	want("modified after the watermark", ProveSponsorships(entry, g, 99, sponsoring, sponsored), nil)
	want("nothing derived", ProveSponsorships(entry, g, 0, sponsoring, sponsored), nil)
	want("absent account", ProveSponsorships(clickhouse.AccountState{}, g, 100, sponsoring, sponsored), nil)
	// The entry was read at ledger 1000; at 1500 the account began sponsoring, which the derive through 2000 holds.
	stale := clickhouse.AccountState{Exists: true, LastModifiedLedger: 900, AsOfLedger: 1000}
	want("snapshot predates the watermark", ProveSponsorships(stale, g, 2000, clickhouse.SponsorshipTotals{InForceReserves: 1}, clickhouse.SponsorshipTotals{}), nil)
	unstamped := entry
	unstamped.AsOfLedger = 0
	want("snapshot with no vintage", ProveSponsorships(unstamped, g, 100, sponsoring, sponsored), nil)
	later := entry
	later.AsOfLedger = 5000
	want("snapshot after the watermark", ProveSponsorships(later, g, 100, sponsoring, sponsored), &yes)
	if p := ProveSponsorships(entry, g, 100, sponsoring, sponsored); p.DerivedSponsoring != 3 || p.DerivedSponsored != 2 || p.ThroughLedger != 100 {
		t.Fatalf("proof numbers = %+v", p)
	}
}

// An arrangement in force since below the derive floor has no event, so the derived reserves undercount and the
// counters cannot be compared: both a recorded floor above genesis and an unrecorded one (0) leave the proof open.
func TestProveSponsorships_FloorAboveGenesisIsNotComparable(t *testing.T) {
	entry := clickhouse.AccountState{Exists: true, NumSponsoring: 2, LastModifiedLedger: 49_000_000, AsOfLedger: 60_000_000}
	for _, floor := range []uint32{50_000_000, GenesisDeriveFloor + 1, 0} {
		p := ProveSponsorships(entry, floor, 60_000_000, clickhouse.SponsorshipTotals{}, clickhouse.SponsorshipTotals{})
		if p.Match != nil {
			t.Fatalf("floor %d: match = %v, want not comparable", floor, *p.Match)
		}
		if p.FromLedger != floor {
			t.Fatalf("floor %d: FromLedger = %d", floor, p.FromLedger)
		}
	}
}
