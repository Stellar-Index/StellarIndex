package archivecompleteness

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
)

func mkHashDB(t *testing.T, startLedger uint32) *hashdb.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := hashdb.Create(path, startLedger)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestHashDBWindowVerifier_OK is the steady-state case: re-observing
// the same bytes hashdb already recorded should tally as Verified,
// not Drifted — mirrors hashdb's own TestVerify_OK.
func TestHashDBWindowVerifier_OK(t *testing.T) {
	t.Parallel()
	db := mkHashDB(t, 1000)

	h := hashdb.Hash([]byte("ledger-1000-bytes"))
	if err := db.Append(1000, h); err != nil {
		t.Fatal(err)
	}

	v := NewHashDBWindowVerifier(db, 1000, 1000)
	if err := v.Observe(1000, h); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	res := v.Result()
	if res.Verified != 1 || res.Drifted != 0 || res.Missing != 0 || res.OutOfRange != 0 {
		t.Fatalf("Result = %+v, want Verified=1 and everything else 0", res)
	}
	if res.AnyDrift() {
		t.Error("AnyDrift() = true on a clean pass")
	}
}

// TestHashDBWindowVerifier_DriftDetected is the alert path this
// package exists to serve: the caller observes a DIFFERENT hash for a
// ledger hashdb already has a record for — e.g. ledger 63332650's
// class of incident (upstream rewrote a previously-fetched ledger, or
// our local copy is corrupted). Must tally as Drifted, record the
// sequence, and AnyDrift() must report true.
func TestHashDBWindowVerifier_DriftDetected(t *testing.T) {
	t.Parallel()
	db := mkHashDB(t, 0)

	original := hashdb.Hash([]byte("ledger-63332650-original-bytes"))
	rewritten := hashdb.Hash([]byte("ledger-63332650-REWRITTEN-bytes"))
	if err := db.Append(63332650, original); err != nil {
		t.Fatal(err)
	}

	v := NewHashDBWindowVerifier(db, 63332650, 63332650)
	if err := v.Observe(63332650, rewritten); err != nil {
		t.Fatalf("Observe on drift returned an error (should be counted, not errored): %v", err)
	}
	res := v.Result()
	if res.Drifted != 1 {
		t.Fatalf("Drifted = %d, want 1", res.Drifted)
	}
	if !res.AnyDrift() {
		t.Error("AnyDrift() = false after a drift observation")
	}
	if len(res.DriftSeqs) != 1 || res.DriftSeqs[0] != 63332650 {
		t.Errorf("DriftSeqs = %v, want [63332650]", res.DriftSeqs)
	}
	if res.Verified != 0 {
		t.Errorf("Verified = %d, want 0 (the ledger drifted, it wasn't clean)", res.Verified)
	}
}

// TestHashDBWindowVerifier_DriftDoesNotAbort confirms multiple
// drifted ledgers in one window all get tallied — an operator
// investigating a suspected rewrite wants the full picture, not just
// the first hit.
func TestHashDBWindowVerifier_DriftDoesNotAbort(t *testing.T) {
	t.Parallel()
	db := mkHashDB(t, 100)

	for seq := uint32(100); seq <= 104; seq++ {
		if err := db.Append(seq, hashdb.Hash([]byte("original"))); err != nil {
			t.Fatal(err)
		}
	}

	v := NewHashDBWindowVerifier(db, 100, 104)
	for seq := uint32(100); seq <= 104; seq++ {
		// Every ledger in the window drifts.
		if err := v.Observe(seq, hashdb.Hash([]byte("rewritten"))); err != nil {
			t.Fatalf("Observe(%d): %v", seq, err)
		}
	}
	res := v.Result()
	if res.Drifted != 5 {
		t.Fatalf("Drifted = %d, want 5 (all 5 ledgers in the window)", res.Drifted)
	}
	if len(res.DriftSeqs) != 5 {
		t.Errorf("DriftSeqs len = %d, want 5", len(res.DriftSeqs))
	}
}

