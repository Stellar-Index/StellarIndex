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

// validateFollowFlags checks the -follow/-to/-parallel combination shared by
// wasm-history and extract-wasm-from-galexie (GH-1190): -follow is the
// explicit opt-in for an unbounded live tail, so it can't be combined with
// an explicit -to, and a bounded parallel split has no meaning without a
// fixed upper bound. Pure — unit-testable without a live archive.
func validateFollowFlags(toolName string, to uint, follow bool, parallel uint) error {
	if follow && to != 0 {
		return fmt.Errorf("%s: -follow tails indefinitely and is incompatible with an explicit -to", toolName)
	}
	if follow && parallel > 1 {
		return fmt.Errorf("%s: -follow (unbounded live tail) is incompatible with -parallel > 1 (workers split a bounded range)", toolName)
	}
	return nil
}

// resolveArchiveTip opens a one-shot DataStore against the same config a
// walker will use and returns the archive's current latest ledger — the
// same primitive verify-archive's -workers>1 tip resolution and
// trim-galexie-archive's hot-tip lookup already use, applied here so
// wasm-history's -to=0 default means "the tip, once" rather than
// ledgerstream.Stream's "tail forever" (GH-1190). Unlike verify-archive's
// resolution, this one has no serial-walk fallback to demote to: a
// bounded-output walker with no upper bound would just hang, so a
// resolution failure (e.g. no bucket ListObjectsV2) is returned to the
// caller as a hard error instead.
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

	// wasm-history walks tend to scan recent ranges (audit trailing N
	// months). The trailing edge can be at the live tip; if -to
	// overshoots a not-yet-uploaded partition the walk would error
	// otherwise. newBoundedLedgerStreamConfig opts into
	// TolerateTrailingMissing for us.
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, bucketName, int(*parallel))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// GH-1190: -to 0 used to fall straight through to ledgerstream.Stream,
	// which reads to==0 as "tail live, unbounded" — a walker whose entire
	// output is one JSON document emitted at completion (see below) then
	// never emits it. -follow is the explicit opt-in for that; otherwise
	// resolve -to to a real tip once, up front, same as verify-archive's
	// -workers>1 tip resolution.
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
	if err := enc.Encode(out); err != nil {
		return err
	}

	// Coverage last, so the operator still gets the JSON (its ranges are
	// honest about what was observed) but the exit code is not. This
	// output is copied into docs/operations/wasm-audits/* and is what a
	// BackfillSafe determination rests on, so "the walk covered less of
	// the range than you asked for" has to be impossible to miss — and
	// stderr alone is easy to miss when stdout is redirected to a file,
	// which is exactly how the runbook invokes this.
	return wasmWalkCoverage(uint32(*from), resolvedTo, totalScanned, bucketName)
}

// wasmWalkCoverage turns a wasm-history walk that did not deliver its
// whole range into a hard error, naming the bucket it read. An
// unbounded walk (-to 0, the live tail) has no requested count and is
// exempt.
//
// Same defect class as chops.walkCoverage / chops.backfillCoverage /
// ingest.censusCoverage. opsutil.NewBoundedLedgerStreamConfig always
// sets TolerateTrailingMissing — deliberately, because -to here may sit
// at the live tip — so a missing partition within 65,536 ledgers of -to
// comes back as a clean walk-complete and the JSON is simply computed
// over fewer ledgers. The per-contract ranges now close at the last
// ledger actually observed rather than at -to, so the artifact no
// longer overstates its coverage; this makes the shortfall itself
// unmissable.
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

// readAllTransitionJSONL reads every path into `transitions` and
// returns the ledger to close the last open range of each contract
// at. It starts from the operator's requested `to` and is pulled
// down to the least-far extent any worker file actually proves it
// reached — a worker's own watermark line if present, else its
// highest observed transition. Without this, a worker that died
// mid-flight (the exact scenario this recovery tool exists for)
// would have its last range published as extending all the way to
// -to, an artefact nobody verified (CA2-A20-correct-3).
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

