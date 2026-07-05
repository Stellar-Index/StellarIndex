package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/ledgerstream"
)

func wasmHistory(args []string) error { //nolint:funlen,gocognit,gocyclo // linear diagnostic, splitting reduces readability
	fs := flag.NewFlagSet("wasm-history", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 2, "First ledger sequence (inclusive)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive). Required when -parallel > 1.")
	contractsCSV := fs.String("contracts", "",
		"Comma-separated contract C-strkey IDs to watch (required, at least one)")
	bucket := fs.String("bucket", "",
		"Galexie bucket name. Default: cfg.Storage.S3BucketArchive.")
	progressEvery := fs.Uint("progress-every", 100_000, "Emit progress lines to stderr every N ledgers")
	parallel := fs.Uint("parallel", 1,
		"Number of concurrent worker ranges. Range [from,to] is split into "+
			"N contiguous chunks. Each worker has its own ledgerstream + dispatcher; "+
			"results are merged at the end. Worth setting >1 for ranges of 1M+ ledgers.")
	checkpointDir := fs.String("checkpoint-dir", "",
		"Optional directory to write per-worker JSONL transition logs into. "+
			"Each transition (one wasm-hash change for one watched contract) is "+
			"appended as one line: {contract, wasm_hash, at_ledger}. Useful for "+
			"long-running walks where the final JSON output is at risk if any "+
			"worker dies mid-flight (the JSON is only written at full completion). "+
			"Files are named <dir>/wasm-history-w<worker>.jsonl. Run "+
			"`stellarindex-ops wasm-history-merge-jsonl -checkpoint-dir <dir> -to N` "+
			"to recover the canonical wasm-history JSON from a partial run.")
	storageOut := fs.String("storage-rotations-out", "",
		"Optional path to write the per-watched-contract storage-rotation log "+
			"as a JSON document. When set, every Created/Updated/Restored "+
			"ContractData entry whose key is NOT LedgerKeyContractInstance is "+
			"recorded. Used to catch admin storage flips like Soroswap factory's "+
			"set_pair_wasm rotation that the wasm-hash-only walker doesn't see. "+
			"Empty = feature off (default).")
	codeOut := fs.String("code-uploads-out", "",
		"Optional path to write a JSON log of every ContractCode entry "+
			"(Created/Restored) observed in the walked range. Captures the "+
			"WASM-upload events themselves, independent of which contract "+
			"references the resulting hash. Output: [{ledger, wasm_hash, "+
			"size_bytes, change_type}]. Empty = feature off (default).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}
	if *contractsCSV == "" {
		return fmt.Errorf("-contracts is required (one or more comma-separated C-strkey IDs)")
	}
	if *parallel == 0 {
		*parallel = 1
	}
	if *parallel > 1 && *to == 0 {
		return fmt.Errorf("-parallel > 1 requires -to (workers split a bounded range)")
	}
	if *to != 0 && *to < *from {
		return fmt.Errorf("-to (%d) must be >= -from (%d)", *to, *from)
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	// Decode the watch list to fixed 32-byte hashes for cheap matching.
	watch := make(map[sdkxdr.Hash]string) // hash → C-strkey (for output)
	for _, s := range strings.Split(*contractsCSV, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		raw, err := strkey.Decode(strkey.VersionByteContract, s)
		if err != nil {
			return fmt.Errorf("invalid contract ID %q: %w", s, err)
		}
		if len(raw) != 32 {
			return fmt.Errorf("contract ID %q decoded to %d bytes, expected 32", s, len(raw))
		}
		var h sdkxdr.Hash
		copy(h[:], raw)
		watch[h] = s
	}
	if len(watch) == 0 {
		return fmt.Errorf("-contracts parsed to empty watch list")
	}

	bucketName := *bucket
	if bucketName == "" {
		bucketName = cfg.Storage.S3BucketArchive
	}
	fmt.Fprintf(os.Stderr, "wasm-history: watching %d contract(s), bucket=%s, range=[%d, %d], parallel=%d\n",
		len(watch), bucketName, *from, *to, *parallel)

	// wasm-history walks tend to scan recent ranges (audit trailing N
	// months). The trailing edge can be at the live tip; if -to
	// overshoots a not-yet-uploaded partition the walk would error
	// otherwise. newBoundedLedgerStreamConfig opts into
	// TolerateTrailingMissing for us.
	lsCfg := newBoundedLedgerStreamConfig(cfg, bucketName)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startedAt := time.Now()

	// Validate / prepare the optional checkpoint dir.
	if *checkpointDir != "" {
		if st, statErr := os.Stat(*checkpointDir); statErr != nil {
			return fmt.Errorf("-checkpoint-dir %q: %w", *checkpointDir, statErr)
		} else if !st.IsDir() {
			return fmt.Errorf("-checkpoint-dir %q is not a directory", *checkpointDir)
		}
		fmt.Fprintf(os.Stderr, "wasm-history: per-worker JSONL transition log → %s/wasm-history-w<i>.jsonl\n", *checkpointDir)
	}

	// Split the range into N contiguous chunks. Worker i gets
	// [from + i*size, from + (i+1)*size - 1] except the last
	// worker absorbs the remainder.
	trackStorage := *storageOut != ""
	trackCode := *codeOut != ""
	workerStates, totalScanned, err := runWasmHistoryWorkers(
		ctx, lsCfg, watch, uint32(*from), uint32(*to), int(*parallel), trackStorage, trackCode,
		uint64(*progressEvery), *checkpointDir)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nwasm-history: scanned %d ledgers across %d worker(s) in %s\n",
		totalScanned, *parallel, time.Since(startedAt).Round(time.Second))

	// Merge worker outputs. Each worker's per-contract ranges are
	// already in ledger-order within its chunk; concatenating in
	// worker-order produces a globally ordered list, then we collapse
	// adjacent same-hash ranges across the boundaries.
	merged := mergeWasmHistories(workerStates, watch)

	// Render: stable order by C-strkey for deterministic output.
	out := make([]contractHistory, 0, len(watch))
	for h, ranges := range merged {
		out = append(out, contractHistory{
			Contract: watch[h],
			Ranges:   ranges,
		})
	}
	// Also emit watched contracts that produced zero changes — useful
	// signal that the audit ran and saw nothing rather than was misconfigured.
	for h, name := range watch {
		if _, seen := merged[h]; !seen {
			out = append(out, contractHistory{Contract: name, Ranges: nil})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Contract < out[j].Contract })

	// Tier-2 outputs (storage rotations + code uploads) are written
	// to separate JSON files so the main wasm-history JSON shape on
	// stdout stays backward-compatible. Each tier-2 feature is opt-in
	// via its `-out` flag; when unset, no extra output is produced.
	if trackStorage {
		if err := writeStorageRotationsOutput(*storageOut, watch, workerStates); err != nil {
			return fmt.Errorf("write storage rotations: %w", err)
		}
	}
	if trackCode {
		if err := writeCodeUploadsOutput(*codeOut, workerStates); err != nil {
			return fmt.Errorf("write code uploads: %w", err)
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// writeStorageRotationsOutput merges per-worker storage-change
// slices in worker order (which is ledger order across the
// chunk-partitioned range) and writes them to path as a JSON array.
func writeStorageRotationsOutput(
	path string,
	watch map[sdkxdr.Hash]string,
	workers []workerResult,
) error {
	merged := make(map[sdkxdr.Hash][]storageChange)
	for _, w := range workers {
		for h, changes := range w.storageChanges {
			merged[h] = append(merged[h], changes...)
		}
	}
	out := make([]contractStorageHistory, 0, len(merged))
	for h, changes := range merged {
		out = append(out, contractStorageHistory{
			Contract: watch[h],
			Changes:  changes,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Contract < out[j].Contract })

	f, err := os.Create(path) //nolint:gosec // operator-supplied output path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wasm-history: wrote %d contract(s)' storage rotations to %s\n", len(out), path)
	return nil
}

// writeCodeUploadsOutput merges per-worker code-upload slices in
// worker order (= ledger order) and writes to path as a JSON array.
// Deduplicates by (ledger, hash) since the same upload can land in
// adjacent worker chunks.
func writeCodeUploadsOutput(path string, workers []workerResult) error {
	var all []codeUpload
	for _, w := range workers {
		all = append(all, w.codeUploads...)
	}
	// Sort by ledger then hash for stable output.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Ledger != all[j].Ledger {
			return all[i].Ledger < all[j].Ledger
		}
		return all[i].WasmHash < all[j].WasmHash
	})
	// Dedupe (rare across worker boundaries; cheap O(n) pass).
	dedup := all[:0]
	var prev codeUpload
	for _, u := range all {
		if u.Ledger == prev.Ledger && u.WasmHash == prev.WasmHash && u.ChangeType == prev.ChangeType {
			continue
		}
		dedup = append(dedup, u)
		prev = u
	}

	f, err := os.Create(path) //nolint:gosec // operator-supplied output path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(dedup); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wasm-history: wrote %d code upload(s) to %s\n", len(dedup), path)
	return nil
}

// wasmHistoryMergeJSONL reconstructs the canonical wasm-history JSON
// output from the per-worker JSONL transition logs that
// `wasm-history -checkpoint-dir` produced. Used to recover from a
// walk that died after writing transitions to JSONL but before
// reaching its end-of-run JSON write.
//
// Required flags:
//   - -checkpoint-dir: directory containing wasm-history-w*.jsonl files.
//   - -to:             upper-bound ledger from the original walk's range.
//     Used to close the last open range per contract.
//
// Optional:
//   - -output: path to write the merged JSON to. Empty = stdout.
//
// The merge logic mirrors what `wasmHistory` does at end-of-run
// (see [mergeWasmHistories]):
//
//  1. Read every wasm-history-w*.jsonl in lexical order (which is
//     worker order — w0, w1, …).
//  2. Per contract, collect all transitions across all workers.
//  3. Sort each contract's transitions by at_ledger. Within a single
//     worker the transitions are already in ledger-ascending order;
//     across workers, sort merges them.
//  4. Collapse adjacent same-hash transitions (a worker's first
//     observation of a contract that already has the same hash from
//     the previous worker is not a real transition).
//  5. Build wasmRange[]: each transition starts a range that closes
//     at the next transition's at_ledger - 1; the last range closes
//     at -to.
//  6. Emit the same JSON shape `wasmHistory` does.
//
// Empty-history contracts (the "ran but saw nothing" signal that
// wasmHistory emits as `{"contract":"...","ranges":null}`) are NOT
// emitted by this tool because the JSONL only carries observed
// transitions. The original walk's JSON IS the canonical artefact;
// this tool's purpose is purely "recover what we did see when the
// walk crashed."
func wasmHistoryMergeJSONL(args []string) error {
	fs := flag.NewFlagSet("wasm-history-merge-jsonl", flag.ContinueOnError)
	checkpointDir := fs.String("checkpoint-dir", "",
		"Directory containing wasm-history-w*.jsonl files (required).")
	to := fs.Uint("to", 0,
		"Upper-bound ledger from the original walk's range (required). "+
			"Closes the last open range per contract.")
	output := fs.String("output", "",
		"Output path. Empty = stdout.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *checkpointDir == "" {
		return fmt.Errorf("-checkpoint-dir is required")
	}
	if *to == 0 {
		return fmt.Errorf("-to is required (the original walk's upper bound)")
	}

	pattern := filepath.Join(*checkpointDir, "wasm-history-w*.jsonl")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob %s: %w", pattern, err)
	}
	if len(paths) == 0 {
		return fmt.Errorf("no wasm-history-w*.jsonl files in %s", *checkpointDir)
	}
	sort.Strings(paths) // lexical = worker-index order
	fmt.Fprintf(os.Stderr, "wasm-history-merge-jsonl: reading %d JSONL file(s) from %s\n",
		len(paths), *checkpointDir)

	// contract → transitions in observation order across all workers.
	transitions := make(map[string][]transitionRecord)
	totalLines := 0
	for _, path := range paths {
		n, err := readTransitionJSONL(path, transitions)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "  %s: %d transition(s)\n", filepath.Base(path), n)
		totalLines += n
	}
	fmt.Fprintf(os.Stderr, "wasm-history-merge-jsonl: %d total transition lines across %d contract(s)\n",
		totalLines, len(transitions))

	// Per contract: sort by at_ledger, collapse adjacent same-hash,
	// build ranges that close at the next transition's at_ledger - 1
	// (or at -to for the last range).
	out := make([]contractHistory, 0, len(transitions))
	for contract, trs := range transitions {
		sort.Slice(trs, func(i, j int) bool { return trs[i].AtLedger < trs[j].AtLedger })
		ranges := buildRangesFromTransitions(trs, uint32(*to))
		if len(ranges) == 0 {
			continue
		}
		out = append(out, contractHistory{Contract: contract, Ranges: ranges})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Contract < out[j].Contract })

	w := io.Writer(os.Stdout)
	if *output != "" {
		f, err := os.Create(*output) //nolint:gosec // operator-supplied output path
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if *output != "" {
		fmt.Fprintf(os.Stderr, "wasm-history-merge-jsonl: wrote %d contract(s) to %s\n", len(out), *output)
	}
	return nil
}

