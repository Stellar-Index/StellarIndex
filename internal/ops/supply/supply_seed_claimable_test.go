package supply

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const seedClaimableIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

// TestParseClaimableSeedAssets pins the scope contract. The DEFAULT must be
// "every classic credit asset" (a nil set): claimable balances have no
// operator-curated watched set, and a seed that quietly covered only some
// assets would leave the rest under-reported in exactly the way this command
// exists to fix.
//
// It also pins the dash/colon acceptance. The 2026-07-02 production bug that
// zeroed three supply components was a dash-form config string compared
// against colon-form decoded keys, failing OPEN (observed nothing, reported
// success). Routing -assets through supply.CanonicalizeWatchedClassic makes a
// typo a loud error instead.
func TestParseClaimableSeedAssets(t *testing.T) {
	wantKey := "AQUA:" + seedClaimableIssuer

	t.Run("empty means every classic asset", func(t *testing.T) {
		for _, raw := range []string{"", "   ", ",", " , ,"} {
			got, err := parseClaimableSeedAssets(raw)
			if err != nil {
				t.Fatalf("parseClaimableSeedAssets(%q): %v", raw, err)
			}
			if got != nil {
				t.Errorf("parseClaimableSeedAssets(%q) = %v, want nil (= seed everything)", raw, got)
			}
		}
	})

	t.Run("dash and colon forms both canonicalize", func(t *testing.T) {
		for _, raw := range []string{"AQUA-" + seedClaimableIssuer, wantKey, " AQUA-" + seedClaimableIssuer + " , " + wantKey} {
			got, err := parseClaimableSeedAssets(raw)
			if err != nil {
				t.Fatalf("parseClaimableSeedAssets(%q): %v", raw, err)
			}
			if len(got) != 1 {
				t.Fatalf("parseClaimableSeedAssets(%q) = %v, want a single key", raw, got)
			}
			if _, ok := got[wantKey]; !ok {
				t.Errorf("parseClaimableSeedAssets(%q) = %v, want %q", raw, got, wantKey)
			}
		}
	})

	t.Run("garbage is a loud error, never an empty filter", func(t *testing.T) {
		for _, raw := range []string{"not-an-asset", "native", "AQUA-nope"} {
			if _, err := parseClaimableSeedAssets(raw); err == nil {
				t.Errorf("parseClaimableSeedAssets(%q) succeeded; a typo must fail the run, not silently scope it", raw)
			}
		}
	})
}

// TestClaimableSeedTally_ObserveTracksMinMaxLedger — the MIN is the operator's
// evidence that the pass actually reached below the live observer's ledger
// 63,301,831 floor rather than re-confirming rows that were already there.
func TestClaimableSeedTally_ObserveTracksMinMaxLedger(t *testing.T) {
	tally := &claimableSeedTally{sum: big.NewInt(0)}
	if tally.haveLedgerBounds {
		t.Fatal("zero-value tally should not have ledger bounds yet")
	}
	tally.observe(63_400_000)
	if tally.minLedger != 63_400_000 || tally.maxLedger != 63_400_000 {
		t.Fatalf("after first observe: min=%d max=%d, want both 63400000", tally.minLedger, tally.maxLedger)
	}
	tally.observe(33_000_000) // a pre-floor balance — the whole point
	if tally.minLedger != 33_000_000 || tally.maxLedger != 63_400_000 {
		t.Errorf("min=%d max=%d, want 33000000/63400000", tally.minLedger, tally.maxLedger)
	}
	tally.observe(64_000_000)
	if tally.maxLedger != 64_000_000 {
		t.Errorf("max = %d, want 64000000", tally.maxLedger)
	}
	tally.observe(50_000_000) // strictly between: changes neither
	if tally.minLedger != 33_000_000 || tally.maxLedger != 64_000_000 {
		t.Errorf("mid-range observe moved the bounds: min=%d max=%d", tally.minLedger, tally.maxLedger)
	}
}

