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
	// (chunk.to) ledger, captured when Done flips true.
	//
	// It is written and never read. The comment it replaces said it
	// was "used for the cross-run chain-continuity proof", which was
	// not true of any code path (RLT-265): one terminal hash cannot
	// prove a boundary, which needs the RIGHT chunk's FirstPrevHash
	// as well. Stitch carries both terms and is what the proof
	// actually reads. Kept because an operator reading the state file
	// by hand uses it, and dropping it would change the on-disk shape
	// for no gain; do not mistake it for evidence.
	LastVerifiedHash string `json:"last_verified_hash,omitempty"`
	// Stitch is the chunk's boundary evidence, captured when Done
	// flips true. A resumed run skips this chunk's walk, so these
	// are the only terms from which the two boundaries the chunk
	// participates in can still be checked (RLT-265). Nil for a
	// chunk recorded by a binary that predates it — planResumedWalk
	// refuses to skip such a chunk, so the boundary is re-derived by
	// re-walking rather than assumed.
	Stitch *ChunkStitch `json:"stitch,omitempty"`
}

// ChunkStitch is the subset of a chunkResult that stitchChunks
// consumes, persisted so a chunk skipped on resume can still supply
// its side of a cross-chunk boundary.
//
// Verified distinguishes a chunk that legitimately saw zero ledgers
// (the range predates the bucket) from one whose hashes are simply
// unknown: stitchChunks re-targets a boundary at the nearest NON-empty
// neighbours, so an empty chunk must be reconstructed as empty rather
// than dropped.
type ChunkStitch struct {
	Verified      int    `json:"verified"`
	FirstSeq      uint32 `json:"first_seq,omitempty"`
	FirstPrevHash string `json:"first_prev_hash,omitempty"`
	LastSeq       uint32 `json:"last_seq,omitempty"`
	LastHash      string `json:"last_hash,omitempty"`
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
	return resolveIncrementalFromTiers(st, incrementalStateTiers(tier), explicitFrom, safetyOverlap)
}

// incrementalStateTiers maps the operator's -tier flag onto the state
// -file tier key(s) whose high-water marks bound an incremental run.
//
// DAT-09: `-tier all` runs the chain AND checkpoint passes and records
// its outcome under BOTH the "chain" and "checkpoint" keys — it never
// writes a key literally named "all". Reading incremental state under
// the raw flag value therefore always missed, so every nightly
// `-tier all -from-last-verified` run silently restarted from genesis
// (and, bounded by -max-runtime, never reached the trailing edge it
// was scheduled to verify).
func incrementalStateTiers(tier string) []string {
	if tier == "all" {
		return []string{"chain", "checkpoint"}
	}
	return []string{tier}
}

// resolveIncrementalFromTiers is resolveIncrementalFrom generalised
// over the several state tiers one -tier value can cover. The bound is
// the MINIMUM high-water across them — a `-tier chain` run can advance
// "chain" past "checkpoint", and resuming from the higher mark would
// skip checkpoint-anchoring the range in between. Any covered tier
// with no prior state at all forces explicitFrom: nothing is verified
// on that side yet, so there is no watermark to resume from.
func resolveIncrementalFromTiers(st VerifyArchiveState, tiers []string, explicitFrom uint32, safetyOverlap uint32) uint32 {
	bound, ok := incrementalBoundTier(st, tiers)
	if !ok {
		return explicitFrom
	}
	return incrementalFromWatermark(bound.LastVerifiedLedger, explicitFrom, safetyOverlap)
}

// incrementalBoundTier returns the state entry that BOUNDS an
// incremental run over tiers — the one with the lowest
// LastVerifiedLedger. ok=false when any covered tier has no prior
// verified ledger.
func incrementalBoundTier(st VerifyArchiveState, tiers []string) (VerifyArchiveTierState, bool) {
	var bound VerifyArchiveTierState
	found := false
	for _, t := range tiers {
		ts, ok := st.Tiers[t]
		if !ok || ts.LastVerifiedLedger == 0 {
			return VerifyArchiveTierState{}, false
		}
		if !found || ts.LastVerifiedLedger < bound.LastVerifiedLedger {
			bound = ts
			found = true
		}
	}
	return bound, found
}

