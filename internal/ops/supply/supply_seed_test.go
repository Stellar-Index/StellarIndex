package supply

import (
	"reflect"
	"testing"
)

// TestAccountObservationSeedProvenance — seed-observations must leave an
// audit trail, or a pass run weeks ago is indistinguishable from one that
// never ran. This pins the record shape supplySeedObservations upserts only
// at the end of a COMPLETE pass (never on -dry-run, never mid-loop): nil
// ledger bounds when nothing was seeded, populated bounds otherwise, and the
// watched/missing G-strkeys carried through so a `missing` count traces to a
// specific account even after the configured watchlist later changes.
func TestAccountObservationSeedProvenance(t *testing.T) {
	watched := []string{"GA", "GB", "GC"}

	t.Run("nothing seeded means nil ledger bounds", func(t *testing.T) {
		missingAccounts := []string{"GB", "GC"}
		p := accountObservationSeedProvenance(watched, 0, 2, 1, 0, 0, false, missingAccounts)
		if p.AccountsWatched != 3 || p.AccountsSeeded != 0 || p.AccountsMissing != 2 || p.AccountsRemoved != 1 {
			t.Errorf("counts = %+v, want {watched:3 seeded:0 missing:2 removed:1}", p)
		}
		if !reflect.DeepEqual(p.WatchedAccounts, watched) {
			t.Errorf("WatchedAccounts = %v, want %v", p.WatchedAccounts, watched)
		}
		if !reflect.DeepEqual(p.MissingAccounts, missingAccounts) {
			t.Errorf("MissingAccounts = %v, want %v", p.MissingAccounts, missingAccounts)
		}
		if p.MinLedgerSeen != nil || p.MaxLedgerSeen != nil {
			t.Errorf("ledger bounds = [%v, %v], want nil/nil when nothing was seeded", p.MinLedgerSeen, p.MaxLedgerSeen)
		}
	})

	t.Run("a complete pass carries the seeded ledger range", func(t *testing.T) {
		p := accountObservationSeedProvenance(watched, 3, 0, 0, 30_000_000, 63_400_000, true, nil)
		if p.AccountsWatched != 3 || p.AccountsSeeded != 3 {
			t.Errorf("counts = %+v, want {watched:3 seeded:3}", p)
		}
		if len(p.MissingAccounts) != 0 {
			t.Errorf("MissingAccounts = %v, want empty when nothing is missing", p.MissingAccounts)
		}
		if p.MinLedgerSeen == nil || *p.MinLedgerSeen != 30_000_000 || p.MaxLedgerSeen == nil || *p.MaxLedgerSeen != 63_400_000 {
			t.Errorf("ledger bounds = [%v, %v], want [30000000, 63400000]", p.MinLedgerSeen, p.MaxLedgerSeen)
		}
	})
}
