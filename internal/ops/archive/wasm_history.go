package archive

import (
	"bufio"
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
	"github.com/stellar/go-stellar-sdk/support/datastore"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// validateFollowFlags checks the -follow/-to/-parallel combination: -follow is the opt-in for an unbounded tail, so it excludes an explicit -to,
// and a parallel split needs a fixed upper bound. Pure.
func validateFollowFlags(toolName string, to uint, follow bool, parallel uint) error {
	if follow && to != 0 {
		return fmt.Errorf("%s: -follow tails indefinitely and is incompatible with an explicit -to", toolName)
	}
	if follow && parallel > 1 {
		return fmt.Errorf("%s: -follow (unbounded live tail) is incompatible with -parallel > 1 (workers split a bounded range)", toolName)
	}
	return nil
}

// resolveArchiveTip returns the archive's latest ledger once, so wasm-history's -to=0 means the tip rather than tailing forever.
// Unlike verify-archive there is no serial fallback: a bounded walker with no upper bound would hang, so failure is a hard error.
func resolveArchiveTip(ctx context.Context, lsCfg ledgerstream.Config) (uint32, error) {
	ds, err := datastore.NewDataStore(ctx, lsCfg.DataStore)
	if err != nil {
		return 0, fmt.Errorf("open datastore: %w", err)
	}
	defer func() { _ = ds.Close() }()
	tip, err := datastore.FindLatestLedgerSequence(ctx, ds)
	if err != nil {
		return 0, fmt.Errorf("find latest ledger: %w", err)
	}
	return tip, nil
}

func wasmHistory(args []string) error { //nolint:funlen,gocognit,gocyclo // linear diagnostic, splitting reduces readability
	fs := flag.NewFlagSet("wasm-history", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 2, "First ledger sequence (inclusive)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive). 0 = resolve the archive tip once at startup (requires bucket ListObjectsV2). Required explicitly when -parallel > 1. For an unbounded LIVE TAIL instead, pass -follow.")
	follow := fs.Bool("follow", false, "Tail the live chain indefinitely from -from instead of resolving -to to a fixed tip (GH-1190: -to 0 used to mean live-tail implicitly, so the default invocation tailed pubnet forever and never wrote the output JSON, which is only emitted at completion). Incompatible with -parallel > 1 and with any -to.")
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
	if err := validateFollowFlags("wasm-history", *to, *follow, *parallel); err != nil {
		return err
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

	// Walks often end at the live tip; newBoundedLedgerStreamConfig opts into TolerateTrailingMissing so an overshooting -to does not error.
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, bucketName, int(*parallel))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ledgerstream.Stream reads to==0 as an unbounded live tail, so a walker emitting one JSON at completion would never emit; -follow opts in,
	// otherwise resolve -to to a real tip once.
	resolvedTo := uint32(*to)
	if *to == 0 && !*follow {
		tip, tipErr := resolveArchiveTip(ctx, lsCfg)
		if tipErr != nil {
			return fmt.Errorf("wasm-history: resolve -to=0 to archive tip (pass -follow for an unbounded live tail instead): %w", tipErr)
		}
		resolvedTo = tip
		fmt.Fprintf(os.Stderr, "wasm-history: resolved -to=0 → tip %d\n", resolvedTo)
	}

	fmt.Fprintf(os.Stderr, "wasm-history: watching %d contract(s), bucket=%s, range=[%d, %d], parallel=%d\n",
		len(watch), bucketName, *from, resolvedTo, *parallel)

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
		ctx, lsCfg, watch, uint32(*from), resolvedTo, int(*parallel), trackStorage, trackCode,
		uint64(*progressEvery), *checkpointDir)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nwasm-history: scanned %d ledgers across %d worker(s) in %s\n",
		totalScanned, *parallel, time.Since(startedAt).Round(time.Second))

	// Merge worker outputs. Each worker's per-contract ranges are already in ledger-order within its chunk; concatenating in
	// worker-order produces a globally ordered list, then we collapse adjacent same-hash ranges across the boundaries.
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

	// Tier-2 outputs (storage rotations + code uploads) are written to separate JSON files so the main wasm-history JSON shape
	// on stdout stays backward-compatible. Each tier-2 feature is opt-in via its `-out` flag; when unset, no extra output is
	// produced.
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
	if err := enc.Encode(out); err != nil {
		return err
	}

	// Coverage last: the operator still gets the JSON, but the exit code fails. The output feeds BackfillSafe determinations,
	// and stderr alone is easy to miss when stdout is redirected.
	return wasmWalkCoverage(uint32(*from), resolvedTo, totalScanned, bucketName)
}