// TestHashDBWindowVerifier_DriftSeqsCapped confirms the reported
// sequence list truncates at MaxHashDBDriftSeqsReported while Drifted
// keeps the true total — mirrors CrossAnchorResult's Truncated
// contract (bounded payload even under a worst-case scenario).
func TestHashDBWindowVerifier_DriftSeqsCapped(t *testing.T) {
	t.Parallel()
	total := MaxHashDBDriftSeqsReported + 10
	db := mkHashDB(t, 0)
	for i := 0; i < total; i++ {
		seq := uint32(i) //nolint:gosec // bounded by test loop, no overflow risk
		if err := db.Append(seq, hashdb.Hash([]byte("original"))); err != nil {
			t.Fatal(err)
		}
	}

	v := NewHashDBWindowVerifier(db, 0, uint32(total-1))
	for i := 0; i < total; i++ {
		seq := uint32(i) //nolint:gosec // bounded by test loop, no overflow risk
		if err := v.Observe(seq, hashdb.Hash([]byte("rewritten"))); err != nil {
			t.Fatal(err)
		}
	}
	res := v.Result()
	if res.Drifted != total {
		t.Fatalf("Drifted = %d, want %d (true count, uncapped)", res.Drifted, total)
	}
	if len(res.DriftSeqs) != MaxHashDBDriftSeqsReported {
		t.Errorf("DriftSeqs len = %d, want %d (capped)", len(res.DriftSeqs), MaxHashDBDriftSeqsReported)
	}
}

// TestHashDBWindowVerifier_Missing covers the "no baseline yet" case
// — a ledger inside hashdb's covered range but never appended (e.g.
// the window reaches back before hashdb was enabled). Must count as
// Missing, NOT Drifted — Missing is "nothing to compare", a
// fundamentally different signal than "compared and disagreed".
func TestHashDBWindowVerifier_Missing(t *testing.T) {
	t.Parallel()
	db := mkHashDB(t, 100)
	// Deliberately append nothing.

	v := NewHashDBWindowVerifier(db, 100, 100)
	if err := v.Observe(100, hashdb.Hash([]byte("anything"))); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	res := v.Result()
	if res.Missing != 1 || res.Drifted != 0 {
		t.Fatalf("Result = %+v, want Missing=1, Drifted=0", res)
	}
}

// TestHashDBWindowVerifier_OutOfRange covers a window whose lower
// bound reaches further back than hashdb's coverage — e.g. hashdb was
// created partway through a region's history. Must count as
// OutOfRange, not error the whole sweep.
func TestHashDBWindowVerifier_OutOfRange(t *testing.T) {
	t.Parallel()
	db := mkHashDB(t, 1000)

	v := NewHashDBWindowVerifier(db, 500, 500)
	if err := v.Observe(500, hashdb.Hash([]byte("anything"))); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	res := v.Result()
	if res.OutOfRange != 1 || res.Drifted != 0 {
		t.Fatalf("Result = %+v, want OutOfRange=1, Drifted=0", res)
	}
}

// TestNewHashDBWindowVerifier_NilDB confirms Observe fails loudly
// (returns an error) rather than nil-pointer-panicking when
// misconstructed with a nil DB — defensive, since a nil db would
// otherwise panic deep inside hashdb.DB.Verify.
func TestNewHashDBWindowVerifier_NilDB(t *testing.T) {
	t.Parallel()
	v := NewHashDBWindowVerifier(nil, 1, 1)
	err := v.Observe(1, hashdb.Hash([]byte("x")))
	if err == nil {
		t.Fatal("Observe with nil DB returned nil error, want a non-nil error")
	}
}

// TestHashDBWindowVerifier_HashdbIOErrorAborts confirms a genuine
// hashdb I/O failure (as opposed to a hash mismatch) is NOT folded
// into Drifted/Missing/OutOfRange — it's a different failure mode
// ("we couldn't even check") and must surface as an error so the
// caller doesn't mistake "the check never ran" for "the check ran
// clean".
func TestHashDBWindowVerifier_HashdbIOErrorAborts(t *testing.T) {
	t.Parallel()
	db := mkHashDB(t, 0)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// db is now closed; any Verify against it should fail with a
	// generic I/O error, not one of hashdb's typed sentinels.
	v := NewHashDBWindowVerifier(db, 0, 0)
	err := v.Observe(0, hashdb.Hash([]byte("x")))
	if err == nil {
		t.Fatal("Observe against a closed hashdb returned nil, want an error")
	}
	if errors.Is(err, hashdb.ErrDrift) || errors.Is(err, hashdb.ErrMissing) || errors.Is(err, hashdb.ErrOutOfRange) {
		t.Errorf("Observe against a closed hashdb returned a typed sentinel (%v) instead of a generic I/O error", err)
	}
}