// readTransitionJSONL appends every transition record in path's JSONL to
// the per-contract slice in `transitions`. Returns the number of
// transition lines successfully decoded. Reads line-by-line (not a
// streaming json.Decoder) so a single malformed line can be skipped and
// parsing RESYNCS at the next line, rather than treating "cannot parse
// here" as "this file ends here" (GH-1199): a malformed/truncated LAST
// line is tolerated (a crashed walk may leave a half-written final
// line — "recover what we have" beats "fail outright"), but a malformed
// line anywhere else is a hard error, because this file is opened
// O_APPEND across separate wasm-history runs sharing a -checkpoint-dir
// (see newTransitionLog) and a truncated per-run start would otherwise
// look identical to legitimate crash residue, silently dropping every
// later run's transitions from the merge.
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
	b opsutil.RangeChunk,
	progressEvery uint64,
	checkpointDir string,
	trackStorage bool,
	trackCode bool,
	totalScanned *atomicUint64,
	startedAt time.Time,
	errCh chan<- error,
) {
	// upperEnd stays 0 until the walk delivers something. It is the
	// LAST LEDGER OBSERVED, not the requested bound: mergeWasmHistories
	// closes every open WASM range at it, so it becomes the ToLedger
	// the tool publishes as observed coverage. Seeding it from b.To
	// here (RLT-282) meant a walk that stopped early — a hole inside
	// TolerateTrailingMissing's 65,536-ledger window, or SIGINT through
	// the shared signal context — still claimed the whole requested
	// chunk. A worker that delivers nothing leaves it 0 and contributes
	// no state, so no range can be closed at a ledger nobody saw.
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
		errCh <- fmt.Errorf("worker %d [%d,%d]: %w", workerIdx, b.From, b.To, err)
	}
}

// mergeWasmHistories combines per-worker state maps into one
// per-contract timeline. Workers scan disjoint, ledger-ordered
// chunks (opsutil.SplitRange gives worker i the i-th contiguous
// range), so each worker's per-contract ranges are reconstructed
// back into point transitions (one per range's FromLedger) and
// concatenated in worker order — reproducing the same ordered
// transition stream a single serial (-parallel 1) walk would have
// produced. Feeding that through buildRangesFromTransitions (the
// same primitive wasm-history-merge-jsonl's crash-recovery path
// uses) collapses hash-unchanged worker boundaries correctly.
//
// The previous implementation only stitched a worker's LAST range
// into the PRECEDING worker's range when the next worker's first
// transition landed exactly on the chunk boundary — true only when
// the version changed on the very first ledger of a chunk. Any
// worker chunk with no instance write at all (the contract's hash
// simply continued unchanged) contributed nothing, leaving a
// silent hole in the reported timeline (CA2-A20-harden-4).
//
// The final range's close point is the LAST worker's upperEnd — the
// true last ledger observed by the whole walk — not the operator's
// requested -to, matching the RLT-282 fix already applied to each
// worker's own open range.
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
// flush overhead is negligible). The file is O_TRUNC'd on open
// (GH-1199): -checkpoint-dir is a stable, operator-chosen directory
// reused across INDEPENDENT wasm-history invocations, and each
// invocation is a fresh walk, not a resume of a prior run's — leaving
// the old O_APPEND meant a second run's lines landed after a first
// run's partial/crashed tail, and wasm-history-merge-jsonl's
// last-line-only tolerance then silently dropped everything the
// second run wrote.
type transitionLog struct {
	f     *os.File
	enc   *json.Encoder
	watch map[sdkxdr.Hash]string
}

type transitionRecord struct {
	Contract string `json:"contract,omitempty"`
	WasmHash string `json:"wasm_hash,omitempty"`
	AtLedger uint32 `json:"at_ledger"`
	// Watermark marks this line as the worker's own record of how
	// far it actually scanned (AtLedger holds that ledger), rather
	// than a WASM-hash transition. Contract/WasmHash are empty on a
	// watermark line.
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

// setExtent appends a watermark line recording how far this worker
// actually scanned (seq, its true upperEnd) — distinct from any
// transition. wasmHistoryMergeJSONL needs this because a JSONL file
// otherwise records only WHERE transitions landed, never HOW FAR the
// worker got: a worker that crashed without seeing another
// transition after ledger X would leave the merge tool no way to
// distinguish "scanned through -to, hash never changed again" from
// "crashed at X, everything after is unknown" (CA2-A20-correct-3).
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