// incrementalFromWatermark applies the safety-overlap arithmetic to a
// resolved high-water mark.
func incrementalFromWatermark(lastVerified, explicitFrom, safetyOverlap uint32) uint32 {
	if lastVerified == 0 {
		return explicitFrom
	}
	var candidate uint32
	switch {
	case safetyOverlap == 0:
		// Strict cross-run boundary mode (REL-05). Resume at the ledger
		// AFTER the last verified one, so the first chunk's
		// FirstPrevHash — the hash of effectiveFrom-1 — IS the saved
		// LastVerifiedHash the resume-from-hash check compares against.
		// Starting AT last-verified re-walks one ledger (an overlap of
		// 1, not 0) and made that check compare the hash AT
		// last-verified against the hash of last-verified-1, so
		// `-safety-overlap 0` failed every run with a bogus
		// "resume-from-hash boundary mismatch".
		candidate = lastVerified + 1
	case lastVerified <= safetyOverlap:
		return 2 // ledger 1 has no predecessor; floor is 2
	default:
		candidate = lastVerified - safetyOverlap
	}
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
//
// For a multi-tier -tier value the hash comes from the tier that
// BOUNDS the run (see incrementalBoundTier) — the same tier
// resolveIncrementalFrom derived effectiveFrom from, so the hash is
// provably the one at effectiveFrom-1. When the bounding tier records
// no hash (the checkpoint tier never does) this is empty and the
// strict check is simply skipped rather than run against the wrong
// ledger's hash.
func resolveIncrementalResumeHash(st VerifyArchiveState, tier string) string {
	bound, ok := incrementalBoundTier(st, incrementalStateTiers(tier))
	if !ok {
		return ""
	}
	return bound.LastVerifiedHash
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

// markChunkDoneStitch is markChunkDone plus the chunk's boundary
// evidence — the form the walk uses. Without the evidence a resumed
// run cannot check the boundaries either side of the chunk it skips,
// and stitchChunks silently compares non-adjacent chunks instead
// (RLT-265).
func markChunkDoneStitch(st VerifyArchiveState, tier string, idx int, res chunkResult, now time.Time) VerifyArchiveState {
	out := markChunkDone(st, tier, idx, res.LastHash, now)
	ts, ok := out.Tiers[tier]
	if !ok || ts.InProgress == nil || idx < 0 || idx >= len(ts.InProgress.Chunks) {
		return out
	}
	// markChunkDone already deep-copied the RunProgress and its chunk
	// slice, so writing through the pointer touches only `out`.
	ts.InProgress.Chunks[idx].Stitch = &ChunkStitch{
		Verified:      res.Verified,
		FirstSeq:      res.FirstSeq,
		FirstPrevHash: hashToHex(res.FirstPrevHash),
		LastSeq:       res.LastSeq,
		LastHash:      hashToHex(res.LastHash),
	}
	return out
}

// chunkResult rebuilds the persisted boundary terms into the shape
// stitchChunks consumes. idx is the chunk's ORIGINAL plan index — the
// label a boundary failure is reported under.
func (s ChunkStitch) chunkResult(idx int, c ChunkProgress) (chunkResult, error) {
	res := chunkResult{
		Idx:      idx,
		From:     c.From,
		To:       c.To,
		FirstSeq: s.FirstSeq,
		LastSeq:  s.LastSeq,
		Verified: s.Verified,
	}
	if s.Verified == 0 {
		// An empty chunk carries no hashes and stitchChunks skips it
		// when choosing pairs; reconstruct it as empty.
		return res, nil
	}
	first, err := hashFromHex(s.FirstPrevHash)
	if err != nil {
		return chunkResult{}, fmt.Errorf("chunk[%d] persisted first_prev_hash: %w", idx, err)
	}
	last, err := hashFromHex(s.LastHash)
	if err != nil {
		return chunkResult{}, fmt.Errorf("chunk[%d] persisted last_hash: %w", idx, err)
	}
	res.FirstPrevHash = first
	res.LastHash = last
	return res, nil
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
// (nil, "all chunks already Done in prior run"). resumeChunks answers
// only "what did the prior run finish?" — whether any of that may
// actually be SKIPPED is planResumedWalk's call, and it re-walks the
// all-Done case rather than certifying it.
func resumeChunks(st VerifyArchiveState, tier string, from, to uint32, workers int, chunks []opsutil.RangeChunk) ([]opsutil.RangeChunk, []int, string) {
	tierState, ok := st.Tiers[tier]
	if !ok || tierState.InProgress == nil {
		return chunks, allChunkIdxs(chunks), "no prior in-progress for this tier"
	}
	rp := tierState.InProgress
	if rp.From != from || rp.To != to || rp.Workers != workers || len(rp.Chunks) != len(chunks) {
		return chunks, allChunkIdxs(chunks), fmt.Sprintf(
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

// allChunkIdxs is the identity index map — "walk every chunk in the
// plan".
func allChunkIdxs(chunks []opsutil.RangeChunk) []int {
	idxs := make([]int, len(chunks))
	for i := range chunks {
		idxs[i] = i
	}
	return idxs
}

// planResumedWalk is resumeChunks' verdict narrowed to what this run
// can still PROVE. It never returns an empty plan.
//
// "Which chunks did the prior run finish?" is not the same question as
// "which chunks may this run skip?". A chunk's Done marker records
// only that the chunk's OWN walk returned no error. Everything that
// makes a run a VERIFICATION rather than a read — the cross-chunk
// stitch, the checkpoint-anchor decision, the high-water advance —
// happens after the walk, and none of it is recorded per chunk. So a
// prior run that marked every chunk Done and still left InProgress
// behind is, by construction, a run that failed at or after those
// proofs: a real stitch break at a chunk boundary leaves exactly this
// state.
//
// Treating that as a no-op success (RLT-281) returned (0, "", nil)
// from a walk that verified ZERO ledgers, which the caller's textfile
// defer writes out as a clean run — advancing
// stellarindex_verify_archive_last_success_unix and holding the
// stellarindex_verify_archive_run_stale page green for a run that
// anchored nothing, while clearing the InProgress record that was the
// only remaining trace of the failed one.
//
// The proofs cannot be reconstructed from the Done markers (the
// checkpoint OK/missed tallies are not persisted at all), so the
// conservative reading is the only defensible one: re-walk.
//
// Second narrowing (RLT-265): a chunk may only be skipped when the
// prior run persisted its boundary evidence. A skipped chunk supplies
// no live chunkResult, so without that record the boundaries either
// side of it cannot be checked at all — and stitchChunks, handed only
// the chunks that ran, compares non-adjacent ones instead. A chunk
// recorded before the evidence existed is re-walked, which is
// self-healing: the next run records it and resume works again.
func planResumedWalk(st VerifyArchiveState, tier string, from, to uint32, workers int, chunks []opsutil.RangeChunk) ([]opsutil.RangeChunk, []int, string) {
	keep, idxs, reason := resumeChunks(st, tier, from, to, workers, chunks)
	if len(keep) == 0 {
		return chunks, allChunkIdxs(chunks), reason +
			" — re-walking the full plan: a Done marker records only that the chunk's own walk " +
			"succeeded, not that the cross-chunk stitch or the checkpoint-anchor decision ran"
	}
	prior := priorChunkProgress(st, tier)
	var unstitchable int
	for i := range chunks {
		if i < len(prior) && prior[i].Done && prior[i].Stitch == nil {
			unstitchable++
		}
	}
	if unstitchable == 0 {
		return keep, idxs, reason
	}
	return chunks, allChunkIdxs(chunks), fmt.Sprintf(
		"%s — but %d of them recorded no cross-chunk boundary evidence, so skipping them would "+
			"leave their boundaries unchecked; re-walking the full plan", reason, unstitchable)
}

// priorChunkProgress returns the tier's recorded per-chunk progress,
// or nil when there is none.
func priorChunkProgress(st VerifyArchiveState, tier string) []ChunkProgress {
	ts, ok := st.Tiers[tier]
	if !ok || ts.InProgress == nil {
		return nil
	}
	return ts.InProgress.Chunks
}

// fullPlanStitchInput returns one chunkResult per chunk of the ORIGINAL
// plan, in plan order: the live result for every chunk this run walked,
// and the prior run's persisted boundary evidence for every chunk it
// skipped.
//
// This is what makes the cross-chunk chain proof survive a resume
// (RLT-265). stitchChunks was handed `results` — only the chunks that
// RAN — so on a resumed walk it compared chunks that are not adjacent
// in ledger space: a boundary next to a skipped chunk was either never
// checked (skipped chunk at an end of the run set) or reported as a
// spurious gap (skipped chunk in the middle). Neither is a proof.
//
// liveIdxs[i] is the original plan index of liveResults[i], as returned
// by planResumedWalk.
func fullPlanStitchInput(st VerifyArchiveState, tier string, planLen int, liveIdxs []int, liveResults []chunkResult) ([]chunkResult, error) {
	if len(liveIdxs) != len(liveResults) {
		return nil, fmt.Errorf("stitch input: %d chunk index(es) for %d result(s)", len(liveIdxs), len(liveResults))
	}
	out := make([]chunkResult, planLen)
	filled := make([]bool, planLen)
	for i, idx := range liveIdxs {
		if idx < 0 || idx >= planLen {
			return nil, fmt.Errorf("stitch input: chunk index %d outside the %d-chunk plan", idx, planLen)
		}
		out[idx] = liveResults[i]
		filled[idx] = true
	}
	prior := priorChunkProgress(st, tier)
	for i := range out {
		if filled[i] {
			continue
		}
		if i >= len(prior) || prior[i].Stitch == nil {
			// planResumedWalk does not skip a chunk without evidence,
			// so this is unreachable; refuse rather than stitch a
			// plan with a hole in it.
			return nil, fmt.Errorf("stitch input: chunk[%d] was skipped but recorded no boundary evidence", i)
		}
		res, err := prior[i].Stitch.chunkResult(i, prior[i])
		if err != nil {
			return nil, err
		}
		out[i] = res
	}
	return out, nil
}