// wasmWalkCoverage turns a walk that did not cover its whole range into a hard error naming the bucket (unbounded -to is exempt):
// TolerateTrailingMissing makes a missing partition near -to look
// like a clean walk, and per-contract ranges close at the last observed ledger, so the shortfall must be unmissable.
func wasmWalkCoverage(from, to uint32, scanned uint64, bucket string) error {
	if to == 0 {
		return nil
	}
	requested := uint64(to) - uint64(from) + 1
	if scanned == requested {
		return nil
	}
	if scanned == 0 {
		return fmt.Errorf(
			"wasm-history scanned 0 ledgers in [%d, %d] from bucket %q — nothing was examined, so "+
				"every watched contract is reported as having no transitions; historical ranges need "+
				"-bucket galexie-archive. Refusing to pass vacuously",
			from, to, bucket)
	}
	return fmt.Errorf(
		"wasm-history scanned %d of %d requested ledgers in [%d, %d] from bucket %q — the ranges above "+
			"close at the LAST LEDGER OBSERVED, not at -to, so this audit covers less than the range you "+
			"named; historical ranges need -bucket galexie-archive",
		scanned, requested, from, to, bucket)
}

// writeStorageRotationsOutput merges per-worker storage-change slices in worker order (ledger order across the chunk-partitioned range)
// and writes them to path as a JSON array.
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

// writeCodeUploadsOutput merges per-worker code-upload slices in worker order (= ledger order) and writes a JSON array, deduplicating by
// (ledger, hash) since one upload can land in adjacent worker chunks.
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

// wasmHistoryMergeJSONL rebuilds the canonical JSON from per-worker JSONL logs (-checkpoint-dir, -to closing the last open range,
// -output), recovering a walk that died before its final write.
// Mirrors [mergeWasmHistories]; empty-history contracts are not emitted and only transitions seen before the crash are recovered.
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

	transitions := make(map[string][]transitionRecord)
	closeAt, err := readAllTransitionJSONL(paths, uint32(*to), transitions)
	if err != nil {
		return err
	}

	// Per contract: sort by at_ledger, collapse adjacent same-hash,
	// build ranges that close at the next transition's at_ledger - 1
	// (or at closeAt for the last range).
	out := make([]contractHistory, 0, len(transitions))
	for contract, trs := range transitions {
		sort.Slice(trs, func(i, j int) bool { return trs[i].AtLedger < trs[j].AtLedger })
		ranges := buildRangesFromTransitions(trs, closeAt)
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

// readAllTransitionJSONL reads every path into transitions and returns the ledger closing each contract's last open range: the requested
// `to`, pulled down to the least-far extent any worker file proves
// (watermark or highest transition), so a crashed worker's last range is not published out to an unverified -to.
func readAllTransitionJSONL(paths []string, to uint32, transitions map[string][]transitionRecord) (uint32, error) {
	totalLines := 0
	closeAt := to
	sawExtent := false
	for _, path := range paths {
		n, extent, hasExtent, err := readTransitionJSONL(path, transitions)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "  %s: %d transition(s)\n", filepath.Base(path), n)
		totalLines += n
		if !hasExtent && n == 0 {
			continue // no information at all from this file
		}
		if !sawExtent || extent < closeAt {
			closeAt = extent
			sawExtent = true
		}
	}
	fmt.Fprintf(os.Stderr, "wasm-history-merge-jsonl: %d total transition lines across %d contract(s)\n",
		totalLines, len(transitions))
	if sawExtent && closeAt < to {
		fmt.Fprintf(os.Stderr,
			"wasm-history-merge-jsonl: observed extent %d is short of requested -to %d — "+
				"closing ranges at the last ledger actually observed, not -to\n",
			closeAt, to)
	}
	return closeAt, nil
}

