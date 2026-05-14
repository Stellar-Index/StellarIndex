package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// VerifyArchiveState is the persisted on-disk record of how far each
// verification tier has successfully covered. Read at the start of an
// incremental run to compute the lower bound; updated at the end on
// success.
//
// File format is JSON (small, hand-editable by operators). Stored at
// the path the operator passes to -state-file — typically
// /var/lib/ratesengine/verify-archive-state.json on r1.
//
// Atomic-write contract: writes go to <path>.tmp and rename(2) into
// place. A crash mid-write leaves the prior state intact rather than
// truncating it.
type VerifyArchiveState struct {
	Tiers map[string]VerifyArchiveTierState `json:"tiers"`
}

// VerifyArchiveTierState is per-tier state. The Tier-A chain check
// stores both the highest verified ledger sequence and its hash; the
// hash is used as -resume-from-hash on the next incremental run so
// the cross-run chain boundary is provably continuous.
type VerifyArchiveTierState struct {
	LastVerifiedLedger uint32    `json:"last_verified_ledger"`
	LastVerifiedAt     time.Time `json:"last_verified_at"`
	// LastVerifiedHash is hex-encoded sha256 of the last ledger close
	// meta whose chain was verified. Empty for tiers that don't carry
	// a hash chain (checkpoint/peers/archivist).
	LastVerifiedHash string `json:"last_verified_hash,omitempty"`
}

// readVerifyArchiveState loads state from disk. Missing file returns
// a zero state (empty Tiers map) without error — the first-ever run
// has no prior state. Malformed JSON returns an error so operators
// notice corruption instead of silently rebuilding from zero.
func readVerifyArchiveState(path string) (VerifyArchiveState, error) {
	if path == "" {
		return VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{}}, nil
	}
	// Path comes from the operator's -state-file flag; arbitrary
	// host paths are the expected interface, not a vulnerability.
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return VerifyArchiveState{Tiers: map[string]VerifyArchiveTierState{}}, nil
		}
		return VerifyArchiveState{}, fmt.Errorf("read %s: %w", path, err)
	}
	var st VerifyArchiveState
	if err := json.Unmarshal(data, &st); err != nil {
		return VerifyArchiveState{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Tiers == nil {
		st.Tiers = map[string]VerifyArchiveTierState{}
	}
	return st, nil
}

// writeVerifyArchiveState writes state to disk atomically. Creates
// parent directories on demand (mkdir -p semantics) so the operator
// doesn't have to pre-create /var/lib/ratesengine.
func writeVerifyArchiveState(path string, st VerifyArchiveState) error {
	if path == "" {
		return fmt.Errorf("state file path empty")
	}
	// 0o750 dir / 0o600 file: state file holds nothing sensitive (a
	// ledger sequence + hash) but the verify-archive runner has no
	// reason to expose it world-readable either; gosec-safe defaults.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmp, path, err)
	}
	return nil
}

// resolveIncrementalFrom computes the lower bound for an incremental
// verify-archive run. Uses the prior state's LastVerifiedLedger
// minus a safety overlap window (defaults to 5000 ledgers, ~17h at
// 12s/ledger) so any chain anomalies that snuck in just before the
// last run's high-water mark get caught on the next pass.
//
// Returns explicitFrom (the operator's -from arg) when no prior state
// exists for this tier — a fresh deployment defaults to full-archive
// from -from=2 unless the operator passes a higher value.
func resolveIncrementalFrom(st VerifyArchiveState, tier string, explicitFrom uint32, safetyOverlap uint32) uint32 {
	tierState, ok := st.Tiers[tier]
	if !ok || tierState.LastVerifiedLedger == 0 {
		return explicitFrom
	}
	if tierState.LastVerifiedLedger <= safetyOverlap {
		return 2 // ledger 1 has no predecessor; floor is 2
	}
	candidate := tierState.LastVerifiedLedger - safetyOverlap
	if candidate < explicitFrom {
		// Operator explicitly asked to go further back — honor it.
		return explicitFrom
	}
	return candidate
}

// resolveIncrementalResumeHash returns the LastVerifiedHash for a
// tier when the operator wants a strict resume-boundary check on
// the next incremental run. Empty string when no prior hash is
// recorded (first run, hash-less tier).
func resolveIncrementalResumeHash(st VerifyArchiveState, tier string) string {
	if tierState, ok := st.Tiers[tier]; ok {
		return tierState.LastVerifiedHash
	}
	return ""
}

// updateTierState merges a successful run's outcome into the prior
// state. Only advances LastVerifiedLedger forward — a partial run
// that covered [oldLow, newHigh) where newHigh > prior.LastVerifiedLedger
// bumps the high-water mark; runs that covered older ranges (or the
// same range twice) leave it alone.
//
// Returns a NEW state value with a fresh map — never mutates the
// caller's input. (Go maps are reference types; without this copy
// `updateTierState(s, ...)` would silently modify s.Tiers in place.)
func updateTierState(st VerifyArchiveState, tier string, newHighLedger uint32, newHighHash string, now time.Time) VerifyArchiveState {
	out := VerifyArchiveState{Tiers: make(map[string]VerifyArchiveTierState, len(st.Tiers)+1)}
	for k, v := range st.Tiers {
		out.Tiers[k] = v
	}
	prior := out.Tiers[tier]
	if newHighLedger > prior.LastVerifiedLedger {
		prior.LastVerifiedLedger = newHighLedger
		prior.LastVerifiedAt = now
		if newHighHash != "" {
			prior.LastVerifiedHash = newHighHash
		}
		out.Tiers[tier] = prior
	}
	return out
}