// TestClaimableSeedWriterDryRunTallies — -dry-run must produce the full
// per-asset report (count + summed balance, as *big.Int per ADR-0003) while
// buffering nothing for the writer. A dry run that queued rows would flush
// them on the next non-dry call path.
func TestClaimableSeedWriterDryRunTallies(t *testing.T) {
	w := &claimableSeedWriter{dryRun: true, tallies: map[string]*claimableSeedTally{}}
	aqua, usdc := "AQUA:"+seedClaimableIssuer, "USDC:"+seedClaimableIssuer
	// Deliberately above 2^53 — the value survives as an exact integer only
	// because it is carried as *big.Int, never a float.
	big1, _ := new(big.Int).SetString("9007199254740993", 10)
	seeds := []clickhouse.ClaimableBalanceSeed{
		{ClaimableID: "a", AssetKey: aqua, Balance: big1, LedgerSeq: 30_000_000},
		{ClaimableID: "b", AssetKey: aqua, Balance: big.NewInt(7), LedgerSeq: 63_400_000},
		{ClaimableID: "c", AssetKey: usdc, Balance: big.NewInt(5), LedgerSeq: 45_000_000},
	}
	for _, s := range seeds {
		if err := w.add(s); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if len(w.pending) != 0 {
		t.Errorf("dry-run buffered %d row(s); it must write nothing", len(w.pending))
	}
	if w.total != 3 {
		t.Errorf("total = %d, want 3", w.total)
	}
	wantAqua := new(big.Int).Add(big1, big.NewInt(7))
	if got := w.tallies[aqua]; got.count != 2 || got.sum.Cmp(wantAqua) != 0 {
		t.Errorf("AQUA tally = {count:%d sum:%s}, want {2 %s}", got.count, got.sum, wantAqua)
	}
	if got := w.tallies[aqua]; got.minLedger != 30_000_000 || got.maxLedger != 63_400_000 {
		t.Errorf("AQUA ledger bounds = [%d, %d], want [30000000, 63400000]", got.minLedger, got.maxLedger)
	}
	if got := w.tallies[usdc]; got.count != 1 || got.sum.Cmp(big.NewInt(5)) != 0 {
		t.Errorf("USDC tally = {count:%d sum:%s}, want {1 5}", got.count, got.sum)
	}
	if err := w.flush(); err != nil {
		t.Errorf("dry-run flush must be a no-op even with a nil store, got %v", err)
	}
}

// TestClaimableSeedWriterRowShape — every buffered row must carry the seed
// posture: is_removal=false (the reducer only ever emits LIVE balances) and
// intra_ledger_seq = SeedIntraLedgerSeq, so a live per-ledger observation can
// never overwrite the reconstructed final state and a re-seed stays corrective
// (audit-2026-07-16 C2-6).
func TestClaimableSeedWriterRowShape(t *testing.T) {
	w := &claimableSeedWriter{tallies: map[string]*claimableSeedTally{}}
	if err := w.add(clickhouse.ClaimableBalanceSeed{
		ClaimableID: "deadbeef", AssetKey: "AQUA:" + seedClaimableIssuer,
		Balance: big.NewInt(42), LedgerSeq: 30_000_000,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(w.pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(w.pending))
	}
	row := w.pending[0]
	if row.IsRemoval {
		t.Error("IsRemoval = true; the reducer emits only live balances, so a seeded row is never a tombstone")
	}
	if row.IntraLedgerSeq != timescale.SeedIntraLedgerSeq {
		t.Errorf("IntraLedgerSeq = %d, want SeedIntraLedgerSeq (%d)", row.IntraLedgerSeq, timescale.SeedIntraLedgerSeq)
	}
	if row.Ledger != 30_000_000 {
		t.Errorf("Ledger = %d, want the balance's TRUE last-modified ledger 30000000", row.Ledger)
	}
	if row.Balance.Cmp(big.NewInt(42)) != 0 {
		t.Errorf("Balance = %s, want 42", row.Balance)
	}
}
