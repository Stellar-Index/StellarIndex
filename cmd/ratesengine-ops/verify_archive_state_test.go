package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadVerifyArchiveState_missingFileReturnsZero confirms the
// first-run path: no state file yet means we get an empty state
// without error, so the caller can compute -from from explicit
// flags only.
func TestReadVerifyArchiveState_missingFileReturnsZero(t *testing.T) {
	t.Parallel()
	got, err := readVerifyArchiveState(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("err = %v, want nil for missing file", err)
	}
	if got.Tiers == nil {
		t.Errorf("Tiers nil — want empty map for caller's safety")
	}
	if len(got.Tiers) != 0 {
		t.Errorf("Tiers should be empty, got %d entries", len(got.Tiers))
	}
}

// TestReadVerifyArchiveState_emptyPathReturnsZero covers the
// state-file-disabled path: empty path means the operator opted out
// of incremental, so we return empty state without error.
func TestReadVerifyArchiveState_emptyPathReturnsZero(t *testing.T) {
	t.Parallel()
	got, err := readVerifyArchiveState("")
	if err != nil {
		t.Fatalf("err = %v, want nil for empty path", err)
	}
	if got.Tiers == nil || len(got.Tiers) != 0 {
		t.Errorf("Tiers = %v, want empty map", got.Tiers)
	}
}

// TestWriteRead_roundTrip exercises the atomic-rename write path
// + read parse. Uses t.TempDir so cleanup is automatic.
func TestWriteRead_roundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	want := VerifyArchiveState{
		Tiers: map[string]VerifyArchiveTierState{
			"chain": {
				LastVerifiedLedger: 60_000_000,
				LastVerifiedAt:     time.Date(2026, 5, 14, 17, 0, 0, 0, time.UTC),
				LastVerifiedHash:   "abc123",
			},
		},
	}
	if err := writeVerifyArchiveState(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Verify the .tmp file is gone (atomic rename happened).
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp still present, atomic rename didn't fire")
	}
	got, err := readVerifyArchiveState(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Tiers["chain"].LastVerifiedLedger != want.Tiers["chain"].LastVerifiedLedger {
		t.Errorf("LastVerifiedLedger = %d, want %d",
			got.Tiers["chain"].LastVerifiedLedger,
			want.Tiers["chain"].LastVerifiedLedger)
	}
	if got.Tiers["chain"].LastVerifiedHash != "abc123" {
		t.Errorf("LastVerifiedHash = %q, want abc123", got.Tiers["chain"].LastVerifiedHash)
	}
}

// TestWriteVerifyArchiveState_createsParentDir covers the
// mkdir -p semantics. Operator should never have to pre-create
// /var/lib/ratesengine for the write to succeed.
func TestWriteVerifyArchiveState_createsParentDir(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nested := filepath.Join(tmpdir, "deeply", "nested", "dir", "state.json")
	if err := writeVerifyArchiveState(nested, VerifyArchiveState{}); err != nil {
		t.Fatalf("write to nested path: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// TestResolveIncrementalFrom covers the four interesting branches:
// no prior state, fresh prior state, prior > overlap (normal case),
// prior < overlap (would underflow without floor).
func TestResolveIncrementalFrom(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		state         VerifyArchiveState
		tier          string
		explicitFrom  uint32
		safetyOverlap uint32
		want          uint32
	}{
		{
			name:          "no prior state → use explicit -from",
			state:         VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{}},
			tier:          "chain",
			explicitFrom:  2,
			safetyOverlap: 5000,
			want:          2,
		},
		{
			name: "prior < overlap → floor to ledger 2",
			state: VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{
				"chain": {LastVerifiedLedger: 1000},
			}},
			tier:          "chain",
			explicitFrom:  2,
			safetyOverlap: 5000,
			want:          2,
		},
		{
			name: "prior - overlap > explicit → use computed",
			state: VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{
				"chain": {LastVerifiedLedger: 60_000_000},
			}},
			tier:          "chain",
			explicitFrom:  2,
			safetyOverlap: 5000,
			want:          59_995_000,
		},
		{
			name: "explicit -from greater than computed → operator wins",
			state: VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{
				"chain": {LastVerifiedLedger: 60_000_000},
			}},
			tier:          "chain",
			explicitFrom:  60_000_000, // operator wants only the most recent
			safetyOverlap: 5000,
			want:          60_000_000,
		},
		{
			name: "different tier → falls through to explicit",
			state: VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{
				"chain": {LastVerifiedLedger: 60_000_000},
			}},
			tier:          "checkpoint",
			explicitFrom:  100,
			safetyOverlap: 5000,
			want:          100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveIncrementalFrom(tc.state, tc.tier, tc.explicitFrom, tc.safetyOverlap)
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestUpdateTierState confirms the only-advances-forward semantics:
// a new run that covers a lower ledger range than prior state DOES
// NOT regress the high-water mark.
func TestUpdateTierState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 14, 17, 0, 0, 0, time.UTC)
	prior := VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{
		"chain": {LastVerifiedLedger: 60_000_000, LastVerifiedHash: "old-hash"},
	}}

	// Advance: new high beats prior.
	advanced := updateTierState(prior, "chain", 62_000_000, "new-hash", now)
	if advanced.Tiers["chain"].LastVerifiedLedger != 62_000_000 {
		t.Errorf("after advance: LastVerifiedLedger = %d, want 62M",
			advanced.Tiers["chain"].LastVerifiedLedger)
	}
	if advanced.Tiers["chain"].LastVerifiedHash != "new-hash" {
		t.Errorf("hash not updated")
	}

	// Regression: new < prior is a no-op.
	noregress := updateTierState(prior, "chain", 50_000_000, "regression-hash", now)
	if noregress.Tiers["chain"].LastVerifiedLedger != 60_000_000 {
		t.Errorf("after regression attempt: LastVerifiedLedger = %d, want 60M (no regression)",
			noregress.Tiers["chain"].LastVerifiedLedger)
	}
	if noregress.Tiers["chain"].LastVerifiedHash != "old-hash" {
		t.Errorf("hash got overwritten on regression attempt")
	}

	// New tier on empty prior.
	freshTier := updateTierState(VerifyArchiveState{}, "checkpoint", 1000, "", now)
	if freshTier.Tiers["checkpoint"].LastVerifiedLedger != 1000 {
		t.Errorf("new tier insert: LastVerifiedLedger = %d, want 1000",
			freshTier.Tiers["checkpoint"].LastVerifiedLedger)
	}
}
