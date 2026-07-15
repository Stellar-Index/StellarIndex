package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// VerifyArchiveState is the persisted on-disk record of how far each
// verification tier has successfully covered. Read at the start of an
// incremental run to compute the lower bound; written periodically
// during the run (per-chunk done-tracking) and on clean exit (final
// high-water mark).
//
// File format is JSON (small, hand-editable by operators). Stored at
// the path the operator passes to -state-file — typically
// /var/lib/stellarindex/verify-archive-state.json on r1.
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

	// InProgress carries per-chunk completion state for an
	// interrupted parallel run. Set when a run starts; updated as
	// chunks complete; cleared on clean end-to-end completion.
	//
	// Lets a SIGTERMed run resume by skipping already-Done chunks
	// rather than restarting from genesis. A run that's never been
	// interrupted on this tier will have nil here (the monotonic
	// LastVerifiedLedger above is sufficient for the next
	// incremental fire).
	InProgress *RunProgress `json:"in_progress,omitempty"`
}

// RunProgress is the per-chunk completion state for one in-flight
// parallel run. Resume requires the From/To/Workers triple to match
// the next run's plan exactly — if the operator changes -from or the
// resolved -to drifts (live ingest moved tip), InProgress is ignored
// and the run starts fresh.
type RunProgress struct {
	// From / To / Workers identify the run's plan; resume only
	// applies when these match the next run's plan.
	From    uint32 `json:"from"`
	To      uint32 `json:"to"`
	Workers int    `json:"workers"`

	StartedAt time.Time       `json:"started_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Chunks    []ChunkProgress `json:"chunks"`
}

// ChunkProgress is one chunk's completion state. Done flips to true
// after the chunk's walker emits its last verified ledger. Mid-chunk
// progress is NOT tracked — restart of an in-flight chunk starts
// from the chunk's From, not from any saved mid-chunk LastSeq. (Full
// mid-chunk resume needs a -resume-from-hash anchor per chunk; left
// for a future revision if the loss of in-flight-chunk work proves
// too costly. With 12 chunks each loses ≤ 1/12 of total work.)
type ChunkProgress struct {
	Idx  int    `json:"idx"`
	From uint32 `json:"from"`
	To   uint32 `json:"to"`
	Done bool   `json:"done"`
	// LastVerifiedHash is the hex hash of the chunk's final
	// (chunk.to) ledger, captured when Done flips true. Used for
	// the cross-run chain-continuity proof.
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
// doesn't have to pre-create /var/lib/stellarindex.
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
// same range twice) leave it alone. Always clears the InProgress
// field — a "this run completed cleanly" signal.
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
	}
	// Clear in-progress on every clean exit — the high-water mark
	// is the durable signal; in-progress is a per-interrupted-run
	// scratch surface.
	prior.InProgress = nil
	out.Tiers[tier] = prior
	return out
}

// startTierProgress seeds a tier's InProgress section with the given
// run plan. Called once at the start of a run before any chunks
// complete. Returns a fresh state value.
func startTierProgress(st VerifyArchiveState, tier string, from, to uint32, workers int, chunks []opsutil.RangeChunk, now time.Time) VerifyArchiveState {
	out := VerifyArchiveState{Tiers: make(map[string]VerifyArchiveTierState, len(st.Tiers)+1)}
	for k, v := range st.Tiers {
		out.Tiers[k] = v
	}
	prior := out.Tiers[tier]
	cp := make([]ChunkProgress, len(chunks))
	for i, c := range chunks {
		cp[i] = ChunkProgress{Idx: i, From: c.From, To: c.To}
	}
	prior.InProgress = &RunProgress{
		From:      from,
		To:        to,
		Workers:   workers,
		StartedAt: now,
		UpdatedAt: now,
		Chunks:    cp,
	}
	out.Tiers[tier] = prior
	return out
}

// markChunkDone flips one chunk's Done flag in the tier's InProgress
// section and updates UpdatedAt. No-op if InProgress isn't set or
// idx is out of range. Caller is expected to serialise calls.
func markChunkDone(st VerifyArchiveState, tier string, idx int, lastHash sdkxdr.Hash, now time.Time) VerifyArchiveState {
	out := VerifyArchiveState{Tiers: make(map[string]VerifyArchiveTierState, len(st.Tiers))}
	for k, v := range st.Tiers {
		out.Tiers[k] = v
	}
	prior, ok := out.Tiers[tier]
	if !ok || prior.InProgress == nil || idx < 0 || idx >= len(prior.InProgress.Chunks) {
		return out
	}
	// Copy the InProgress + chunks slice so we don't mutate the
	// input's underlying arrays (maps were shallow-copied above).
	rp := *prior.InProgress
	rp.Chunks = append([]ChunkProgress(nil), prior.InProgress.Chunks...)
	rp.Chunks[idx].Done = true
	rp.Chunks[idx].LastVerifiedHash = hashToHex(lastHash)
	rp.UpdatedAt = now
	prior.InProgress = &rp
	out.Tiers[tier] = prior
	return out
}

// pinnedTipFromPriorRun returns the prior in-progress run's pinned
// `To` ledger when a follow-up fire should adopt it instead of
// re-resolving the live tip. Matches when the prior InProgress is
// present for this tier, its From and Workers match the requested
// values, and its To is non-zero.
//
// The point: the systemd timer omits `-to`, so verify-archive resolves
// `-to` to the bucket's live tip at launch. Stellar adds ledgers
// constantly, so a relaunch 30 min after a SIGTERM sees a new tip.
// Without tip-pinning, `resumeChunks` then sees rp.To != to and
// discards the prior state — every chunk already marked Done gets
// re-walked from scratch.
//
// By adopting the prior run's tip, the relaunch uses the same chunk
// plan, resumeChunks matches exactly, and only the unfinished
// chunks run. The new ledgers in [old_tip, new_tip] are picked up
// by the *next* nightly fire after this one completes
// (`-from-last-verified` walks forward from the new high-water mark).
func pinnedTipFromPriorRun(st VerifyArchiveState, tier string, from uint32, workers int) (uint32, bool) {
	tierState, ok := st.Tiers[tier]
	if !ok || tierState.InProgress == nil {
		return 0, false
	}
	rp := tierState.InProgress
	if rp.From != from || rp.Workers != workers || rp.To == 0 {
		return 0, false
	}
	return rp.To, true
}

// resumeChunks filters `chunks` to just those NOT marked Done in the
// prior in-progress state, when that state's run-plan matches the
// current plan exactly. Returns (chunksToRun, resumeReason). The
// reason is a short operator-facing string for the boot log:
//
//	"no prior in-progress for this tier"     → run all chunks
//	"prior in-progress plan differs, ignoring" → run all chunks
//	"resumed N of M chunks, K already Done"   → run filtered
//
// When prior progress matches but every chunk is Done, returns
// (nil, "all chunks already Done in prior run") — the caller
// treats that as a no-op success.
func resumeChunks(st VerifyArchiveState, tier string, from, to uint32, workers int, chunks []opsutil.RangeChunk) ([]opsutil.RangeChunk, []int, string) {
	tierState, ok := st.Tiers[tier]
	if !ok || tierState.InProgress == nil {
		idxs := make([]int, len(chunks))
		for i := range chunks {
			idxs[i] = i
		}
		return chunks, idxs, "no prior in-progress for this tier"
	}
	rp := tierState.InProgress
	if rp.From != from || rp.To != to || rp.Workers != workers || len(rp.Chunks) != len(chunks) {
		idxs := make([]int, len(chunks))
		for i := range chunks {
			idxs[i] = i
		}
		return chunks, idxs, fmt.Sprintf(
			"prior in-progress plan differs (from=%d→%d to=%d→%d workers=%d→%d chunks=%d→%d), ignoring",
			rp.From, from, rp.To, to, rp.Workers, workers, len(rp.Chunks), len(chunks))
	}
	// Plan matches — filter to undone chunks.
	var keep []opsutil.RangeChunk
	var idxs []int
	doneCount := 0
	for i, c := range chunks {
		if rp.Chunks[i].Done {
			doneCount++
			continue
		}
		keep = append(keep, c)
		idxs = append(idxs, i)
	}
	if len(keep) == 0 {
		return nil, nil, fmt.Sprintf("all %d chunks already Done in prior run", len(chunks))
	}
	return keep, idxs, fmt.Sprintf("resumed %d of %d chunks, %d already Done", len(keep), len(chunks), doneCount)
}
