//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestObservationIntraLedgerSeqGuard is the proven-red test for the C2-6
// 8-worker last-writer-wins bug (audit-2026-07-16 / migration 0111): the
// PersistEvents workers (PersistWorkers=8) do NOT preserve order, and the
// `*_observations` writers upserted with pure last-writer-wins. So when a
// single (contract/holder, asset, ledger) changes MULTIPLE times within one
// ledger, whichever worker commits LAST wins — which is NOT necessarily the
// FINAL intra-ledger state. A stale intra-ledger balance could be persisted
// as the observation for that ledger → a wrong classic-supply component (the
// served supply the operator keeps re-backfilling).
//
// The fix stamps each observation with its within-ledger position
// (intra_ledger_seq) and guards the upsert
// (intra_ledger_seq <= EXCLUDED.intra_ledger_seq) so an EARLIER intra-ledger
// change delivered LATE by an out-of-order worker can never overwrite the
// FINAL change.
//
// Proven-red evidence, all on ONE live container:
//   - "unguarded_last_writer_wins_reproduces_bug" runs the EXACT pre-fix SQL
//     (a plain ON CONFLICT DO UPDATE with no seq guard) and shows the STALE
//     (earlier) value wins when it commits last — the bug, reproduced.
//   - "guarded_writer_keeps_final" runs the REAL fixed writer with the SAME
//     out-of-order sequence and shows the FINAL value wins. This assertion
//     FAILS on the pre-fix writer (no column / no guard → the stale late
//     write wins), so it is red-on-unfixed and green-after-fix.
func TestObservationIntraLedgerSeqGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// One fixed instant so both writes share the (…, ledger, observed_at)
	// primary key — a conflict on the SAME row, exactly as the live path
	// produces for two changes to one entry in one ledger (observed_at is the
	// ledger close time, identical for every change in the ledger).
	observedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	const (
		assetKey   = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		ledger     = uint32(5_000_000)
		staleValue = int64(100) // an EARLIER intra-ledger balance
		finalValue = int64(900) // the FINAL intra-ledger balance
	)

	readSAC := func(t *testing.T, contractID, holder string) (balance string, seq int64) {
		t.Helper()
		const q = `SELECT balance_stroops::text, intra_ledger_seq
		             FROM sac_balance_observations
		            WHERE contract_id = $1 AND holder = $2 AND ledger = $3`
		if err := store.DB().QueryRowContext(ctx, q, contractID, holder, int(ledger)).
			Scan(&balance, &seq); err != nil {
			t.Fatalf("read sac_balance_observations (%s,%s): %v", contractID, holder, err)
		}
		return balance, seq
	}

	writeSAC := func(t *testing.T, contractID, holder string, bal int64, seq uint32) {
		t.Helper()
		if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
			ContractID:     contractID,
			AssetKey:       assetKey,
			Holder:         holder,
			Ledger:         ledger,
			ObservedAt:     observedAt,
			Balance:        big.NewInt(bal),
			IntraLedgerSeq: seq,
		}); err != nil {
			t.Fatalf("InsertSACBalanceObservation(%s,%s,seq=%d): %v", contractID, holder, seq, err)
		}
	}

	// ── The pre-fix behaviour, reproduced live ────────────────────────────
	// Raw unguarded last-writer-wins (the writer BEFORE this fix), writing the
	// FINAL change first and the EARLIER change last (an out-of-order worker):
	// the stale earlier value overwrites the final. This is the C2-6 bug.
	t.Run("unguarded_last_writer_wins_reproduces_bug", func(t *testing.T) {
		const (
			contractID = "CBUNGUARDEDSACWRAPPERCONTRACTID000000000000000000000000"
			holder     = "GHOLDERUNGUARDEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		unguardedUpsert := func(bal int64, seq uint32) {
			// Byte-for-byte the pre-0111 writer: value columns overwritten on
			// conflict with NO intra_ledger_seq guard. (We still populate the
			// column so the row is valid post-migration; the point is the
			// MISSING WHERE clause.)
			const q = `
                INSERT INTO sac_balance_observations (
                    contract_id, asset_key, holder, ledger, observed_at,
                    balance_stroops, is_removal, intra_ledger_seq
                ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
                ON CONFLICT (contract_id, holder, ledger, observed_at) DO UPDATE SET
                    asset_key        = EXCLUDED.asset_key,
                    balance_stroops  = EXCLUDED.balance_stroops,
                    is_removal       = EXCLUDED.is_removal,
                    intra_ledger_seq = EXCLUDED.intra_ledger_seq
            `
			if _, err := store.DB().ExecContext(ctx, q,
				contractID, assetKey, holder, int(ledger), observedAt,
				big.NewInt(bal).String(), false, int64(seq),
			); err != nil {
				t.Fatalf("unguarded upsert (bal=%d,seq=%d): %v", bal, seq, err)
			}
		}

		unguardedUpsert(finalValue, 5) // FINAL change lands first
		unguardedUpsert(staleValue, 2) // EARLIER change commits LAST (out of order)

		if bal, _ := readSAC(t, contractID, holder); bal != "100" {
			t.Fatalf("pre-fix reproduction: balance = %s, expected the STALE 100 to win "+
				"under unguarded last-writer-wins (the C2-6 bug)", bal)
		}
		t.Logf("pre-fix last-writer-wins persisted the STALE intra-ledger balance 100 "+
			"(final was %d) — the bug the guard fixes", finalValue)
	})

	// ── The fix ───────────────────────────────────────────────────────────
	// The REAL guarded writer, SAME out-of-order sequence: FINAL first, then
	// the EARLIER change last. The guard rejects the late earlier write, so
	// the FINAL value survives. Red-on-unfixed (no column/guard → stale wins).
	t.Run("guarded_writer_keeps_final_when_earlier_change_commits_last", func(t *testing.T) {
		const (
			contractID = "CBGUARDEDSACWRAPPERCONTRACTID000000000000000000000000000"
			holder     = "GHOLDERGUARDEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		writeSAC(t, contractID, holder, finalValue, 5) // FINAL change lands first
		writeSAC(t, contractID, holder, staleValue, 2) // EARLIER change commits LAST

		if bal, seq := readSAC(t, contractID, holder); bal != "900" || seq != 5 {
			t.Fatalf("out-of-order: balance = %s (seq %d), want FINAL 900 (seq 5) — "+
				"the guard must reject the late-arriving earlier change (C2-6)", bal, seq)
		}
	})

	// Forward path unbroken: writing in the natural order (earlier then final)
	// must also land the FINAL value — the guard admits a HIGHER position.
	t.Run("guarded_writer_forward_order_still_lands_final", func(t *testing.T) {
		const (
			contractID = "CBFORWARDSACWRAPPERCONTRACTID000000000000000000000000000"
			holder     = "GHOLDERFORWARDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		writeSAC(t, contractID, holder, staleValue, 2) // earlier change first
		writeSAC(t, contractID, holder, finalValue, 5) // final change second

		if bal, seq := readSAC(t, contractID, holder); bal != "900" || seq != 5 {
			t.Fatalf("forward order: balance = %s (seq %d), want FINAL 900 (seq 5)", bal, seq)
		}
	})

	// The ops seed sentinel (SeedIntraLedgerSeq = MaxUint32) is the top of the
	// intra-ledger order: a live per-ledger change (a much smaller position)
	// can never overwrite a seed, and a re-seed (equal sentinel) stays
	// corrective. This is what keeps the seed authoritative AND correctable.
	t.Run("seed_sentinel_wins_over_live_and_reseed_is_corrective", func(t *testing.T) {
		const (
			contractID = "CBSEEDSACWRAPPERCONTRACTID0000000000000000000000000000000"
			holder     = "GHOLDERSEEDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		)
		writeSAC(t, contractID, holder, 500, timescale.SeedIntraLedgerSeq) // seed: authoritative final state
		writeSAC(t, contractID, holder, 999, 7)                            // a live per-ledger change, later
		if bal, _ := readSAC(t, contractID, holder); bal != "500" {
			t.Fatalf("seed sentinel: balance = %s, want the seed's 500 to survive a "+
				"live per-ledger change (live position cannot exceed the sentinel)", bal)
		}
		writeSAC(t, contractID, holder, 501, timescale.SeedIntraLedgerSeq) // re-seed correction
		if bal, _ := readSAC(t, contractID, holder); bal != "501" {
			t.Fatalf("re-seed: balance = %s, want the corrective re-seed 501 to land "+
				"(equal sentinel is admitted by the <= guard)", bal)
		}
	})

	// The finding lists account_observations.go:43 as a sibling site; the
	// same guard protects the native-XLM reserve component (fee + op + op in
	// one ledger). Prove the FINAL post-state wins under out-of-order commit.
	t.Run("account_observation_guard_keeps_final", func(t *testing.T) {
		const accountID = "GACCOUNTC26AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		writeAcct := func(bal int64, seq uint32) {
			t.Helper()
			if err := store.InsertAccountObservation(ctx, domain.AccountObservation{
				AccountID:      accountID,
				Ledger:         ledger,
				ObservedAt:     observedAt,
				Balance:        big.NewInt(bal),
				IntraLedgerSeq: seq,
			}); err != nil {
				t.Fatalf("InsertAccountObservation(seq=%d): %v", seq, err)
			}
		}
		writeAcct(finalValue, 9) // FINAL post-state (last op) lands first
		writeAcct(staleValue, 4) // an earlier change (e.g. fee debit) commits last

		const q = `SELECT balance_stroops::text FROM account_observations
		            WHERE account_id = $1 AND ledger = $2`
		var bal string
		if err := store.DB().QueryRowContext(ctx, q, accountID, int(ledger)).Scan(&bal); err != nil {
			t.Fatalf("read account_observations: %v", err)
		}
		if bal != "900" {
			t.Fatalf("account out-of-order: balance = %s, want FINAL 900 (C2-6)", bal)
		}
	})
}
