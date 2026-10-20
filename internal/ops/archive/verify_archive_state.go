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

// VerifyArchiveState is the persisted record of how far each verification tier has covered, JSON at the -state-file path, written atomically via <path>.tmp and rename(2) so a crash keeps the prior state.
type VerifyArchiveState struct {
	Tiers map[string]VerifyArchiveTierState `json:"tiers"`
}

// VerifyArchiveTierState is per-tier state; Tier A stores the highest verified sequence and its hash, used as -resume-from-hash so the cross-run chain boundary is continuous.
type VerifyArchiveTierState struct {
	LastVerifiedLedger uint32    `json:"last_verified_ledger"`
	LastVerifiedAt     time.Time `json:"last_verified_at"`
	// LastVerifiedHash is hex-encoded sha256 of the last ledger close
	// meta whose chain was verified. Empty for tiers that don't carry
	// a hash chain (checkpoint/peers/archivist).
	LastVerifiedHash string `json:"last_verified_hash,omitempty"`

	// InProgress carries per-chunk completion state for an interrupted parallel run, so a SIGTERMed run skips Done chunks; nil if never interrupted.
	InProgress *RunProgress `json:"in_progress,omitempty"`
}

// RunProgress is the per-chunk completion state of one in-flight run; resume needs the From/To/Workers triple to match the next plan exactly, else the run starts fresh.
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

// ChunkProgress is one chunk's completion state; Done flips after its last verified ledger, and mid-chunk progress is not tracked (restart begins at the chunk's From).
type ChunkProgress struct {
	Idx  int    `json:"idx"`
	From uint32 `json:"from"`
	To   uint32 `json:"to"`
	Done bool   `json:"done"`
	// LastVerifiedHash is the hex hash of the last ledger the walk verified (Stitch.LastSeq), set when Done flips.
	// It is write-only and is not evidence for the chain-continuity proof, which needs Stitch; kept for operators reading the file by hand.
	LastVerifiedHash string `json:"last_verified_hash,omitempty"`
	// Stitch is the chunk's boundary evidence, captured when Done flips; a resumed run skips the walk so it is the only source for the chunk's boundaries.
	// Nil means planResumedWalk refuses to skip the chunk and re-walks it.
	Stitch *ChunkStitch `json:"stitch,omitempty"`
}

// ChunkStitch is the part of a chunkResult stitchChunks consumes, persisted so a skipped chunk still supplies its side of a boundary; Verified separates a legitimately empty chunk from unknown hashes.
type ChunkStitch struct {
	Verified      int    `json:"verified"`
	FirstSeq      uint32 `json:"first_seq,omitempty"`
	FirstPrevHash string `json:"first_prev_hash,omitempty"`
	LastSeq       uint32 `json:"last_seq,omitempty"`
	LastHash      string `json:"last_hash,omitempty"`
	// CheckpointChecked records whether the producing run had the checkpoint tier active; false means the Checkpoints* counts are "never counted", not "zero misses", and planResumedWalk must not skip on them.
	CheckpointChecked     bool `json:"checkpoint_checked,omitempty"`
	CheckpointsOK         int  `json:"checkpoints_ok,omitempty"`
	CheckpointsMissed     int  `json:"checkpoints_missed,omitempty"`
	CheckpointsUnmirrored int  `json:"checkpoints_unmirrored,omitempty"`
}

// readVerifyArchiveState loads state from disk. A missing file returns a zero state (first-ever run); malformed JSON returns an error so
// corruption is noticed, not silently rebuilt from zero.
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

// writeVerifyArchiveState writes state atomically, creating parent directories on demand so the operator needn't pre-create
// /var/lib/stellarindex.
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

// resolveIncrementalFrom computes the incremental lower bound: the prior LastVerifiedLedger minus a safety overlap (default 5000 ledgers) so late anomalies are re-checked.
// With no prior state for the tier it returns explicitFrom.
func resolveIncrementalFrom(st VerifyArchiveState, tier string, explicitFrom uint32, safetyOverlap uint32) uint32 {
	return resolveIncrementalFromTiers(st, incrementalStateTiers(tier), explicitFrom, safetyOverlap)
}

// incrementalStateTiers maps -tier onto the state tier keys bounding an incremental run.
// `-tier all` records under both "chain" and "checkpoint" and never writes a key named "all", so reading under the raw flag would always miss and restart from genesis.
func incrementalStateTiers(tier string) []string {
	if tier == "all" {
		return []string{"chain", "checkpoint"}
	}
	return []string{tier}
}

// resolveIncrementalFromTiers generalises resolveIncrementalFrom over several tiers, taking the MINIMUM high-water because resuming from the higher mark would skip checkpoint-anchoring in between.
// Any covered tier with no prior state forces explicitFrom.
func resolveIncrementalFromTiers(st VerifyArchiveState, tiers []string, explicitFrom uint32, safetyOverlap uint32) uint32 {
	bound, ok := incrementalBoundTier(st, tiers)
	if !ok {
		return explicitFrom
	}
	return incrementalFromWatermark(bound.LastVerifiedLedger, explicitFrom, safetyOverlap)
}

// incrementalBoundTier returns the state entry that BOUNDS an incremental run over tiers (the lowest LastVerifiedLedger); ok=false when
// any covered tier has no prior verified ledger.
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
		// Strict boundary mode resumes at the ledger AFTER the last verified one, so the first chunk's FirstPrevHash is the saved LastVerifiedHash; starting at it would mismatch with `-safety-overlap 0`.
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

