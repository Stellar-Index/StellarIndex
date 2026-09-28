package supply

import "testing"

// TestAccountObservationSeedProvenance — GH #1201: seed-observations wrote no
// audit trail, so a pass run weeks ago was indistinguishable from one that
// never ran. This pins the record shape supplySeedObservations upserts only
// at the end of a COMPLETE pass (never on -dry-run, never mid-loop): nil
// ledger bounds when nothing was seeded, and populated bounds otherwise.
func TestAccountObservationSeedProvenance(t *testing.T) {
	t.Run("nothing seeded means nil ledger bounds", func(t *testing.T) {
		p := accountObservationSeedProvenance(3, 0, 2, 1, 0, 0, false)
		if p.AccountsWatched != 3 || p.AccountsSeeded != 0 || p.AccountsMissing != 2 || p.AccountsRemoved != 1 {
			t.Errorf("counts = %+v, want {watched:3 seeded:0 missing:2 removed:1}", p)
		}
		if p.MinLedgerSeen != nil || p.MaxLedgerSeen != nil {
			t.Errorf("ledger bounds = [%v, %v], want nil/nil when nothing was seeded", p.MinLedgerSeen, p.MaxLedgerSeen)
		}
	})

	t.Run("a complete pass carries the seeded ledger range", func(t *testing.T) {
		p := accountObservationSeedProvenance(3, 3, 0, 0, 30_000_000, 63_400_000, true)
		if p.AccountsWatched != 3 || p.AccountsSeeded != 3 {
			t.Errorf("counts = %+v, want {watched:3 seeded:3}", p)
		}
		if p.MinLedgerSeen == nil || *p.MinLedgerSeen != 30_000_000 || p.MaxLedgerSeen == nil || *p.MaxLedgerSeen != 63_400_000 {
			t.Errorf("ledger bounds = [%v, %v], want [30000000, 63400000]", p.MinLedgerSeen, p.MaxLedgerSeen)
		}
	})
}
