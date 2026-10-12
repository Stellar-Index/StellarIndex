//go:build integration

package archive

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// testPassphrase is the SDF testnet passphrase. Doesn't matter for
// hash-only tests but must be SOMETHING the SDK accepts.
const testPassphrase = "Test SDF Network ; September 2015"

// TestRunVerifyChunks_FilesystemBackend_ParallelWalk drives the chunked verify end-to-end on a filesystem datastore (8 chunks, Verified sums, stitch and chain checks pass).
func TestRunVerifyChunks_FilesystemBackend_ParallelWalk(t *testing.T) {
	const (
		from    = uint32(100)
		to      = uint32(163) // 64 ledgers — a full checkpoint range
		workers = 8
	)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	seqs := make([]uint32, 0, to-from+1)
	for s := from; s <= to; s++ {
		seqs = append(seqs, s)
	}
	seedEmptyLedgers(t, ctx, dir, seqs)

	chunks := opsutil.SplitRange(from, to, workers)
	if len(chunks) != workers {
		t.Fatalf("splitRange returned %d chunks, want %d", len(chunks), workers)
	}

	lsCfg := filesystemLedgerstreamConfig(dir)

	results, err := runVerifyChunks(
		ctx, lsCfg, chunks,
		false, // doCheckpoint — no Tier B fixtures wired in this test
		filepath.Join(dir, "no-archive-root"),
		time.Now(),
		1*time.Hour, // suppress per-chunk progress prints
		chunkOrchestratorOpts{},
	)
	if err != nil {
		t.Fatalf("runVerifyChunks: %v", err)
	}
	if len(results) != workers {
		t.Fatalf("got %d results, want %d", len(results), workers)
	}

	totalVerified := 0
	for i, r := range results {
		if r.Idx != i {
			t.Errorf("results[%d].Idx = %d, want %d", i, r.Idx, i)
		}
		if r.Mismatches != 0 {
			t.Errorf("results[%d] reports %d mismatches; chain check should pass on empty ledgers", i, r.Mismatches)
		}
		expectedSpan := chunks[i].To - chunks[i].From + 1
		if uint32(r.Verified) != expectedSpan {
			t.Errorf("results[%d] verified %d ledgers, want %d for chunk [%d, %d]",
				i, r.Verified, expectedSpan, chunks[i].From, chunks[i].To)
		}
		totalVerified += r.Verified
	}

	expectedTotal := int(to - from + 1)
	if totalVerified != expectedTotal {
		t.Errorf("total verified = %d, want %d", totalVerified, expectedTotal)
	}

	if err := stitchChunks(results); err != nil {
		t.Errorf("stitchChunks: %v — empty-ledger boundaries should match (zero hash on both sides)", err)
	}
}

