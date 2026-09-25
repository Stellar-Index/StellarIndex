package archive

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// extractWasmFromGalexie pulls raw WASM bytes for one or more
// Soroban-contract WASM hashes by walking the local Galexie LCM
// archive — the truer source than RPC `getLedgerEntry` because:
//
//  1. It works for evicted WASMs (TTL-expired bytes are no longer
//     in active ledger state but ARE preserved in galexie LCM).
//  2. It doesn't depend on a public RPC's retention policy.
//  3. It runs offline against r1's full archive.
//
// For each target hash, the tool scans LCM in [from, to] looking
// for a `LedgerEntryChange` whose `Data.Type == ContractCode` and
// whose `ContractCode.Hash` matches the target. The WASM bytes
// (`ContractCode.Code`) are written to `<output-dir>/<hash>.wasm`.
//
// Each WASM is uploaded to chain via an `INVOKE_HOST_FUNCTION` op
// of type `HOST_FUNCTION_TYPE_UPLOAD_CONTRACT_WASM`, which on
// success creates a `ContractCode` LedgerEntry. We catch this
// `Created` change. Restored entries (a TTL-extension of an
// already-installed WASM) also carry the bytes, so we accept both.
//
// CLI usage:
//
//	stellarindex-ops extract-wasm-from-galexie \
//	    -config /etc/stellarindex.toml \
//	    -hashes <hex,hex,...> \
//	    -output-dir /var/wasm-audit \
//	    [-from N] [-to N] [-parallel N]
//
// Defaults: from=2 (genesis is fine; the walker skips pre-Soroban
// LCMs). to=0 means walk to archive tip — the caller should set
// it explicitly when running parallel.
func extractWasmFromGalexie(args []string) error { //nolint:funlen,gocognit,gocyclo // linear diagnostic, splitting reduces readability
	fs := flag.NewFlagSet("extract-wasm-from-galexie", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	hashesCSV := fs.String("hashes", "",
		"Comma-separated WASM hashes (32-byte hex strings) to extract (required)")
	outputDir := fs.String("output-dir", "",
		"Directory to write <hash>.wasm files into (required, must exist)")
	from := fs.Uint("from", 2, "First ledger sequence (inclusive)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive). 0 = resolve the archive tip once at startup (requires bucket ListObjectsV2). Required explicitly when -parallel > 1. For an unbounded LIVE TAIL instead, pass -follow.")
	follow := fs.Bool("follow", false, "Tail the live chain indefinitely from -from instead of resolving -to to a fixed tip (GH-1190: -to 0 used to mean live-tail implicitly, so an absent hash's MISSING/exit-non-zero path was unreachable — the walk never ended). Incompatible with -parallel > 1 and with any -to.")
	bucket := fs.String("bucket", "",
		"Galexie bucket name. Default: cfg.Storage.S3BucketArchive.")
	parallel := fs.Uint("parallel", 1,
		"Worker count. Range [from,to] is split into N contiguous chunks; first worker "+
			"to find a hash wins. Use >1 for ranges of 1M+ ledgers.")
	progressEvery := fs.Uint("progress-every", 100_000,
		"Emit progress lines to stderr every N ledgers")
	earlyExit := fs.Bool("early-exit", true,
		"Stop walking once every requested hash has been found. "+
			"Set false to verify a hash appears at multiple ledgers (rare diagnostic).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}
	if *hashesCSV == "" {
		return fmt.Errorf("-hashes is required (one or more comma-separated 64-hex-char WASM hashes)")
	}
	if *outputDir == "" {
		return fmt.Errorf("-output-dir is required")
	}
	if *parallel == 0 {
		*parallel = 1
	}
	if err := validateFollowFlags("extract-wasm", *to, *follow, *parallel); err != nil {
		return err
	}
	if *parallel > 1 && *to == 0 {
		return fmt.Errorf("-parallel > 1 requires -to (workers split a bounded range)")
	}
	if *to != 0 && *to < *from {
		return fmt.Errorf("-to (%d) must be >= -from (%d)", *to, *from)
	}

	if st, err := os.Stat(*outputDir); err != nil {
		return fmt.Errorf("-output-dir %q: %w", *outputDir, err)
	} else if !st.IsDir() {
		return fmt.Errorf("-output-dir %q is not a directory", *outputDir)
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	// Decode the hash watch list to fixed 32-byte arrays.
	wantHashes := make(map[sdkxdr.Hash]struct{})
	wantHexes := make(map[sdkxdr.Hash]string) // hash → original hex (for output filename)
	for _, h := range strings.Split(*hashesCSV, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		raw, herr := hex.DecodeString(h)
		if herr != nil {
			return fmt.Errorf("invalid hash %q: %w", h, herr)
		}
		if len(raw) != 32 {
			return fmt.Errorf("hash %q decoded to %d bytes, expected 32", h, len(raw))
		}
		var hh sdkxdr.Hash
		copy(hh[:], raw)
		wantHashes[hh] = struct{}{}
		wantHexes[hh] = h
	}
	if len(wantHashes) == 0 {
		return fmt.Errorf("-hashes parsed to empty set")
	}

	bucketName := *bucket
	if bucketName == "" {
		bucketName = cfg.Storage.S3BucketArchive
	}

	lsCfg := ledgerstream.Config{
		DataStore: datastore.DataStoreConfig{
			Type: "S3",
			Params: map[string]string{
				"destination_bucket_path": bucketName,
				"region":                  cfg.Storage.S3Region,
				"endpoint_url":            cfg.Storage.S3Endpoint,
			},
			NetworkPassphrase: cfg.Stellar.Passphrase(),
			Compression:       "zstd",
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// GH-1190: -to 0 used to fall straight through to ledgerstream.Stream
	// (unbounded live tail), so an absent hash's "MISSING ... exit
	// non-zero" path was unreachable — the walk never ended. -follow is
	// the explicit opt-in for that; otherwise resolve -to to a real tip
	// once, up front (same helper wasm-history uses).
	resolvedTo := uint32(*to)
	if *to == 0 && !*follow {
		tip, tipErr := resolveArchiveTip(ctx, lsCfg)
		if tipErr != nil {
			return fmt.Errorf("extract-wasm: resolve -to=0 to archive tip (pass -follow for an unbounded live tail instead): %w", tipErr)
		}
		resolvedTo = tip
		fmt.Fprintf(os.Stderr, "extract-wasm: resolved -to=0 → tip %d\n", resolvedTo)
	}

	fmt.Fprintf(os.Stderr, "extract-wasm: looking for %d hash(es), bucket=%s, range=[%d, %d], parallel=%d\n",
		len(wantHashes), bucketName, *from, resolvedTo, *parallel)

	// Shared "found" map — once a worker writes a hash, others can
	// skip it. Atomic per-hash via mutex around the whole map.
	var foundMu sync.Mutex
	found := make(map[sdkxdr.Hash]string) // hash → output path
	startedAt := time.Now()

	bounds := opsutil.SplitRange(uint32(*from), resolvedTo, int(*parallel))
	var wg sync.WaitGroup
	errCh := make(chan error, len(bounds))
	totalScanned := atomicUint64{}

	for i, b := range bounds {
		i, b := i, b
		wg.Add(1)
		go func() {
			defer wg.Done()
			workerScanned := uint64(0)
			err := ledgerstream.Stream(ctx, lsCfg, b.From, b.To,
				func(lcm sdkxdr.LedgerCloseMeta) error {
					seq := lcm.LedgerSequence()
					if err := scanLCMForWasmCode(lcm, wantHashes, *outputDir, wantHexes, &foundMu, found); err != nil {
						return err
					}
					workerScanned++
					if *progressEvery > 0 && workerScanned%uint64(*progressEvery) == 0 {
						total := totalScanned.add(uint64(*progressEvery))
						rate := float64(total) / time.Since(startedAt).Seconds()
						foundMu.Lock()
						foundCount := len(found)
						foundMu.Unlock()
						fmt.Fprintf(os.Stderr, "extract-wasm: w%d ledger %d, total scanned %d, %.0f ledgers/s, found %d/%d\n",
							i, seq, total, rate, foundCount, len(wantHashes))
					}
					if *earlyExit {
						foundMu.Lock()
						done := len(found) == len(wantHashes)
						foundMu.Unlock()
						if done {
							cancel()
							return context.Canceled
						}
					}
					return nil
				},
			)
			// F-1239 (codex audit-2026-05-12): `-progress-every 0`
			// must mean "disable progress" without panicking on
			// the residue add post-walk.
			if *progressEvery == 0 {
				totalScanned.add(workerScanned)
			} else {
				totalScanned.add(workerScanned % uint64(*progressEvery))
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("worker %d [%d,%d]: %w", i, b.From, b.To, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	// First non-cancel worker error wins; printed AND returned (GH-1189) —
	// dropping it after printing let a stream failure (wrong bucket, MinIO
	// down) exit 0 with "N hash(es) not found — try a wider range" instead
	// of reporting the walk itself never completed.
	var workerErr error
	for err := range errCh {
		if workerErr == nil {
			workerErr = err
		}
		fmt.Fprintf(os.Stderr, "extract-wasm: ERROR %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "\nextract-wasm: scanned %d ledgers in %s; wrote %d/%d hash(es)\n",
		totalScanned.load(), time.Since(startedAt).Round(time.Second),
		len(found), len(wantHashes))

	// Report missing hashes and exit non-zero so callers can detect
	// partial completion.
	missing := 0
	for h, hex := range wantHexes {
		if _, ok := found[h]; !ok {
			fmt.Fprintf(os.Stderr, "  MISSING %s\n", hex)
			missing++
		}
	}
	switch {
	case workerErr != nil && missing > 0:
		return fmt.Errorf("worker error: %w (also %d hash(es) not found in [%d,%d])", workerErr, missing, *from, resolvedTo)
	case workerErr != nil:
		return workerErr
	case missing > 0:
		return fmt.Errorf("%d hash(es) not found in [%d,%d] — try a wider range", missing, *from, resolvedTo)
	}
	return nil
}

// scanLCMForWasmCode walks every operation's LedgerEntryChanges
// in this LCM, looking for ContractCode entries whose hash matches
// the watch set. The first match per hash wins; subsequent matches
// (from Restored events at later ledgers) are silently skipped to
// avoid clobbering and to allow concurrent workers to be lock-free
// on the write path.
func scanLCMForWasmCode(
	lcm sdkxdr.LedgerCloseMeta,
	wantHashes map[sdkxdr.Hash]struct{},
	outputDir string,
	wantHexes map[sdkxdr.Hash]string,
	foundMu *sync.Mutex,
	found map[sdkxdr.Hash]string,
) error {
	seq := lcm.LedgerSequence()
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
					maybeWriteWasmCode(&changes[k], wantHashes, outputDir, wantHexes, foundMu, found)
				}
			}
		case 4:
			for j := range txMeta.V4.Operations {
				changes := txMeta.V4.Operations[j].Changes
				for k := range changes {
					maybeWriteWasmCode(&changes[k], wantHashes, outputDir, wantHexes, foundMu, found)
				}
			}
		default:
			return fmt.Errorf("ledger %d tx %d: unsupported TransactionMeta.V=%d", seq, i, txMeta.V)
		}
	}
	return nil
}

// maybeWriteWasmCode inspects a single LedgerEntryChange. If it's a
// Created or Restored ContractCode entry whose hash matches a target
// AND the target hasn't been written yet, the WASM bytes are
// written to <outputDir>/<hash>.wasm.
//
// The lock granularity is the whole `found` map; this is fine because
// (a) most LCMs touch zero ContractCode entries, (b) writes are
// rare per-walk, and (c) the lock-free hot path is just the
// LedgerEntryType discriminator check.
func maybeWriteWasmCode(
	change *sdkxdr.LedgerEntryChange,
	wantHashes map[sdkxdr.Hash]struct{},
	outputDir string,
	wantHexes map[sdkxdr.Hash]string,
	foundMu *sync.Mutex,
	found map[sdkxdr.Hash]string,
) {
	// Match every change type that carries a [LedgerEntry] body
	// (everything except LEDGER_ENTRY_REMOVED, which carries a
	// LedgerKey instead). Earlier versions of this function only
	// looked at Created + Restored, but the wasm-history walker
	// finds ContractInstance updates under Updated too — and
	// audit experience (2026-05-01 r1 walk) showed extract-wasm
	// returning MISSING for every hash in the same archive the
	// wasm-history walker reads cleanly. State is the pre-image
	// of an Updated change in V2/V3 LCMs; if a ContractCode entry
	// already exists at the target hash and is being TTL-extended
	// or otherwise touched, the bytes are still in the State /
	// Updated entry body.
	var entry *sdkxdr.LedgerEntry
	switch change.Type {
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryCreated:
		entry = change.Created
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		entry = change.Updated
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryState:
		entry = change.State
	case sdkxdr.LedgerEntryChangeTypeLedgerEntryRestored:
		entry = change.Restored
	default:
		return
	}
	if entry == nil {
		return
	}
	if entry.Data.Type != sdkxdr.LedgerEntryTypeContractCode {
		return
	}
	cc := entry.Data.ContractCode
	if cc == nil {
		return
	}
	if _, want := wantHashes[cc.Hash]; !want {
		return
	}

	foundMu.Lock()
	if _, already := found[cc.Hash]; already {
		foundMu.Unlock()
		return
	}
	// Claim the hash before writing (still under the lock) so a concurrent
	// worker matching the same hash later (e.g. a Restored entry at a
	// later ledger) doesn't race the write, but don't record it as FOUND
	// yet: found is the walk's completion receipt (drives -early-exit, the
	// wrote N/N summary, and the missing-hash exit code), and GH-1189 was
	// exactly this receipt being issued before the write it attests to had
	// happened. An empty value marks "claimed, write in flight".
	found[cc.Hash] = ""
	foundMu.Unlock()

	hexHash := wantHexes[cc.Hash]
	outPath := filepath.Join(outputDir, hexHash+".wasm")
	tmpPath := outPath + ".tmp"
	writeErr := os.WriteFile(tmpPath, cc.Code, 0o600)
	if writeErr == nil {
		writeErr = os.Rename(tmpPath, outPath)
	}

	foundMu.Lock()
	if writeErr != nil {
		delete(found, cc.Hash) // unclaim so a later occurrence can retry
		foundMu.Unlock()
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "extract-wasm: ERROR writing %s: %v\n", outPath, writeErr)
		return
	}
	found[cc.Hash] = outPath
	foundMu.Unlock()
	fmt.Fprintf(os.Stderr, "extract-wasm: wrote %s (%d bytes)\n", outPath, len(cc.Code))
}