// readTransitionJSONL appends every record in path's JSONL to the
// per-contract slice in `transitions`. Returns the number of lines
// successfully decoded (corrupted or partial trailing lines are
// logged + skipped — a crashed walk may have left a half-written
// last line, and "recover what we have" beats "fail outright").
func readTransitionJSONL(path string, transitions map[string][]transitionRecord) (int, error) {
	// gosec G304: path comes from -checkpoint-dir glob expansion; the
	// merge tool is itself a privileged ops command that operators run
	// against operator-chosen paths.
	f, err := os.Open(path) //nolint:gosec // intentional ops-tool file read
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	count := 0
	for dec.More() {
		var r transitionRecord
		if err := dec.Decode(&r); err != nil {
			fmt.Fprintf(os.Stderr,
				"wasm-history-merge-jsonl: %s: skipping malformed/truncated line near offset %d (%v)\n",
				filepath.Base(path), dec.InputOffset(), err)
			break
		}
		transitions[r.Contract] = append(transitions[r.Contract], r)
		count++
	}
	return count, nil
}

// buildRangesFromTransitions converts a per-contract sorted
// transition slice into the wasmRange shape `wasmHistory` emits.
// Adjacent same-hash transitions are collapsed (the second one is
// just a downstream worker's first re-observation of an unchanged
// hash). The last open range closes at `to`.
func buildRangesFromTransitions(trs []transitionRecord, to uint32) []wasmRange {
	if len(trs) == 0 {
		return nil
	}
	// Collapse adjacent same-hash entries. We keep the EARLIEST
	// at_ledger for each run (matching the walker's first-observation
	// semantic). Tracking the previous hash via a local string avoids
	// the trs[i-1] index expression that gosec G602 flags as a
	// slice-bound risk.
	collapsed := trs[:0]
	prevHash := ""
	for _, r := range trs {
		if len(collapsed) > 0 && r.WasmHash == prevHash {
			continue
		}
		collapsed = append(collapsed, r)
		prevHash = r.WasmHash
	}
	out := make([]wasmRange, 0, len(collapsed))
	for i, r := range collapsed {
		rng := wasmRange{WasmHash: r.WasmHash, FromLedger: r.AtLedger}
		if i+1 < len(collapsed) {
			rng.ToLedger = collapsed[i+1].AtLedger - 1
		} else {
			rng.ToLedger = to
		}
		out = append(out, rng)
	}
	return out
}