// TestRunVerifyChunks_FilesystemBackend_SerialPath confirms the workers=1 single-chunk path on the same fixture.
func TestRunVerifyChunks_FilesystemBackend_SerialPath(t *testing.T) {
	const (
		from = uint32(200)
		to   = uint32(231)
	)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	seqs := make([]uint32, 0, to-from+1)
	for s := from; s <= to; s++ {
		seqs = append(seqs, s)
	}
	seedEmptyLedgers(t, ctx, dir, seqs)

	chunks := opsutil.SplitRange(from, to, 1)
	results, err := runVerifyChunks(
		ctx, filesystemLedgerstreamConfig(dir), chunks,
		false, "",
		time.Now(), 1*time.Hour,
		chunkOrchestratorOpts{},
	)
	if err != nil {
		t.Fatalf("runVerifyChunks: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Verified != int(to-from+1) {
		t.Errorf("Verified = %d, want %d", results[0].Verified, int(to-from+1))
	}
	if err := stitchChunks(results); err != nil {
		t.Errorf("single-chunk stitch should pass: %v", err)
	}
}

// TestRunVerifyChunks_CheckpointOnly_StillDetectsChainBreak: a checkpoint-only run (no "chain" tier) must still catch a mid-chunk hash break.
func TestRunVerifyChunks_CheckpointOnly_StillDetectsChainBreak(t *testing.T) {
	const (
		from = uint32(300)
		to   = uint32(310)
	)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	seqs := make([]uint32, 0, to-from+1)
	for s := from; s <= to; s++ {
		seqs = append(seqs, s)
	}
	// Ledger 305's PreviousLedgerHash is corrupted — every other
	// ledger keeps the trivially-matching zero hash.
	const brokenSeq = uint32(305)
	seedLedgersWithBrokenChain(t, ctx, dir, seqs, brokenSeq)

	chunks := opsutil.SplitRange(from, to, 1)
	_, err := runVerifyChunks(
		ctx, filesystemLedgerstreamConfig(dir), chunks,
		false, "", // doCheckpoint=false: this is NOT the "chain" tier either — a
		// bare walk should still catch the break, matching Tier B's
		// doCheckpoint=true/doChain=false shape.
		time.Now(), 1*time.Hour,
		chunkOrchestratorOpts{},
	)
	if err == nil {
		t.Fatalf("runVerifyChunks: got nil error, want a chain-break error at ledger %d", brokenSeq)
	}
}

// ─── fixture helpers ────────────────────────────────────────────────

// filesystemLedgerstreamConfig duplicates the helper in test/integration/projector_test.go so this integration test
// need not import across package boundaries.
func filesystemLedgerstreamConfig(dir string) ledgerstream.Config {
	return ledgerstream.Config{
		DataStore: datastore.DataStoreConfig{
			Type:              "Filesystem",
			Params:            map[string]string{"destination_path": dir},
			Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
			NetworkPassphrase: testPassphrase,
			Compression:       "zstd",
		},
	}
}

// seedEmptyLedgers writes valid empty LedgerCloseMeta values (right LedgerSeq, zero hashes, so the chain
// check passes trivially) to the filesystem datastore at dir; duplicated from test/integration to stay self-contained.
func seedEmptyLedgers(t *testing.T, ctx context.Context, dir string, seqs []uint32) { //nolint:revive // ctx-second matches the test/integration helper signature
	t.Helper()
	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": dir},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: testPassphrase,
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish manifest: %v", err)
	}

	for _, seq := range seqs {
		lcm := xdr.LedgerCloseMeta{
			V: 1,
			V1: &xdr.LedgerCloseMetaV1{
				LedgerHeader: xdr.LedgerHeaderHistoryEntry{
					Header: xdr.LedgerHeader{LedgerSeq: xdr.Uint32(seq)},
				},
				TxSet: xdr.GeneralizedTransactionSet{
					V:       1,
					V1TxSet: &xdr.TransactionSetV1{},
				},
			},
		}
		batch := xdr.LedgerCloseMetaBatch{
			StartSequence:    xdr.Uint32(seq),
			EndSequence:      xdr.Uint32(seq),
			LedgerCloseMetas: []xdr.LedgerCloseMeta{lcm},
		}
		encoder := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch)
		var buf bytes.Buffer
		if _, err := encoder.WriteTo(&buf); err != nil {
			t.Fatalf("encode batch seq=%d: %v", seq, err)
		}
		key := cfg.Schema.GetObjectKeyFromSequenceNumber(seq)
		if err := store.PutFile(ctx, key, byteSliceWriterTo(buf.Bytes()), nil); err != nil {
			t.Fatalf("put seq=%d: %v", seq, err)
		}
	}
}

// seedLedgersWithBrokenChain is seedEmptyLedgers with one ledger's PreviousLedgerHash non-matching, proving the continuity check fires outside the "chain" tier.
func seedLedgersWithBrokenChain(t *testing.T, ctx context.Context, dir string, seqs []uint32, brokenSeq uint32) { //nolint:revive // ctx-second matches seedEmptyLedgers
	t.Helper()
	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": dir},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: testPassphrase,
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish manifest: %v", err)
	}

	for _, seq := range seqs {
		header := xdr.LedgerHeader{LedgerSeq: xdr.Uint32(seq)}
		if seq == brokenSeq {
			header.PreviousLedgerHash = xdr.Hash{0xde, 0xad, 0xbe, 0xef}
		}
		lcm := xdr.LedgerCloseMeta{
			V: 1,
			V1: &xdr.LedgerCloseMetaV1{
				LedgerHeader: xdr.LedgerHeaderHistoryEntry{Header: header},
				TxSet: xdr.GeneralizedTransactionSet{
					V:       1,
					V1TxSet: &xdr.TransactionSetV1{},
				},
			},
		}
		batch := xdr.LedgerCloseMetaBatch{
			StartSequence:    xdr.Uint32(seq),
			EndSequence:      xdr.Uint32(seq),
			LedgerCloseMetas: []xdr.LedgerCloseMeta{lcm},
		}
		encoder := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch)
		var buf bytes.Buffer
		if _, err := encoder.WriteTo(&buf); err != nil {
			t.Fatalf("encode batch seq=%d: %v", seq, err)
		}
		key := cfg.Schema.GetObjectKeyFromSequenceNumber(seq)
		if err := store.PutFile(ctx, key, byteSliceWriterTo(buf.Bytes()), nil); err != nil {
			t.Fatalf("put seq=%d: %v", seq, err)
		}
	}
}

type byteSliceWriterTo []byte

func (b byteSliceWriterTo) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}