// readTransitionJSONL appends a file's transition records per contract and returns the decoded count. A malformed LAST line is tolerated
// (a crash can half-write it; the log is truncated on open);
// a malformed line elsewhere is corruption and a hard error, since skipping it would silently drop later transitions.
func readTransitionJSONL(path string, transitions map[string][]transitionRecord) (count int, extent uint32, hasExtent bool, err error) {
	// gosec G304: path comes from -checkpoint-dir glob expansion; the
	// merge tool is itself a privileged ops command that operators run
	// against operator-chosen paths.
	f, err := os.Open(path) //nolint:gosec // intentional ops-tool file read
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = f.Close() }()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20) // transition lines are small; allow generous headroom
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if serr := scanner.Err(); serr != nil {
		return 0, 0, false, fmt.Errorf("scan %s: %w", filepath.Base(path), serr)
	}

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r transitionRecord
		if jerr := json.Unmarshal([]byte(line), &r); jerr != nil {
			lineNo := i + 1
			fmt.Fprintf(os.Stderr,
				"wasm-history-merge-jsonl: %s: malformed/truncated line %d (%v)\n",
				filepath.Base(path), lineNo, jerr)
			if i != len(lines)-1 {
				return count, extent, hasExtent, fmt.Errorf(
					"%s: line %d is malformed and is NOT the file's last line — mid-file corruption, not crash residue; refusing to silently drop the transitions after it",
					filepath.Base(path), lineNo)
			}
			continue // malformed LAST line: tolerated as a crash-truncated tail
		}
		if r.Watermark {
			extent = r.AtLedger
			hasExtent = true
			continue
		}
		transitions[r.Contract] = append(transitions[r.Contract], r)
		count++
		if !hasExtent && r.AtLedger > extent {
			extent = r.AtLedger
		}
	}
	return count, extent, hasExtent, nil
}