// resolveIncrementalResumeHash returns the LastVerifiedHash of the tier that bounds the run (see incrementalBoundTier), or "" if none is recorded.
// When the bounding tier records no hash (checkpoint never does) the strict check is skipped rather than run against the wrong ledger's hash.
func resolveIncrementalResumeHash(st VerifyArchiveState, tier string) string {
	bound, ok := incrementalBoundTier(st, incrementalStateTiers(tier))
	if !ok {
		return ""
	}
	return bound.LastVerifiedHash
}

// updateTierState merges a successful run into the prior state and returns a NEW value with a fresh map (Go maps are references; mutating st.Tiers would leak into the caller).
// It only advances LastVerifiedLedger forward and always clears InProgress.
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

// startTierProgress seeds a tier's InProgress section with the run plan, once before any chunk completes; returns a fresh state value.
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

// markChunkDone flips one chunk's Done flag in the tier's InProgress and updates UpdatedAt; a no-op if InProgress is unset or idx out of
// range. Callers must serialise calls.
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

// markChunkDoneStitch is markChunkDone plus the chunk's boundary evidence. Without it a resumed run cannot check the boundaries around a
// skipped chunk
// and stitchChunks would silently compare non-adjacent chunks.
func markChunkDoneStitch(st VerifyArchiveState, tier string, idx int, res chunkResult, now time.Time, doCheckpoint bool) VerifyArchiveState {
	out := markChunkDone(st, tier, idx, res.LastHash, now)
	ts, ok := out.Tiers[tier]
	if !ok || ts.InProgress == nil || idx < 0 || idx >= len(ts.InProgress.Chunks) {
		return out
	}
	// markChunkDone already deep-copied the RunProgress and its chunk
	// slice, so writing through the pointer touches only `out`.
	stitch := &ChunkStitch{
		Verified:      res.Verified,
		FirstSeq:      res.FirstSeq,
		FirstPrevHash: hashToHex(res.FirstPrevHash),
		LastSeq:       res.LastSeq,
		LastHash:      hashToHex(res.LastHash),
	}
	if doCheckpoint {
		stitch.CheckpointChecked = true
		stitch.CheckpointsOK = res.CheckpointsOK
		stitch.CheckpointsMissed = res.CheckpointsMissed
		stitch.CheckpointsUnmirrored = res.CheckpointsUnmirrored
	}
	ts.InProgress.Chunks[idx].Stitch = stitch
	return out
}

// chunkResult rebuilds the persisted boundary terms into the shape stitchChunks consumes; idx is the chunk's ORIGINAL plan index, the
// label a boundary failure is reported under.
func (s ChunkStitch) chunkResult(idx int, c ChunkProgress) (chunkResult, error) {
	res := chunkResult{
		Idx:                   idx,
		From:                  c.From,
		To:                    c.To,
		FirstSeq:              s.FirstSeq,
		LastSeq:               s.LastSeq,
		Verified:              s.Verified,
		CheckpointsOK:         s.CheckpointsOK,
		CheckpointsMissed:     s.CheckpointsMissed,
		CheckpointsUnmirrored: s.CheckpointsUnmirrored,
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

// pinnedTipFromPriorRun returns the prior in-progress run's pinned `To` when this tier's From and Workers match and To is non-zero.
// The timer omits `-to`, so a relaunch would see a newer tip, make resumeChunks discard the prior plan and re-walk Done chunks; adopting the old tip avoids that, and the next fire picks up the new ledgers.
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

// resumeChunks filters chunks to those not Done in the prior in-progress state when its plan matches exactly, returning the chunks, their indexes and a boot-log reason.
// It only answers what the prior run finished; whether that may be skipped is planResumedWalk's call.
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

// planResumedWalk narrows resumeChunks to what this run can still prove, and never returns an empty plan. Done only means the
// walk returned no error (stitch, anchor and high-water proofs come after), so an all-Done run with InProgress left behind is
// re-walked, not certified; a chunk is skipped only if its boundary evidence was persisted.
func planResumedWalk(st VerifyArchiveState, tier string, from, to uint32, workers int, chunks []opsutil.RangeChunk, doCheckpoint bool) ([]opsutil.RangeChunk, []int, string) {
	keep, idxs, reason := resumeChunks(st, tier, from, to, workers, chunks)
	if len(keep) == 0 {
		return chunks, allChunkIdxs(chunks), reason +
			" — re-walking the full plan: a Done marker records only that the chunk's own walk " +
			"succeeded, not that the cross-chunk stitch or the checkpoint-anchor decision ran"
	}
	prior := priorChunkProgress(st, tier)
	var unstitchable int
	for i := range chunks {
		if i >= len(prior) || !prior[i].Done {
			continue
		}
		// No boundary evidence at all — chain stitch cannot check
		// this chunk's edges.
		if prior[i].Stitch == nil {
			unstitchable++
			continue
		}
		// The prior Done marker did not check checkpoints, so its Checkpoints* counts mean "never counted", not "zero misses"; skipping would drop its contribution.
		if doCheckpoint && !prior[i].Stitch.CheckpointChecked {
			unstitchable++
		}
	}
	if unstitchable == 0 {
		return keep, idxs, reason
	}
	return chunks, allChunkIdxs(chunks), fmt.Sprintf(
		"%s — but %d of them recorded no cross-chunk boundary evidence (or none covering this run's "+
			"checkpoint tier), so skipping them would leave their contribution unchecked; re-walking the full plan",
		reason, unstitchable)
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

// fullPlanStitchInput returns one chunkResult per chunk of the original plan: live results for walked chunks, persisted boundary evidence for skipped ones.
// Handing stitchChunks only the chunks that ran would compare non-adjacent chunks on resume, missing a boundary or reporting a spurious gap. liveIdxs[i] is the plan index of liveResults[i].
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