// workerResult is what each parallel worker produces: a state map
// covering its bounded range, plus the actual upper bound it reached
// (used by merge to know where this worker's open ranges should close).
type workerResult struct {
	state map[sdkxdr.Hash]*wasmContractState
	// storageChanges is populated only when -track-storage-rotations
	// is set. Keyed by watched contract hash; per-contract slice is
	// in ledger order within the worker's chunk.
	storageChanges map[sdkxdr.Hash][]storageChange
	// codeUploads is populated only when -track-code-uploads is set.
	// Global per-worker (not per-contract); merged across workers in
	// ledger order.
	codeUploads []codeUpload
	scanned     uint64
	upperEnd    uint32 // last ledger the worker actually saw (inclusive)
}

// runWasmHistoryWorkers splits [from,to] into `parallel` contiguous
// chunks and runs each in its own goroutine. Returns per-worker
// state maps in worker-order plus the total ledgers scanned.
//
// When `checkpointDir` is non-empty, each worker also writes one
// JSONL line per observed transition to
// `<checkpointDir>/wasm-history-w<i>.jsonl`. This gives crash-
// resilience for long-running walks: if a worker dies mid-flight,
// the per-worker JSONL contains every transition it saw before the
// crash. The final stdout JSON is unchanged.
func runWasmHistoryWorkers( //nolint:funlen // worker scaffolding; long function is the cleanest expression of the tier-2 fan-out
	ctx context.Context,
	lsCfg ledgerstream.Config,
	watch map[sdkxdr.Hash]string,
	from, to uint32,
	parallel int,
	trackStorage bool,
	trackCode bool,
	progressEvery uint64,
	checkpointDir string,
) ([]workerResult, uint64, error) {
	if parallel < 1 {
		parallel = 1
	}
	results := make([]workerResult, parallel)
	for i := range results {
		results[i].state = make(map[sdkxdr.Hash]*wasmContractState)
		if trackStorage {
			results[i].storageChanges = make(map[sdkxdr.Hash][]storageChange)
		}
	}

	// Range partition. Use the unbounded form (to == 0) only when
	// parallel == 1 — the parallel path always works on bounded
	// chunks since unbounded only makes sense for live tail.
	bounds := splitRange(from, to, parallel)
	startedAt := time.Now()

	var wg sync.WaitGroup
	errCh := make(chan error, parallel)
	totalScanned := atomicUint64{}

	for i, b := range bounds {
		i, b := i, b
		wg.Add(1)
		go func() {
			defer wg.Done()
			runOneWasmHistoryWorker(ctx, lsCfg, watch, &results[i], i, b,
				progressEvery, checkpointDir, trackStorage, trackCode,
				&totalScanned, startedAt, errCh)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return nil, totalScanned.load(), err // first error wins
	}
	return results, totalScanned.load(), nil
}

// runOneWasmHistoryWorker is the per-goroutine body of
// runWasmHistoryWorkers. Extracted so the parent function's
// cognitive complexity stays manageable. Owns one worker chunk's
// scan + optional checkpoint-log lifecycle.
func runOneWasmHistoryWorker( //nolint:funlen,gocognit // worker hot path; refactor would obscure ledger-stream lifecycle
	ctx context.Context,
	lsCfg ledgerstream.Config,
	watch map[sdkxdr.Hash]string,
	result *workerResult,
	workerIdx int,
	b rangeChunk,
	progressEvery uint64,
	checkpointDir string,
	trackStorage bool,
	trackCode bool,
	totalScanned *atomicUint64,
	startedAt time.Time,
	errCh chan<- error,
) {
	result.upperEnd = b.to
	workerScanned := uint64(0)

	// Per-worker transition log (optional). nil → no incremental writes.
	var tlog *transitionLog
	if checkpointDir != "" {
		path := filepath.Join(checkpointDir, fmt.Sprintf("wasm-history-w%d.jsonl", workerIdx))
		t, terr := newTransitionLog(path, watch)
		if terr != nil {
			errCh <- fmt.Errorf("worker %d: open checkpoint %q: %w", workerIdx, path, terr)
			return
		}
		tlog = t
		defer func() {
			if cerr := tlog.Close(); cerr != nil {
				fmt.Fprintf(os.Stderr, "wasm-history: w%d close checkpoint: %v\n", workerIdx, cerr)
			}
		}()
	}

	err := ledgerstream.Stream(ctx, lsCfg, b.from, b.to,
		func(lcm sdkxdr.LedgerCloseMeta) error {
			seq := lcm.LedgerSequence()
			scanLCMForWasmChanges(lcm, watch, result.state, seq, tlog)
			if trackStorage {
				scanLCMForStorageRotations(lcm, watch, result.storageChanges, seq)
			}
			if trackCode {
				result.codeUploads = scanLCMForCodeUploads(lcm, result.codeUploads, seq)
			}
			workerScanned++
			if progressEvery > 0 && workerScanned%progressEvery == 0 {
				total := totalScanned.add(progressEvery)
				rate := float64(total) / time.Since(startedAt).Seconds()
				fmt.Fprintf(os.Stderr, "wasm-history: w%d ledger %d, total scanned %d, %.0f ledgers/s\n",
					workerIdx, seq, total, rate)
			}
			return nil
		},
	)
	result.scanned = workerScanned
	// Add the un-counted residue. F-1239 (codex audit-2026-05-12):
	// `-progress-every 0` means "disable progress output"; the
	// previous unconditional `workerScanned % progressEvery`
	// panicked on divide-by-zero AFTER the expensive ledger walk
	// had finished. Either branch: progressEvery == 0 → add the
	// full workerScanned (nothing was counted in-loop); otherwise
	// add the residue.
	if progressEvery == 0 {
		totalScanned.add(workerScanned)
	} else {
		totalScanned.add(workerScanned % progressEvery)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		errCh <- fmt.Errorf("worker %d [%d,%d]: %w", workerIdx, b.from, b.to, err)
	}
}

// rangeChunk is one worker's slice of the overall [from,to] range.
type rangeChunk struct{ from, to uint32 }

// splitRange divides [from,to] into n contiguous chunks. The last
// chunk absorbs any remainder so the union exactly covers [from,to].
//
// Degrades to a single chunk when n ≤ 1, the range is single-ledger,
// or n exceeds the range span (would otherwise produce zero-width
// chunks that the downstream walkers can't process).
func splitRange(from, to uint32, n int) []rangeChunk {
	if n <= 1 || to <= from {
		return []rangeChunk{{from, to}}
	}
	span := to - from + 1
	if uint32(n) > span {
		return []rangeChunk{{from, to}}
	}
	width := span / uint32(n)
	out := make([]rangeChunk, n)
	for i := 0; i < n; i++ {
		chunkFrom := from + uint32(i)*width
		chunkTo := chunkFrom + width - 1
		if i == n-1 {
			chunkTo = to // last chunk absorbs remainder
		}
		out[i] = rangeChunk{chunkFrom, chunkTo}
	}
	return out
}

// mergeWasmHistories combines per-worker state maps into one
// per-contract timeline. Open ranges from each worker (where the
// worker exited mid-WASM-version) are closed at the worker's upper
// bound, then the timelines are concatenated in worker-order.
// Adjacent ranges with the same hash across worker boundaries are
// collapsed into a single range.
func mergeWasmHistories(
	workers []workerResult,
	watch map[sdkxdr.Hash]string,
) map[sdkxdr.Hash][]wasmRange {
	merged := make(map[sdkxdr.Hash][]wasmRange)
	for _, w := range workers {
		for h, s := range w.state {
			// Close the worker's open range at its upper bound.
			if len(s.ranges) > 0 && s.ranges[len(s.ranges)-1].ToLedger == 0 {
				s.ranges[len(s.ranges)-1].ToLedger = w.upperEnd
			}
			existing := merged[h]
			for _, r := range s.ranges {
				if len(existing) > 0 && existing[len(existing)-1].WasmHash == r.WasmHash &&
					existing[len(existing)-1].ToLedger+1 == r.FromLedger {
					// Adjacent same-hash → extend the prior range.
					existing[len(existing)-1].ToLedger = r.ToLedger
				} else {
					existing = append(existing, r)
				}
			}
			merged[h] = existing
		}
	}
	// Reopen the LAST range of each contract — i.e. clear ToLedger
	// if it hits the very last worker's upperEnd, since "we don't
	// know yet" is more honest than "ends here" for the operator
	// reading the JSON. Actually no — the operator scoped -to
	// explicitly; closing at to is correct. Leave as-is.
	_ = watch // referenced only for godoc symmetry; merging is keyed by Hash.
	return merged
}

// atomicUint64 is a tiny helper for thread-safe counter increments
// without pulling in sync/atomic boilerplate at every call site.
type atomicUint64 struct {
	mu sync.Mutex
	v  uint64
}

func (a *atomicUint64) add(n uint64) uint64 {
	a.mu.Lock()
	a.v += n
	r := a.v
	a.mu.Unlock()
	return r
}

func (a *atomicUint64) load() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

// scanLCMForWasmChanges walks every operation's LedgerEntryChanges
// in lcm and updates state when a watched contract's instance
// executable hash changes (or first appears).
//
// Performance note: every value access on the SDK XDR types is a
// deep copy (LedgerCloseMetaV1 includes TxProcessing[] — potentially
// thousands of bytes per ledger). At this hot path we use pointer
// access exclusively — `lcm.V1`, `entry.Data.ContractData`,
// `cd.Val.Instance` — to avoid per-ledger XDR copies. An earlier
// implementation using GetV1() / GetContractData() / GetInstance()
// burned ~6 minutes of 99% CPU on a 100k-ledger sample.
func scanLCMForWasmChanges(
	lcm sdkxdr.LedgerCloseMeta,
	watch map[sdkxdr.Hash]string,
	state map[sdkxdr.Hash]*wasmContractState,
	seq uint32,
	tlog *transitionLog,
) {
	if lcm.V != 1 || lcm.V1 == nil {
		return // pre-V1 LCM (very old ledgers); no Soroban; nothing to scan
	}
	v1 := lcm.V1
	for i := range v1.TxProcessing {
		txMeta := &v1.TxProcessing[i].TxApplyProcessing
		switch {
		case txMeta.V3 != nil:
			for j := range txMeta.V3.Operations {
				changes := txMeta.V3.Operations[j].Changes
				for k := range changes {
					scanLedgerEntryChange(&changes[k], watch, state, seq, tlog)
				}
			}
		case txMeta.V4 != nil:
			for j := range txMeta.V4.Operations {
				changes := txMeta.V4.Operations[j].Changes
				for k := range changes {
					scanLedgerEntryChange(&changes[k], watch, state, seq, tlog)
				}
			}
		default:
			// V1/V2 didn't have ContractData. Skip.
			continue
		}
	}
}

// scanLCMForStorageRotations walks every operation's
// LedgerEntryChanges in lcm and records non-Instance ContractData
// changes for any watched contract. Mirrors scanLCMForWasmChanges
// but with the inverse Key.Type filter.
//
// "Storage rotation" here means any modification to a contract's
// custom storage entries (per-instance balance, factory parameter,
// admin pointer, etc.) that the wasm-history walker's
// instance-only filter ignores. Useful for catching admin storage
// flips like Soroswap factory's `set_pair_wasm` rotation.
func scanLCMForStorageRotations(
	lcm sdkxdr.LedgerCloseMeta,
	watch map[sdkxdr.Hash]string,
	out map[sdkxdr.Hash][]storageChange,
	seq uint32,
) {
	if lcm.V != 1 || lcm.V1 == nil {
		return
	}
	v1 := lcm.V1
	for i := range v1.TxProcessing {
		txMeta := &v1.TxProcessing[i].TxApplyProcessing
		switch {
		case txMeta.V3 != nil:
			for j := range txMeta.V3.Operations {
				changes := txMeta.V3.Operations[j].Changes
				for k := range changes {
					recordStorageChange(&changes[k], watch, out, seq)
				}
			}
		case txMeta.V4 != nil:
			for j := range txMeta.V4.Operations {
				changes := txMeta.V4.Operations[j].Changes
				for k := range changes {
					recordStorageChange(&changes[k], watch, out, seq)
				}
			}
		}
	}
}

// recordStorageChange appends one entry per non-Instance
// ContractData change for a watched contract. Captures the raw key
// XDR (base64) + a best-effort `key_hint` summary for human
// readability. Doesn't decode the value (kept raw to keep the
// scanner's hot path tight).
func recordStorageChange(
	change *sdkxdr.LedgerEntryChange,
	watch map[sdkxdr.Hash]string,
	out map[sdkxdr.Hash][]storageChange,
	seq uint32,
) {
	var entry *sdkxdr.LedgerEntry
	var changeType string
	switch change.Type {
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryCreated:
		entry, changeType = change.Created, "created"
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		entry, changeType = change.Updated, "updated"
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryRestored:
		entry, changeType = change.Restored, "restored"
	default:
		return
	}
	if entry == nil || entry.Data.Type != sdkxdr.LedgerEntryTypeContractData {
		return
	}
	cd := entry.Data.ContractData
	if cd == nil {
		return
	}
	// Inverse filter to scanLedgerEntryChange's: skip the Instance
	// row (already covered by wasm-history); we only want the
	// non-Instance custom-storage rows.
	if cd.Key.Type == sdkxdr.ScValTypeScvLedgerKeyContractInstance {
		return
	}
	if cd.Contract.Type != sdkxdr.ScAddressTypeScAddressTypeContract || cd.Contract.ContractId == nil {
		return
	}
	contractHash := sdkxdr.Hash(*cd.Contract.ContractId)
	if _, watched := watch[contractHash]; !watched {
		return
	}

	keyB64, err := sdkxdr.MarshalBase64(cd.Key)
	if err != nil {
		// Don't drop the row; emit a placeholder hint and zero-byte
		// key. Operator can still see *that* a change happened.
		keyB64 = ""
	}
	durability := "persistent"
	if cd.Durability == sdkxdr.ContractDataDurabilityTemporary {
		durability = "temporary"
	}
	out[contractHash] = append(out[contractHash], storageChange{
		Ledger:     seq,
		ChangeType: changeType,
		KeyXDRB64:  keyB64,
		KeyHint:    storageKeyHint(cd.Key),
		Durability: durability,
	})
}

// storageKeyHint returns a best-effort one-line human summary of
// an SCVal key so an operator skimming output can recognise common
// storage patterns (Symbol("ADMIN"), Vec[Symbol("PAIR"), Address],
// etc.) without round-tripping the base64-encoded XDR through a
// decoder. Returns "" when the key shape doesn't fit a simple
// pattern.
func storageKeyHint(k sdkxdr.ScVal) string {
	switch k.Type {
	case sdkxdr.ScValTypeScvSymbol:
		if k.Sym != nil {
			return fmt.Sprintf("symbol(%q)", string(*k.Sym))
		}
	case sdkxdr.ScValTypeScvVec:
		if k.Vec == nil || *k.Vec == nil {
			return "vec[]"
		}
		v := **k.Vec // ScVec is []ScVal under a double pointer
		if len(v) == 0 {
			return "vec[]"
		}
		// Common case: Vec starts with a Symbol that names the slot.
		if v[0].Type == sdkxdr.ScValTypeScvSymbol && v[0].Sym != nil {
			return fmt.Sprintf("vec[symbol(%q), ...×%d]", string(*v[0].Sym), len(v)-1)
		}
		return fmt.Sprintf("vec[×%d]", len(v))
	case sdkxdr.ScValTypeScvBytes:
		if k.Bytes != nil {
			return fmt.Sprintf("bytes[%d]", len(*k.Bytes))
		}
	case sdkxdr.ScValTypeScvU32:
		if k.U32 != nil {
			return fmt.Sprintf("u32(%d)", *k.U32)
		}
	}
	return ""
}

// scanLCMForCodeUploads walks LedgerEntryChanges in lcm looking
// for ContractCode entry Created/Restored events — i.e. raw WASM
// upload events emitted when someone calls UploadContractWasm.
//
// Captured globally (not per-watched-contract) because the upload
// is independent of which contract may later reference the
// resulting hash. Returns the (possibly extended) slice; caller
// reassigns to keep the per-worker accumulator in sync.
//
// We capture both Created (a fresh upload) and Restored (a TTL-
// extended upload restored from cold storage) for completeness.
// `Updated` is excluded — Soroban doesn't update ContractCode
// bytes (the bytes are immutable; only the entry's TTL changes).
func scanLCMForCodeUploads(
	lcm sdkxdr.LedgerCloseMeta,
	uploads []codeUpload,
	seq uint32,
) []codeUpload {
	if lcm.V != 1 || lcm.V1 == nil {
		return uploads
	}
	v1 := lcm.V1
	for i := range v1.TxProcessing {
		txMeta := &v1.TxProcessing[i].TxApplyProcessing
		switch {
		case txMeta.V3 != nil:
			for j := range txMeta.V3.Operations {
				changes := txMeta.V3.Operations[j].Changes
				for k := range changes {
					uploads = maybeAppendCodeUpload(&changes[k], uploads, seq)
				}
			}
		case txMeta.V4 != nil:
			for j := range txMeta.V4.Operations {
				changes := txMeta.V4.Operations[j].Changes
				for k := range changes {
					uploads = maybeAppendCodeUpload(&changes[k], uploads, seq)
				}
			}
		}
	}
	return uploads
}

// maybeAppendCodeUpload checks one LedgerEntryChange for a
// ContractCode Created/Restored event and appends to uploads if
// it's a match. Skips other change types and other entry types.
func maybeAppendCodeUpload(
	change *sdkxdr.LedgerEntryChange,
	uploads []codeUpload,
	seq uint32,
) []codeUpload {
	var entry *sdkxdr.LedgerEntry
	var changeType string
	switch change.Type {
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryCreated:
		entry, changeType = change.Created, "created"
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryRestored:
		entry, changeType = change.Restored, "restored"
	default:
		return uploads
	}
	if entry == nil || entry.Data.Type != sdkxdr.LedgerEntryTypeContractCode {
		return uploads
	}
	cc := entry.Data.ContractCode
	if cc == nil {
		return uploads
	}
	return append(uploads, codeUpload{
		Ledger:     seq,
		WasmHash:   hex.EncodeToString(cc.Hash[:]),
		SizeBytes:  len(cc.Code),
		ChangeType: changeType,
	})
}

// scanLedgerEntryChange checks one LedgerEntryChange for a
// watched-contract instance update. Updates state in place.
//
// Takes the change by pointer to avoid copying the (potentially
// deep) LedgerEntry tree on every call.
func scanLedgerEntryChange(
	change *sdkxdr.LedgerEntryChange,
	watch map[sdkxdr.Hash]string,
	state map[sdkxdr.Hash]*wasmContractState,
	seq uint32,
	tlog *transitionLog,
) {
	var entry *sdkxdr.LedgerEntry
	switch change.Type {
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryCreated:
		entry = change.Created
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		entry = change.Updated
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryRestored:
		// Restored counts as "the entry exists at this hash again" —
		// treat like Created for tracking purposes.
		entry = change.Restored
	default:
		return
	}
	if entry == nil {
		return
	}

	// Type discriminator first — most LedgerEntries are Account /
	// Trustline / Offer / etc., not ContractData. Cheap reject path.
	if entry.Data.Type != sdkxdr.LedgerEntryTypeContractData {
		return
	}
	cd := entry.Data.ContractData
	if cd == nil {
		return
	}

	// Only the LedgerKeyContractInstance row carries the executable;
	// per-storage-key data rows have unrelated keys.
	if cd.Key.Type != sdkxdr.ScValTypeScvLedgerKeyContractInstance {
		return
	}

	// Match against our watch list. ContractId is *ContractId on the
	// ScAddress union when Type == ScAddressTypeScAddressTypeContract.
	if cd.Contract.Type != sdkxdr.ScAddressTypeScAddressTypeContract {
		return
	}
	if cd.Contract.ContractId == nil {
		return
	}
	contractHash := sdkxdr.Hash(*cd.Contract.ContractId)
	if _, watched := watch[contractHash]; !watched {
		return
	}

	// The Val should be an ScContractInstance carrying an Executable.
	if cd.Val.Type != sdkxdr.ScValTypeScvContractInstance {
		return
	}
	inst := cd.Val.Instance
	if inst == nil {
		return
	}
	if inst.Executable.Type != sdkxdr.ContractExecutableTypeContractExecutableWasm {
		// Stellar-asset contracts have no WASM; skip them but record
		// a placeholder hash so the timeline is unambiguous.
		recordWasmTransition(state, contractHash, "stellar-asset", seq, tlog)
		return
	}
	if inst.Executable.WasmHash == nil {
		return
	}
	hashHex := hex.EncodeToString(inst.Executable.WasmHash[:])
	recordWasmTransition(state, contractHash, hashHex, seq, tlog)
}

// recordWasmTransition advances a contract's history when its
// executable hash differs from the previously seen one. First-seen
// opens an initial range; same-hash repeats are no-ops.
//
// When tlog is non-nil, the transition is also appended to the
// per-worker JSONL log (one line per transition) — the crash-
// resilient checkpoint mechanism. Same-hash repeats produce no
// log line either, since they're not transitions.
func recordWasmTransition(
	state map[sdkxdr.Hash]*wasmContractState,
	contract sdkxdr.Hash,
	wasmHash string,
	seq uint32,
	tlog *transitionLog,
) {
	s, ok := state[contract]
	if !ok {
		s = &wasmContractState{}
		state[contract] = s
	}
	if s.current == wasmHash {
		return // no transition
	}
	// Close the previous open range (if any).
	if s.current != "" && len(s.ranges) > 0 {
		s.ranges[len(s.ranges)-1].ToLedger = seq - 1
	}
	// Open a new range at this ledger.
	s.ranges = append(s.ranges, wasmRange{WasmHash: wasmHash, FromLedger: seq})
	s.current = wasmHash

	if tlog != nil {
		// Best-effort write — don't fail the whole walk on a log error.
		// The in-memory state remains the source of truth for the final
		// stdout JSON; the JSONL is purely for crash recovery.
		if err := tlog.append(contract, wasmHash, seq); err != nil {
			fmt.Fprintf(os.Stderr, "wasm-history: transitionlog append failed (continuing): %v\n", err)
		}
	}
}

// transitionLog is a per-worker append-only JSONL writer for
// crash-resilient walks. One line per transition observed:
//
//	{"contract": "C...", "wasm_hash": "abc...", "at_ledger": 12345}
//
// The writer is buffered (4 KiB default) and flushed every
// transition (transitions are rare relative to ledgers, so the
// flush overhead is negligible). The file is opened with O_APPEND
// so concurrent appends from multiple workers to the SAME file
// would be safe at the OS level — but each worker writes to its
// own file by convention to avoid log-line interleaving.
type transitionLog struct {
	f     *os.File
	enc   *json.Encoder
	watch map[sdkxdr.Hash]string
}

type transitionRecord struct {
	Contract string `json:"contract"`
	WasmHash string `json:"wasm_hash"`
	AtLedger uint32 `json:"at_ledger"`
}

func newTransitionLog(path string, watch map[sdkxdr.Hash]string) (*transitionLog, error) {
	// gosec G304: path comes from operator-controlled -checkpoint-dir
	// flag; the wasm-history subcommand is itself a privileged ops
	// tool that needs to write to operator-chosen paths.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // intentional ops-tool file write
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(f)
	// Default Encoder writes one line per Encode() with a trailing
	// newline — that's exactly the JSONL shape we want. No SetIndent.
	return &transitionLog{f: f, enc: enc, watch: watch}, nil
}

func (t *transitionLog) append(contract sdkxdr.Hash, wasmHash string, seq uint32) error {
	cstrkey, ok := t.watch[contract]
	if !ok {
		cstrkey = hex.EncodeToString(contract[:]) // fallback: shouldn't happen since recordWasmTransition only fires for watched contracts
	}
	return t.enc.Encode(transitionRecord{
		Contract: cstrkey,
		WasmHash: wasmHash,
		AtLedger: seq,
	})
}

func (t *transitionLog) Close() error {
	if t == nil || t.f == nil {
		return nil
	}
	return t.f.Close()
}

// ─── scan-soroban-events ─────────────────────────────────────────
