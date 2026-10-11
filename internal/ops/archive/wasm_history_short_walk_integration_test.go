//go:build integration

package archive

import (
	"context"
	"testing"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

// TestWasmHistoryWorker_ShortWalk_UpperEndIsLastObserved: upperEnd closes each contract's WASM range, so after a short walk (TolerateTrailingMissing) it must be the last ledger observed, never the requested bound.
func TestWasmHistoryWorker_ShortWalk_UpperEndIsLastObserved(t *testing.T) {
	const (
		from       = uint32(300)
		lastSeeded = uint32(320)
		to         = uint32(340) // 321..340 are absent from the datastore
	)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	seqs := make([]uint32, 0, lastSeeded-from+1)
	for s := from; s <= lastSeeded; s++ {
		seqs = append(seqs, s)
	}
	seedEmptyLedgers(t, ctx, dir, seqs)

	lsCfg := tolerantFilesystemLedgerstreamConfig(dir)

	var watched sdkxdr.Hash
	watched[0] = 0x0b
	watch := map[sdkxdr.Hash]string{watched: "CONTRACT-UNDER-AUDIT"}

	results, totalScanned, err := runWasmHistoryWorkers(
		ctx, lsCfg, watch, from, to,
		1,     // parallel
		false, // trackStorage
		false, // trackCode
		0,     // progressEvery (disabled)
		"",    // checkpointDir
	)
	if err != nil {
		t.Fatalf("runWasmHistoryWorkers over a tolerated trailing hole must return nil "+
			"(that is the whole point of TolerateTrailingMissing); got %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d worker results, want 1", len(results))
	}
	w := results[0]

	if w.scanned > uint64(lastSeeded-from+1) {
		t.Fatalf("worker scanned %d ledgers but only %d were seeded — fixture is wrong",
			w.scanned, lastSeeded-from+1)
	}
	if totalScanned != w.scanned {
		t.Errorf("totalScanned = %d, want %d (single worker)", totalScanned, w.scanned)
	}

	wantUpper := uint32(0)
	if w.scanned > 0 {
		wantUpper = from + uint32(w.scanned) - 1
	}
	if w.upperEnd != wantUpper {
		t.Errorf("upperEnd = %d after a short walk that delivered %d ledger(s) from %d; want %d "+
			"(the LAST LEDGER OBSERVED). Recording the requested bound %d as observed coverage "+
			"publishes a range the worker never walked.",
			w.upperEnd, w.scanned, from, wantUpper, to)
	}
	if w.upperEnd > lastSeeded {
		t.Errorf("upperEnd = %d exceeds the last ledger that exists on the datastore (%d) — "+
			"the worker cannot have observed a ledger that was never stored", w.upperEnd, lastSeeded)
	}

	// Writer → reader: feed the walk's own result through the reader
	// that publishes it. mergeWasmHistories closes a contract's open
	// range at upperEnd, and that ToLedger is what lands in the tool's
	// stdout JSON, so the coverage claim must not outrun the walk.
	w.state = map[sdkxdr.Hash]*wasmContractState{
		watched: {ranges: []wasmRange{{WasmHash: "abc123", FromLedger: from, ToLedger: 0}}},
	}
	merged := mergeWasmHistories([]workerResult{w}, watch)
	ranges := merged[watched]
	if len(ranges) != 1 {
		t.Fatalf("merged ranges = %d, want 1", len(ranges))
	}
	if ranges[0].ToLedger != wantUpper {
		t.Errorf("published coverage range ToLedger = %d, want %d — the wasm-history JSON claims "+
			"the contract was observed on that wasm hash through a ledger the walk never reached",
			ranges[0].ToLedger, wantUpper)
	}
}

// tolerantFilesystemLedgerstreamConfig adds the TolerateTrailingMissing that every ops subcommand gets
// from opsutil.NewBoundedLedgerStreamConfig, so the fixture matches the configuration operators run.
func tolerantFilesystemLedgerstreamConfig(dir string) ledgerstream.Config {
	cfg := filesystemLedgerstreamConfig(dir)
	cfg.TolerateTrailingMissing = true
	return cfg
}