// buildRangesFromTransitions converts sorted per-contract transitions into wasmRange values, collapsing adjacent same-hash entries;
// the last open range closes at `to`.
func buildRangesFromTransitions(trs []transitionRecord, to uint32) []wasmRange {
	if len(trs) == 0 {
		return nil
	}
	// Collapse adjacent same-hash entries, keeping the EARLIEST at_ledger (first observation). A local previous-hash avoids the trs[i-1] gosec G602 flag.
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

// workerResult is what each parallel worker produces: a state map covering its bounded range plus the upper bound it reached (merge closes
// this worker's open ranges there).
type workerResult struct {
	state map[sdkxdr.Hash]*wasmContractState
	// storageChanges is populated only with -track-storage-rotations; keyed by watched contract hash, each slice in ledger order within the
	// worker's chunk.
	storageChanges map[sdkxdr.Hash][]storageChange
	// codeUploads is populated only with -track-code-uploads; global per worker (not per contract), merged across workers in ledger order.
	codeUploads []codeUpload
	scanned     uint64
	upperEnd    uint32 // last ledger the worker actually saw (inclusive)
}

// runWasmHistoryWorkers splits [from,to] into `parallel` contiguous chunks, one goroutine each, and returns per-worker state in worker order
// plus total ledgers scanned. With checkpointDir set each worker also appends transitions to wasm-history-w<i>.jsonl for crash recovery.
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
	bounds := opsutil.SplitRange(from, to, parallel)
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

// runOneWasmHistoryWorker is the per-goroutine body of runWasmHistoryWorkers (split for cognitive complexity); owns one chunk's scan plus
// optional checkpoint-log lifecycle.
func runOneWasmHistoryWorker( //nolint:funlen,gocognit // worker hot path; refactor would obscure ledger-stream lifecycle
	ctx context.Context,
	lsCfg ledgerstream.Config,
	watch map[sdkxdr.Hash]string,
	result *workerResult,
	workerIdx int,
	b opsutil.RangeChunk,
	progressEvery uint64,
	checkpointDir string,
	trackStorage bool,
	trackCode bool,
	totalScanned *atomicUint64,
	startedAt time.Time,
	errCh chan<- error,
) {
	// upperEnd stays 0 until the walk delivers: it is the LAST LEDGER OBSERVED, not the requested bound, because mergeWasmHistories closes open ranges at it.
	// Seeding it from b.To would let a walk that stopped early claim the whole chunk; a worker delivering nothing contributes no state.
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

	err := ledgerstream.Stream(ctx, lsCfg, b.From, b.To,
		func(lcm sdkxdr.LedgerCloseMeta) error {
			seq := lcm.LedgerSequence()
			result.upperEnd = seq
			if err := scanLCMForWasmChanges(lcm, watch, result.state, seq, tlog); err != nil {
				return err
			}
			if trackStorage {
				if err := scanLCMForStorageRotations(lcm, watch, result.storageChanges, seq); err != nil {
					return err
				}
			}
			if trackCode {
				uploads, err := scanLCMForCodeUploads(lcm, result.codeUploads, seq)
				if err != nil {
					return err
				}
				result.codeUploads = uploads
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
	if tlog != nil {
		// Record the true extent BEFORE the deferred Close runs, so
		// a crash-recovery merge knows how far this worker got even
		// if it saw zero transitions in its whole chunk.
		if werr := tlog.setExtent(result.upperEnd); werr != nil {
			fmt.Fprintf(os.Stderr, "wasm-history: w%d write extent watermark: %v\n", workerIdx, werr)
		}
	}
	// Add the un-counted residue. -progress-every 0 disables progress output, and `% progressEvery` would panic after the walk finished.
	if progressEvery == 0 {
		totalScanned.add(workerScanned)
	} else {
		totalScanned.add(workerScanned % progressEvery)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		errCh <- fmt.Errorf("worker %d [%d,%d]: %w", workerIdx, b.From, b.To, err)
	}
}

// mergeWasmHistories combines per-worker state into one per-contract timeline. Workers scan disjoint ordered chunks, so their
// ranges become point transitions concatenated in worker order; buildRangesFromTransitions collapses unchanged hashes at
// boundaries, and the final range closes at the LAST worker's upperEnd (last ledger observed), not -to.
func mergeWasmHistories(
	workers []workerResult,
	watch map[sdkxdr.Hash]string,
) map[sdkxdr.Hash][]wasmRange {
	if len(workers) == 0 {
		return nil
	}
	closeAt := workers[len(workers)-1].upperEnd

	byContract := make(map[sdkxdr.Hash][]transitionRecord)
	for _, w := range workers {
		for h, s := range w.state {
			name := watch[h]
			for _, r := range s.ranges {
				byContract[h] = append(byContract[h], transitionRecord{
					Contract: name,
					WasmHash: r.WasmHash,
					AtLedger: r.FromLedger,
				})
			}
		}
	}

	merged := make(map[sdkxdr.Hash][]wasmRange, len(byContract))
	for h, trs := range byContract {
		sort.Slice(trs, func(i, j int) bool { return trs[i].AtLedger < trs[j].AtLedger })
		merged[h] = buildRangesFromTransitions(trs, closeAt)
	}
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

// scanLCMForWasmChanges updates state when a watched contract's instance executable hash changes or first appears.
// Hot path: use pointer access (lcm.V1, entry.Data.ContractData, cd.Val.Instance), since SDK XDR value accessors deep-copy per ledger.
func scanLCMForWasmChanges(
	lcm sdkxdr.LedgerCloseMeta,
	watch map[sdkxdr.Hash]string,
	state map[sdkxdr.Hash]*wasmContractState,
	seq uint32,
	tlog *transitionLog,
) error {
	if lcm.V < 0 || lcm.V > 2 {
		return fmt.Errorf("ledger %d: unsupported LedgerCloseMeta.V=%d", seq, lcm.V)
	}
	for i := 0; i < lcm.CountTransactions(); i++ {
		txMeta := lcm.TxApplyProcessing(i)
		switch txMeta.V {
		case 0, 1, 2:
			// Predate Soroban ContractData changes. Skip.
			continue
		case 3:
			for j := range txMeta.V3.Operations {
				changes := txMeta.V3.Operations[j].Changes
				for k := range changes {
					scanLedgerEntryChange(&changes[k], watch, state, seq, tlog)
				}
			}
		case 4:
			for j := range txMeta.V4.Operations {
				changes := txMeta.V4.Operations[j].Changes
				for k := range changes {
					scanLedgerEntryChange(&changes[k], watch, state, seq, tlog)
				}
			}
		default:
			// Unknown/future TransactionMeta arm: fail loudly rather
			// than silently miss WASM changes it might carry.
			return fmt.Errorf("ledger %d tx %d: unsupported TransactionMeta.V=%d", seq, i, txMeta.V)
		}
	}
	return nil
}

// scanLCMForStorageRotations records non-Instance ContractData changes (custom storage such as admin flips) for watched contracts,
// the inverse Key.Type filter of scanLCMForWasmChanges.
func scanLCMForStorageRotations(
	lcm sdkxdr.LedgerCloseMeta,
	watch map[sdkxdr.Hash]string,
	out map[sdkxdr.Hash][]storageChange,
	seq uint32,
) error {
	if lcm.V < 0 || lcm.V > 2 {
		return fmt.Errorf("ledger %d: unsupported LedgerCloseMeta.V=%d", seq, lcm.V)
	}
	for i := 0; i < lcm.CountTransactions(); i++ {
		txMeta := lcm.TxApplyProcessing(i)
		switch txMeta.V {
		case 0, 1, 2:
			continue
		case 3:
			for j := range txMeta.V3.Operations {
				changes := txMeta.V3.Operations[j].Changes
				for k := range changes {
					recordStorageChange(&changes[k], watch, out, seq)
				}
			}
		case 4:
			for j := range txMeta.V4.Operations {
				changes := txMeta.V4.Operations[j].Changes
				for k := range changes {
					recordStorageChange(&changes[k], watch, out, seq)
				}
			}
		default:
			return fmt.Errorf("ledger %d tx %d: unsupported TransactionMeta.V=%d", seq, i, txMeta.V)
		}
	}
	return nil
}

// recordStorageChange appends one entry per non-Instance ContractData change: the raw key XDR (base64) and a best-effort key_hint; the value is not decoded.
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

// storageKeyHint returns a best-effort one-line summary of an SCVal key for human skimming, or "" when the shape is not simple.
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

// scanLCMForCodeUploads records ContractCode Created/Restored entries (WASM uploads) globally, since an upload is independent of which contract later
// references the hash. Updated is excluded: ContractCode bytes are immutable. Returns the possibly extended slice; the caller must reassign it.
func scanLCMForCodeUploads(
	lcm sdkxdr.LedgerCloseMeta,
	uploads []codeUpload,
	seq uint32,
) ([]codeUpload, error) {
	if lcm.V < 0 || lcm.V > 2 {
		return uploads, fmt.Errorf("ledger %d: unsupported LedgerCloseMeta.V=%d", seq, lcm.V)
	}
	for i := 0; i < lcm.CountTransactions(); i++ {
		txMeta := lcm.TxApplyProcessing(i)
		switch txMeta.V {
		case 0, 1, 2:
			continue
		case 3:
			for j := range txMeta.V3.Operations {
				changes := txMeta.V3.Operations[j].Changes
				for k := range changes {
					uploads = maybeAppendCodeUpload(&changes[k], uploads, seq)
				}
			}
		case 4:
			for j := range txMeta.V4.Operations {
				changes := txMeta.V4.Operations[j].Changes
				for k := range changes {
					uploads = maybeAppendCodeUpload(&changes[k], uploads, seq)
				}
			}
		default:
			return uploads, fmt.Errorf("ledger %d tx %d: unsupported TransactionMeta.V=%d", seq, i, txMeta.V)
		}
	}
	return uploads, nil
}

// maybeAppendCodeUpload appends to uploads when one LedgerEntryChange is a ContractCode Created/Restored event; other change and entry
// types are skipped.
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

// scanLedgerEntryChange checks one change for a watched-contract instance update, updating state in place; it takes the change by pointer to avoid deep copies.
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
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		recordInstanceRemoval(change.Removed, watch, state, seq, tlog)
		return
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

// instanceRemovedMarker closes a watched contract's hash range when its instance entry is deleted, so the timeline does not claim the last
// WASM stayed live; a later Created/Restored reopens a range.
const instanceRemovedMarker = "removed"

// recordInstanceRemoval records a watched contract's instance-entry deletion.
func recordInstanceRemoval(
	key *sdkxdr.LedgerKey,
	watch map[sdkxdr.Hash]string,
	state map[sdkxdr.Hash]*wasmContractState,
	seq uint32,
	tlog *transitionLog,
) {
	if key == nil || key.Type != sdkxdr.LedgerEntryTypeContractData || key.ContractData == nil {
		return
	}
	kcd := key.ContractData
	if kcd.Key.Type != sdkxdr.ScValTypeScvLedgerKeyContractInstance ||
		kcd.Contract.Type != sdkxdr.ScAddressTypeScAddressTypeContract ||
		kcd.Contract.ContractId == nil {
		return
	}
	contractHash := sdkxdr.Hash(*kcd.Contract.ContractId)
	if _, watched := watch[contractHash]; !watched {
		return
	}
	recordWasmTransition(state, contractHash, instanceRemovedMarker, seq, tlog)
}

// recordWasmTransition advances a contract's history when its executable hash differs from the last seen (first-seen opens a range, repeats are no-ops),
// and appends the transition to the per-worker JSONL log when tlog is non-nil.
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

// transitionLog is a per-worker JSONL writer, flushed per transition, for crash-resilient walks. The file is O_TRUNC'd on open because -checkpoint-dir is reused
// across INDEPENDENT invocations; with O_APPEND a crashed tail would make merge-jsonl's last-line-only tolerance drop the next run's lines.
type transitionLog struct {
	f     *os.File
	enc   *json.Encoder
	watch map[sdkxdr.Hash]string
}

type transitionRecord struct {
	Contract string `json:"contract,omitempty"`
	WasmHash string `json:"wasm_hash,omitempty"`
	AtLedger uint32 `json:"at_ledger"`
	// Watermark marks this line as the worker's own record of how far it actually scanned (AtLedger holds that ledger), rather
	// than a WASM-hash transition. Contract/WasmHash are empty on a watermark line.
	Watermark bool `json:"watermark,omitempty"`
}

func newTransitionLog(path string, watch map[sdkxdr.Hash]string) (*transitionLog, error) {
	// gosec G304: path comes from operator-controlled -checkpoint-dir
	// flag; the wasm-history subcommand is itself a privileged ops
	// tool that needs to write to operator-chosen paths.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // intentional ops-tool file write
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

// setExtent appends a watermark line with how far this worker scanned, which wasmHistoryMergeJSONL needs to tell scanned-through-to from crashed-at-X.
func (t *transitionLog) setExtent(seq uint32) error {
	return t.enc.Encode(transitionRecord{AtLedger: seq, Watermark: true})
}

func (t *transitionLog) Close() error {
	if t == nil || t.f == nil {
		return nil
	}
	return t.f.Close()
}

// ─── scan-soroban-events ─────────────────────────────────────────
