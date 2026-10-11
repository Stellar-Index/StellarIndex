package ledgerstream_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	sdklog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestStream_boundedRange_filesystemDatastore proves the SDK's BufferedStorageBackend happy path
// flows through our wrapper: three ledger files in Galexie layout, Stream(from=5, to=7) delivers
// all three in order. Uses the filesystem datastore, so no Docker.
func TestStream_boundedRange_filesystemDatastore(t *testing.T) {
	tmp := t.TempDir()

	// The Galexie layout this test simulates: 1 ledger per file, no
	// partition directories (FilesPerPartition=1). Object keys look
	// like `FFFFFFFA--5.xdr.zstd` for ledger 5, etc.
	const ledgersPerFile = 1
	const filesPerPartition = 1

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type: "Filesystem",
		Params: map[string]string{
			"destination_path": tmp,
		},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    ledgersPerFile,
			FilesPerPartition: filesPerPartition,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	// Publish the datastore manifest so LoadSchema (called inside
	// ApplyLedgerMetadata) can find it.
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}

	// Seed three ledger files.
	const fromLedger = 5
	const toLedger = 7
	for seq := uint32(fromLedger); seq <= toLedger; seq++ {
		writeLedgerFixture(t, ctx, store, cfg.Schema, seq)
	}

	// Verify ListFilePaths sees them — canary for the layout.
	files, err := store.ListFilePaths(ctx, datastore.ListFileOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) < 3 {
		t.Fatalf("expected ≥3 files, got %d: %v", len(files), files)
	}

	// Drive our wrapper; collect the LCMs the callback sees.
	got := make([]xdr.LedgerCloseMeta, 0, 3)
	err = ledgerstream.Stream(ctx,
		ledgerstream.Config{DataStore: cfg},
		fromLedger, toLedger,
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("callback invoked %d times, want 3", len(got))
	}
	for i, lcm := range got {
		wantSeq := uint32(fromLedger + i)
		gotSeq := uint32(lcm.LedgerSequence())
		if gotSeq != wantSeq {
			t.Errorf("ledger[%d]: got seq %d, want %d", i, gotSeq, wantSeq)
		}
	}
}

// TestStream_singleLedgerBoundedRange pins the ch-live-catchup tip-extend case: Stream(from=N,
// to=N) must walk exactly one ledger. The SDK rejects single-ledger bounded ranges, so Stream
// routes them through the in-house hot-only walk.
func TestStream_singleLedgerBoundedRange(t *testing.T) {
	tmp := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type: "Filesystem",
		Params: map[string]string{
			"destination_path": tmp,
		},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}

	const seq = uint32(9)
	writeLedgerFixture(t, ctx, store, cfg.Schema, seq)

	got := make([]xdr.LedgerCloseMeta, 0, 1)
	err = ledgerstream.Stream(ctx,
		ledgerstream.Config{DataStore: cfg},
		seq, seq,
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Stream(from=to=%d): %v", seq, err)
	}
	if len(got) != 1 {
		t.Fatalf("callback invoked %d times, want 1", len(got))
	}
	if gotSeq := uint32(got[0].LedgerSequence()); gotSeq != seq {
		t.Errorf("got seq %d, want %d", gotSeq, seq)
	}
}

// TestStream_singleLedgerBoundedRange_BelowGenesis_ReturnsError pins that Stream(from=1, to=1),
// entirely below genesis (ledger 2), returns an error. Clamped-from (2) > To (1) would otherwise
// make the walk deliver ZERO ledgers and return nil, a silent no-op.
func TestStream_singleLedgerBoundedRange_BelowGenesis_ReturnsError(t *testing.T) {
	tmp := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type: "Filesystem",
		Params: map[string]string{
			"destination_path": tmp,
		},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}
	// Deliberately do NOT write a ledger-1 fixture: a correct fix must
	// reject this range before ever attempting to read a ledger file.

	callCount := 0
	err = ledgerstream.Stream(ctx,
		ledgerstream.Config{DataStore: cfg},
		1, 1,
		func(xdr.LedgerCloseMeta) error {
			callCount++
			return nil
		},
	)
	if err == nil {
		t.Fatalf("Stream(from=to=1) returned nil error with %d callback invocations — want an explicit error (every requested ledger is below genesis ledger 2), not a silent zero-ledger success", callCount)
	}
	if callCount != 0 {
		t.Errorf("callback invoked %d times, want 0", callCount)
	}
}

func TestStream_rejectsNilCallback(t *testing.T) {
	err := ledgerstream.Stream(
		context.Background(),
		ledgerstream.Config{
			DataStore: datastore.DataStoreConfig{Type: "Filesystem"},
		},
		1, 2,
		nil,
	)
	if err == nil {
		t.Fatal("expected error on nil callback")
	}
}

func TestStream_rejectsEmptyDataStoreType(t *testing.T) {
	err := ledgerstream.Stream(
		context.Background(),
		ledgerstream.Config{},
		1, 2,
		func(xdr.LedgerCloseMeta) error { return nil },
	)
	if err == nil {
		t.Fatal("expected error on empty DataStore.Type")
	}
}

func TestStream_callbackErrorAborts(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": tmp},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish: %v", err)
	}
	writeLedgerFixture(t, ctx, store, cfg.Schema, 5)
	writeLedgerFixture(t, ctx, store, cfg.Schema, 6)

	sentinel := errors.New("callback-said-stop")
	callCount := 0
	err = ledgerstream.Stream(ctx,
		ledgerstream.Config{DataStore: cfg},
		5, 6,
		func(xdr.LedgerCloseMeta) error {
			callCount++
			return sentinel
		},
	)
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain lost sentinel: %v", err)
	}
	// Stream stops at first callback error — second ledger never
	// invokes the callback.
	if callCount != 1 {
		t.Errorf("callback called %d times, want 1 (stop on first error)", callCount)
	}
}

// ─── fixture helpers ────────────────────────────────────────────

// writeLedgerFixture writes a minimal zstd-compressed LedgerCloseMetaBatch for seq at its Galexie
// object key, built in-test so no fixture binaries ship in the repo.
func writeLedgerFixture(t *testing.T, ctx context.Context, store datastore.DataStore, schema datastore.DataStoreSchema, seq uint32) {
	t.Helper()
	lcm := minimalLedgerCloseMeta(seq)
	batch := xdr.LedgerCloseMetaBatch{
		StartSequence:    xdr.Uint32(seq),
		EndSequence:      xdr.Uint32(seq),
		LedgerCloseMetas: []xdr.LedgerCloseMeta{lcm},
	}
	buf := encodeBatch(t, batch)
	key := schema.GetObjectKeyFromSequenceNumber(seq)
	if err := store.PutFile(ctx, key, bufferWriterTo(buf), nil); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	// Sanity: confirm the file is on disk where we expect.
	base, _ := os.Getwd()
	_ = base
	if exists, _ := store.Exists(ctx, key); !exists {
		t.Fatalf("file not found after put: %s", key)
	}
	_ = filepath.Join // silence unused on older paths
}

// minimalLedgerCloseMeta builds a V1 LCM with just the sequence set, plus an empty
// GeneralizedTransactionSet arm so the XDR encoder accepts the union. Stream inspects nothing
// beyond what the SDK needs to round-trip the batch.
func minimalLedgerCloseMeta(seq uint32) xdr.LedgerCloseMeta {
	return xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V:       1,
				V1TxSet: &xdr.TransactionSetV1{},
			},
		},
	}
}

// encodeBatch marshals a LedgerCloseMetaBatch to XDR and zstd-
// compresses — the on-wire format Galexie emits.
func encodeBatch(t *testing.T, batch xdr.LedgerCloseMetaBatch) []byte {
	t.Helper()
	encoder := compressxdr.NewXDREncoder(compressxdr.DefaultCompressor, batch)
	var buf bytes.Buffer
	if _, err := encoder.WriteTo(&buf); err != nil {
		t.Fatalf("encode batch: %v", err)
	}
	return buf.Bytes()
}

// bufferWriterTo adapts a []byte to io.WriterTo — the interface
// PutFile expects.
type bufferWriterTo []byte

func (b bufferWriterTo) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}

// TestStream_ColdTierInitFailure_FallsBackToHotOnly proves a cold tier that fails to construct
// must NOT abort the walk: per ADR-0027 it is an optional fallback, so a broken cold config cannot
// fail a good hot read. streamTiered logs a WARN and degrades to hot-only.
//
// This covers the multi-ledger branch (From != To), which closes the open hot store and re-enters
// via ApplyLedgerMetadata; the single-ledger branch is TestStream_ColdTierInitFailure_SingleLedgerRange.
func TestStream_ColdTierInitFailure_FallsBackToHotOnly(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hotCfg := datastore.DataStoreConfig{
		Type: "Filesystem",
		Params: map[string]string{
			"destination_path": tmp,
		},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, hotCfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}
	writeLedgerFixture(t, ctx, store, hotCfg.Schema, 5)
	writeLedgerFixture(t, ctx, store, hotCfg.Schema, 6)

	logger := sdklog.New()
	stopTest := logger.StartTest(sdklog.WarnLevel)

	lsCfg := ledgerstream.Config{
		DataStore: hotCfg,
		// An unsupported Type makes datastore.NewDataStore fail
		// immediately with no network access — the cheapest faithful
		// simulation of "cold endpoint misconfigured" (wrong
		// region/endpoint for aws-public-blockchain).
		ColdDataStore: datastore.DataStoreConfig{
			Type: "Bogus-Unsupported-Type",
		},
		Logger: logger,
	}

	got := 0
	err = ledgerstream.Stream(ctx, lsCfg, 5, 6, func(_ xdr.LedgerCloseMeta) error {
		got++
		return nil
	})
	entries := stopTest()

	if err != nil {
		t.Fatalf("Stream returned err=%v; want nil — cold-init failure must fall back to hot-only, not abort", err)
	}
	if got != 2 {
		t.Fatalf("callback invoked %d times, want 2 (hot-only fallback should still deliver every hot ledger)", got)
	}

	foundWarn := false
	for _, e := range entries {
		if e.Level == sdklog.WarnLevel && strings.Contains(e.Message, "cold datastore init failed") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected a WARN log containing %q; got entries: %+v", "cold datastore init failed", entries)
	}
}

// TestStream_ColdSchemaMismatch_ReturnsError guards the schema check: when hot and cold export
// different shapes (here LedgersPerFile), hot-shaped keys silently 404 on cold forever. Stream
// must refuse loudly rather than wrap a TieredDataStore whose cold side can never be reached.
func TestStream_ColdSchemaMismatch_ReturnsError(t *testing.T) {
	hotDir := t.TempDir()
	coldDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hotStore, err := datastore.NewFilesystemDataStoreWithPath(hotDir)
	if err != nil {
		t.Fatalf("open hot filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = hotStore.Close() })

	coldStore, err := datastore.NewFilesystemDataStoreWithPath(coldDir)
	if err != nil {
		t.Fatalf("open cold filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = coldStore.Close() })

	hotCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": hotDir},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	// Cold uses a DIFFERENT LedgersPerFile — a real-world shape a
	// separately-configured archive export could plausibly have.
	coldCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": coldDir},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    64,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, hotStore, hotCfg); err != nil {
		t.Fatalf("publish hot config: %v", err)
	}
	if _, _, err := datastore.PublishConfig(ctx, coldStore, coldCfg); err != nil {
		t.Fatalf("publish cold config: %v", err)
	}
	writeLedgerFixture(t, ctx, hotStore, hotCfg.Schema, 5)

	lsCfg := ledgerstream.Config{
		DataStore:     hotCfg,
		ColdDataStore: coldCfg,
	}
	got := 0
	err = ledgerstream.Stream(ctx, lsCfg, 5, 6, func(_ xdr.LedgerCloseMeta) error {
		got++
		return nil
	})
	if err == nil {
		t.Fatalf("Stream returned nil with a hot/cold LedgersPerFile mismatch (1 vs 64) and %d callback invocations — want an explicit error, not a silently-defeated cold fallback", got)
	}
	if !strings.Contains(err.Error(), "differs from hot") {
		t.Errorf("err = %v; want it to name the schema mismatch", err)
	}
}

// TestStream_ColdTierInitFailure_SingleLedgerRange covers the other half of the cold-init-failure
// branch: a single-ledger range reuses the open hot store via the in-house walk, since
// ApplyLedgerMetadata rejects it. Both branches must degrade to hot-only.
func TestStream_ColdTierInitFailure_SingleLedgerRange(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hotCfg := datastore.DataStoreConfig{
		Type: "Filesystem",
		Params: map[string]string{
			"destination_path": tmp,
		},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, hotCfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}
	const seq = uint32(9)
	writeLedgerFixture(t, ctx, store, hotCfg.Schema, seq)

	logger := sdklog.New()
	stopTest := logger.StartTest(sdklog.WarnLevel)

	lsCfg := ledgerstream.Config{
		DataStore: hotCfg,
		ColdDataStore: datastore.DataStoreConfig{
			Type: "Bogus-Unsupported-Type",
		},
		Logger: logger,
	}

	got := 0
	err = ledgerstream.Stream(ctx, lsCfg, seq, seq, func(_ xdr.LedgerCloseMeta) error {
		got++
		return nil
	})
	entries := stopTest()

	if err != nil {
		t.Fatalf("Stream(from=to=%d) returned err=%v; want nil (hot-only fallback)", seq, err)
	}
	if got != 1 {
		t.Fatalf("callback invoked %d times, want 1", got)
	}

	foundWarn := false
	for _, e := range entries {
		if e.Level == sdklog.WarnLevel && strings.Contains(e.Message, "cold datastore init failed") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected a WARN log containing %q; got entries: %+v", "cold datastore init failed", entries)
	}
}

// TestStream_ColdDataStoreFactory_TakesPrecedence proves that when Config.ColdDataStoreFactory is
// set, streamTiered opens the cold tier through it and NOT through datastore.NewDataStore, whose
// ambient AWS credential chain carries the hot tier's MinIO keys (see the factory's godoc).
//
// The paths are distinguishable by outcome: ColdDataStore.Type is unsupported, so
// datastore.NewDataStore would fail and degrade to hot-only, which cannot serve ledger 6 (hot
// holds only 5). Delivering both ledgers is only possible if the factory opened the cold store.
func TestStream_ColdDataStoreFactory_TakesPrecedence(t *testing.T) {
	hotDir := t.TempDir()
	coldDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hotStore, err := datastore.NewFilesystemDataStoreWithPath(hotDir)
	if err != nil {
		t.Fatalf("open hot filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = hotStore.Close() })
	coldStore, err := datastore.NewFilesystemDataStoreWithPath(coldDir)
	if err != nil {
		t.Fatalf("open cold filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = coldStore.Close() })

	schema := datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1}
	hotCfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": hotDir},
		Schema:            schema,
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	// Same schema/passphrase/compression as hot (LoadSchema compares
	// manifests), but a Type datastore.NewDataStore cannot construct —
	// so the only way cold opens is via the factory.
	coldCfg := hotCfg
	coldCfg.Type = "Bogus-Unsupported-Type"
	coldCfg.Params = map[string]string{"destination_path": coldDir}

	if _, _, err := datastore.PublishConfig(ctx, hotStore, hotCfg); err != nil {
		t.Fatalf("publish hot config: %v", err)
	}
	fsColdCfg := coldCfg
	fsColdCfg.Type = "Filesystem"
	if _, _, err := datastore.PublishConfig(ctx, coldStore, fsColdCfg); err != nil {
		t.Fatalf("publish cold config: %v", err)
	}
	writeLedgerFixture(t, ctx, hotStore, schema, 5)
	writeLedgerFixture(t, ctx, coldStore, schema, 6)

	factoryCalls := 0
	lsCfg := ledgerstream.Config{
		DataStore:     hotCfg,
		ColdDataStore: coldCfg,
		ColdDataStoreFactory: func(context.Context) (datastore.DataStore, error) {
			factoryCalls++
			return datastore.NewFilesystemDataStoreWithPath(coldDir)
		},
	}

	var seen []uint32
	err = ledgerstream.Stream(ctx, lsCfg, 5, 6, func(lcm xdr.LedgerCloseMeta) error {
		seen = append(seen, lcm.LedgerSequence())
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if factoryCalls != 1 {
		t.Errorf("ColdDataStoreFactory called %d times, want 1 — streamTiered must prefer it over datastore.NewDataStore", factoryCalls)
	}
	if len(seen) != 2 || seen[0] != 5 || seen[1] != 6 {
		t.Fatalf("delivered ledgers %v, want [5 6] — ledger 6 exists only in the cold tier, so anything else means "+
			"the cold store was opened through datastore.NewDataStore (which fails on this Type) instead of the factory", seen)
	}
}

// TestStream_ColdDataStoreFactoryError_FallsBackToHotOnly keeps the "cold tier is optional"
// invariant across the factory hook: a failing factory must degrade to hot-only exactly like a
// datastore.NewDataStore failure, not abort the walk.
func TestStream_ColdDataStoreFactoryError_FallsBackToHotOnly(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hotCfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": tmp},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, hotCfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}
	writeLedgerFixture(t, ctx, store, hotCfg.Schema, 5)
	writeLedgerFixture(t, ctx, store, hotCfg.Schema, 6)

	logger := sdklog.New()
	stopTest := logger.StartTest(sdklog.WarnLevel)

	coldCfg := hotCfg
	coldCfg.Type = "S3"
	lsCfg := ledgerstream.Config{
		DataStore:     hotCfg,
		ColdDataStore: coldCfg,
		ColdDataStoreFactory: func(context.Context) (datastore.DataStore, error) {
			return nil, errors.New("cold datastore: s3_cold_access_key_env names STELLARINDEX_S3_COLD_ACCESS_KEY but it is unset")
		},
		Logger: logger,
	}

	got := 0
	err = ledgerstream.Stream(ctx, lsCfg, 5, 6, func(_ xdr.LedgerCloseMeta) error {
		got++
		return nil
	})
	entries := stopTest()

	if err != nil {
		t.Fatalf("Stream returned err=%v; want nil — a cold-factory failure must degrade to hot-only", err)
	}
	if got != 2 {
		t.Fatalf("callback invoked %d times, want 2", got)
	}
	foundWarn := false
	for _, e := range entries {
		if e.Level == sdklog.WarnLevel && strings.Contains(e.Message, "cold datastore init failed") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected a WARN log containing %q; got entries: %+v", "cold datastore init failed", entries)
	}
}

// An inverted bounded range is rejected before any datastore is opened —
// on the tiered path the cold tier must not be opened (a live bucket
// round-trip in production) before walkDataStore reaches validateRange.
func TestStream_InvertedRangeRejectedBeforeAnyDatastoreOpens(t *testing.T) {
	hotCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": t.TempDir()},
		Schema: datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
	}
	coldCfg := hotCfg
	coldOpens := 0
	cfg := ledgerstream.Config{
		DataStore:     hotCfg,
		ColdDataStore: coldCfg,
		ColdDataStoreFactory: func(ctx context.Context) (datastore.DataStore, error) {
			coldOpens++
			return datastore.NewDataStore(ctx, coldCfg)
		},
	}

	err := ledgerstream.Stream(context.Background(), cfg, 10, 5, func(xdr.LedgerCloseMeta) error { return nil })

	if err == nil || !strings.Contains(err.Error(), "must not be less than start") {
		t.Fatalf("Stream(10, 5) = %v, want the validateRange rejection", err)
	}
	if coldOpens != 0 {
		t.Errorf("cold tier opened %d time(s) for an inverted range, want 0", coldOpens)
	}
}

func TestStream_buffered_overrideUsedVerbatim(t *testing.T) {
	tmp := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": tmp},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish: %v", err)
	}
	writeLedgerFixture(t, ctx, store, cfg.Schema, 5)
	writeLedgerFixture(t, ctx, store, cfg.Schema, 6)

	// Hand-tuned override — small buffer + workers so the test
	// exercises the same throughput path as the default but with
	// values the SDK didn't compute.
	override := &ledgerbackend.BufferedStorageBackendConfig{
		BufferSize: 2,
		NumWorkers: 1,
		RetryLimit: 1,
		RetryWait:  10 * time.Millisecond,
	}

	calls := 0
	err = ledgerstream.Stream(ctx,
		ledgerstream.Config{DataStore: cfg, Buffered: override},
		5, 6,
		func(xdr.LedgerCloseMeta) error {
			calls++
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Stream with override: %v", err)
	}
	if calls != 2 {
		t.Errorf("callback invoked %d times, want 2", calls)
	}
}

// TestStream_liveRetryWait_picksUpLateLedger pins Config.LiveRetryWait: a live-tail fetch worker
// that misses the next object must re-check within LiveRetryWait, not the SDK's 30s default. It
// writes ledger 7 ~400ms after the stream caught up to 6 (so the worker is already in its
// retry-wait) and asserts delivery well inside 8s, which the 30s default could not meet.
func TestStream_liveRetryWait_picksUpLateLedger(t *testing.T) {
	tmp := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": tmp},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish: %v", err)
	}
	writeLedgerFixture(t, ctx, store, cfg.Schema, 5)
	writeLedgerFixture(t, ctx, store, cfg.Schema, 6)

	// Pre-encode ledger 7 on the test goroutine — the fixture helpers
	// call t.Fatalf, which is illegal off the test goroutine.
	buf7 := encodeBatch(t, xdr.LedgerCloseMetaBatch{
		StartSequence:    xdr.Uint32(7),
		EndSequence:      xdr.Uint32(7),
		LedgerCloseMetas: []xdr.LedgerCloseMeta{minimalLedgerCloseMeta(7)},
	})
	key7 := cfg.Schema.GetObjectKeyFromSequenceNumber(7)

	// Publish ledger 7 only AFTER the stream has caught up. With a
	// short LiveRetryWait the worker waiting on 7 must surface it
	// promptly; with the 30s default it would miss the 8s window.
	putErr := make(chan error, 1)
	go func() {
		time.Sleep(400 * time.Millisecond)
		putErr <- store.PutFile(ctx, key7, bufferWriterTo(buf7), nil)
	}()

	stop := errors.New("stop")
	got := make([]uint32, 0, 3)
	err = ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore:     cfg,
			LiveRetryWait: 50 * time.Millisecond,
		},
		5, 0, /*unbounded — live tail*/
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm.LedgerSequence())
			if lcm.LedgerSequence() == 7 {
				return stop
			}
			return nil
		},
	)
	if !errors.Is(err, stop) {
		t.Fatalf("stream did not deliver ledger 7 within 8s — "+
			"LiveRetryWait override not honored? err=%v got=%v", err, got)
	}
	if perr := <-putErr; perr != nil {
		t.Fatalf("put ledger 7: %v", perr)
	}
	if len(got) != 3 || got[0] != 5 || got[1] != 6 || got[2] != 7 {
		t.Errorf("got %v, want [5 6 7]", got)
	}
}

// Config.LiveRetryBudget must cover the datastore STARTUP, not only the SDK fetch worker's
// in-walk retries.
//
// The budget is spent inside the SDK's ledger buffer, which exists only after NewDataStore and
// LoadSchema (a live LIST) have succeeded once with no retry. A lake unreachable when the tail
// STARTS would make Stream fail in microseconds, and restarts dying in ~1s exhaust the unit's
// StartLimit and park it in `failed` even after the lake recovers.
//
// A directory that does not exist yet is the hermetic stand-in for a down MinIO: LoadSchema fails
// as against a refused socket, and creating it mid-test is the lake coming back.
func TestStream_liveTail_retriesWhenDataStoreIsDownAtStart(t *testing.T) {
	root := t.TempDir()
	// The datastore path deliberately does not exist yet.
	lakePath := filepath.Join(root, "galexie-live")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": lakePath},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	// Pre-encode the ledgers on the test goroutine — the fixture helpers
	// call t.Fatalf, which is illegal off it.
	type pending struct {
		key string
		buf []byte
	}
	var seed []pending
	for seq := uint32(5); seq <= 6; seq++ {
		seed = append(seed, pending{
			key: dsCfg.Schema.GetObjectKeyFromSequenceNumber(seq),
			buf: encodeBatch(t, xdr.LedgerCloseMetaBatch{
				StartSequence:    xdr.Uint32(seq),
				EndSequence:      xdr.Uint32(seq),
				LedgerCloseMetas: []xdr.LedgerCloseMeta{minimalLedgerCloseMeta(seq)},
			}),
		})
	}

	// Bring the "lake" up ~600ms in — i.e. after Stream has already
	// failed its first start and is inside the retry loop. Well short of
	// the 6s budget, and well beyond the ~85µs the un-fixed code took to
	// give up.
	bringUp := make(chan error, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		bringUp <- func() error {
			store, err := datastore.NewFilesystemDataStoreWithPath(lakePath)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			if _, _, err := datastore.PublishConfig(ctx, store, dsCfg); err != nil {
				return err
			}
			for _, p := range seed {
				if err := store.PutFile(ctx, p.key, bufferWriterTo(p.buf), nil); err != nil {
					return err
				}
			}
			return nil
		}()
	}()

	before := testutil.ToFloat64(obs.LedgerstreamLiveStartRetriesTotal)

	stop := errors.New("stop")
	got := make([]uint32, 0, 2)
	start := time.Now()
	err := ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore:       dsCfg,
			LiveRetryWait:   50 * time.Millisecond,
			LiveRetryBudget: 6 * time.Second,
		},
		5, 0, /* unbounded — live tail */
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm.LedgerSequence())
			if lcm.LedgerSequence() == 6 {
				return stop
			}
			return nil
		},
	)
	elapsed := time.Since(start)

	if berr := <-bringUp; berr != nil {
		t.Fatalf("bring the lake up: %v", berr)
	}

	// The assertion that matters: the stream RODE OUT the outage in
	// process and went on to deliver the real ledgers. `stop` is the
	// caller's own sentinel from the callback, so reaching it proves the
	// walk actually ran — not merely that Stream returned late.
	if !errors.Is(err, stop) {
		t.Fatalf("live tail did not survive a datastore that was down at start: "+
			"err=%v after %v, delivered=%v (want ledgers [5 6] then the caller's stop)",
			err, elapsed, got)
	}
	if len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Errorf("delivered %v, want [5 6]", got)
	}
	// Pin the no-skip invariant explicitly: the retry re-issued the SAME
	// range, so the caller sees ledger 5 first. A resume that jumped
	// ahead would show up here as a missing 5.
	if len(got) > 0 && got[0] != 5 {
		t.Errorf("retry skipped ledger(s): first delivered ledger is %d, want 5", got[0])
	}

	// And the stall must not be silent while it happens.
	after := testutil.ToFloat64(obs.LedgerstreamLiveStartRetriesTotal)
	if after <= before {
		t.Errorf("stellarindex_ledgerstream_live_start_retries_total did not move: %v -> %v", before, after)
	}
}

// The budget is a ceiling: a lake that never comes back must surface the error within roughly one
// budget, not hang until the caller's context dies; a silent freeze is worse than a crash.
func TestStream_liveTail_startRetryIsBounded(t *testing.T) {
	root := t.TempDir()
	lakePath := filepath.Join(root, "never-arrives")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": lakePath},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	const budget = 700 * time.Millisecond
	start := time.Now()
	err := ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore:       dsCfg,
			LiveRetryWait:   50 * time.Millisecond,
			LiveRetryBudget: budget,
		},
		5, 0, /* unbounded — live tail */
		func(xdr.LedgerCloseMeta) error {
			t.Error("callback must not run: nothing was ever readable")
			return nil
		},
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Stream returned nil for a lake that never became readable")
	}
	// Loud, not silent: the operator-facing error still names the real
	// cause rather than a generic timeout.
	if got := err.Error(); !containsAll(got, "datastore", "schema") {
		t.Errorf("error lost the cause: %q", got)
	}
	if elapsed < budget {
		t.Errorf("gave up after %v, before the %v budget was spent", elapsed, budget)
	}
	// Generous ceiling: the deadline is fixed before the first
	// re-attempt, so total time is ~budget plus one final attempt.
	if elapsed > 10*budget {
		t.Errorf("retry was not bounded by the budget: ran %v for a %v budget", elapsed, budget)
	}
}

// A BOUNDED stream must be untouched: a missing object there is a hard error owned by the caller
// (see maybeTolerateTrailingMissing). Retrying would silently stretch every ops backfill's failure
// path, and the archive walk in StreamArchiveThenLive depends on failing fast.
func TestStream_boundedRange_startFailureIsNotRetried(t *testing.T) {
	root := t.TempDir()
	lakePath := filepath.Join(root, "never-arrives")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": lakePath},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	start := time.Now()
	err := ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore:       dsCfg,
			LiveRetryWait:   50 * time.Millisecond,
			LiveRetryBudget: 10 * time.Second,
		},
		5, 7, /* BOUNDED */
		func(xdr.LedgerCloseMeta) error { return nil },
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("bounded Stream returned nil against an unreadable datastore")
	}
	if elapsed > 2*time.Second {
		t.Errorf("bounded range consumed the live-tail retry budget: returned after %v", elapsed)
	}
}

// A SIGTERM landing in the backoff window must still read as a clean stop: the indexer checks
// errors.Is(err, context.Canceled), and anything else exits non-zero and marks the unit failed on
// a deliberate stop. The cause must survive alongside it so the journal says why the tail had not
// started.
func TestStream_liveTail_startRetryShutdownIsNotFatal(t *testing.T) {
	root := t.TempDir()
	lakePath := filepath.Join(root, "never-arrives")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dsCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": lakePath},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}

	// Cancel while the loop is asleep between re-attempts.
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	err := ledgerstream.Stream(ctx,
		ledgerstream.Config{
			DataStore: dsCfg,
			// Long wait so the cancellation reliably lands in the sleep.
			LiveRetryWait:   3 * time.Second,
			LiveRetryBudget: 30 * time.Second,
		},
		5, 0, /* unbounded — live tail */
		func(xdr.LedgerCloseMeta) error { return nil },
	)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown during the retry backoff is not reported as a clean stop: %v", err)
	}
	if got := err.Error(); !containsAll(got, "datastore", "schema") {
		t.Errorf("cancellation swallowed the cause: %q", got)
	}
}

// Every Stream walk is counted under the path it took, and a configured cold
// tier that fails to open is counted as cold_degraded rather than vanishing
// into a conditional WARN.
func TestStream_CountsReadPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := hotOnlyConfig(t, ctx)
	noop := func(xdr.LedgerCloseMeta) error { return nil }

	cases := []struct {
		name, path string
		cfg        func() ledgerstream.Config
		from, to   uint32
	}{
		{"multi-ledger hot-only", "sdk", func() ledgerstream.Config { return cfg }, 5, 6},
		{"single ledger hot-only", "hot_single_ledger", func() ledgerstream.Config { return cfg }, 6, 6},
		{"cold tier fails to open", "cold_degraded", func() ledgerstream.Config {
			c := cfg
			c.ColdDataStore = cfg.DataStore
			c.ColdDataStore.Type = "S3"
			c.ColdDataStoreFactory = func(context.Context) (datastore.DataStore, error) {
				return nil, errors.New("cold datastore: credentials unset")
			}
			return c
		}, 5, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := pathCount(tc.path)
			if err := ledgerstream.Stream(ctx, tc.cfg(), tc.from, tc.to, noop); err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if d := pathCount(tc.path) - before; d != 1 {
				t.Errorf("stream_path_total{path=%q} delta = %v, want 1", tc.path, d)
			}
		})
	}
}

// TestStream_TolerateTrailingMissing_HappyPath asserts that a
// bounded Stream whose range overshoots the materialised content
// returns nil (walk-complete) when TolerateTrailingMissing is set
// and the missing sequence falls within the trailing window.
func TestStream_TolerateTrailingMissing_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, store := tolerateTrailingEdgeStore(t, tmp)
	t.Cleanup(func() { _ = store.Close() })

	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}

	// Materialise ledgers 5,6,7 but ask for [5, 9]. Ledgers 8 + 9
	// don't exist — the SDK will surface the missing-file error for
	// 8 first. With TolerateTrailingMissing=true and a window that
	// covers the gap (9 - 8 = 1 ≤ 16), Stream should return nil.
	for seq := uint32(5); seq <= 7; seq++ {
		writeLedgerFixture(t, ctx, store, cfg.Schema, seq)
	}

	got := 0
	lsCfg := ledgerstream.Config{
		DataStore:               cfg,
		TolerateTrailingMissing: true,
		TrailingMissingWindow:   16,
	}
	err := ledgerstream.Stream(ctx, lsCfg, 5, 9, func(_ xdr.LedgerCloseMeta) error {
		got++
		return nil
	})
	if err != nil {
		t.Fatalf("Stream returned err=%v; want nil (trailing-edge tolerated)", err)
	}
	// `got` is intentionally NOT asserted: on a trailing-edge miss the SDK's
	// BufferedStorageBackend cancels its context and drops undelivered pre-fetched ledgers, even
	// ones present on disk, and with parallel workers the race can drop 0 or all of them. Full
	// coverage requires clamping -to below the live tip; the tolerate flag only gives a graceful
	// exit on trailing-edge races.
	_ = got
}

// TestStream_TolerateTrailingMissing_MidRangeStillErrors asserts a missing file FAR from To still
// errors under TolerateTrailingMissing, so real corruption is not masked: only the trailing window
// is a tip race.
func TestStream_TolerateTrailingMissing_MidRangeStillErrors(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, store := tolerateTrailingEdgeStore(t, tmp)
	t.Cleanup(func() { _ = store.Close() })

	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}

	// Materialise 5,6,7. Ask for [5, 200] with window 16. Missing
	// seq 8 has gap 200-8=192 > 16 → mid-range, must error.
	for seq := uint32(5); seq <= 7; seq++ {
		writeLedgerFixture(t, ctx, store, cfg.Schema, seq)
	}

	lsCfg := ledgerstream.Config{
		DataStore:               cfg,
		TolerateTrailingMissing: true,
		TrailingMissingWindow:   16,
	}
	err := ledgerstream.Stream(ctx, lsCfg, 5, 200, func(_ xdr.LedgerCloseMeta) error {
		return nil
	})
	if err == nil {
		t.Fatalf("Stream returned nil; expected mid-range gap error")
	}
	if !strings.Contains(err.Error(), "is missing") {
		t.Errorf("err = %v; expected to contain SDK 'is missing' message", err)
	}
}

// A chunked walker passes a chunk boundary as To, so a hole just below it
// sits inside the To-relative window; later ledgers on disk prove it is not the tip.
func TestStream_TolerateTrailingMissing_ChunkedWalkRefusesHoleBelowTip(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, store := tolerateTrailingEdgeStore(t, tmp)
	t.Cleanup(func() { _ = store.Close() })
	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}
	// Ledger 8 is a hole; the store holds 5..7 and 9..40.
	for seq := uint32(5); seq <= 40; seq++ {
		if seq != 8 {
			writeLedgerFixture(t, ctx, store, cfg.Schema, seq)
		}
	}

	lsCfg := ledgerstream.Config{
		DataStore:               cfg,
		TolerateTrailingMissing: true,
		TrailingMissingWindow:   16,
	}
	err := ledgerstream.Stream(ctx, lsCfg, 5, 10, func(_ xdr.LedgerCloseMeta) error { return nil })
	if err == nil {
		t.Fatal("Stream over chunk [5,10] with ledger 8 missing and the store at 40 returned nil — " +
			"a mid-archive hole was tolerated as a trailing edge")
	}
	for _, want := range []string{"is missing", "below the datastore tip 40"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want it to contain %q", err, want)
		}
	}

	// The same chunk at the real tip is still tolerated.
	if err := ledgerstream.Stream(ctx, lsCfg, 38, 45, func(_ xdr.LedgerCloseMeta) error { return nil }); err != nil {
		t.Errorf("Stream [38,45] past the store tip 40: err = %v, want nil (trailing edge)", err)
	}
}

// A hole in both tiers must surface as ErrBothTiersMissing through the SDK's wrap.
func TestStream_TieredHoleBelowTipIsErrBothTiersMissing(t *testing.T) {
	hotDir, coldDir := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hotCfg, hot := tolerateTrailingEdgeStore(t, hotDir)
	t.Cleanup(func() { _ = hot.Close() })
	coldCfg, cold := tolerateTrailingEdgeStore(t, coldDir)
	t.Cleanup(func() { _ = cold.Close() })
	for _, p := range []struct {
		store datastore.DataStore
		cfg   datastore.DataStoreConfig
	}{{hot, hotCfg}, {cold, coldCfg}} {
		if _, _, err := datastore.PublishConfig(ctx, p.store, p.cfg); err != nil {
			t.Fatalf("publish config: %v", err)
		}
	}
	for seq := uint32(5); seq <= 40; seq++ {
		if seq != 8 {
			writeLedgerFixture(t, ctx, hot, hotCfg.Schema, seq)
		}
	}
	// LoadSchema infers the file extension from an existing object, so cold needs one.
	writeLedgerFixture(t, ctx, cold, coldCfg.Schema, 7)

	lsCfg := ledgerstream.Config{
		DataStore:               hotCfg,
		ColdDataStore:           coldCfg,
		TolerateTrailingMissing: true,
		TrailingMissingWindow:   16,
	}
	err := ledgerstream.Stream(ctx, lsCfg, 5, 10, func(_ xdr.LedgerCloseMeta) error { return nil })
	if !errors.Is(err, ledgerstream.ErrBothTiersMissing) {
		t.Fatalf("Stream over a hole missing from both tiers: err = %v, want errors.Is ErrBothTiersMissing", err)
	}
}

// TestStream_TolerateTrailingMissing_DisabledStrictMode asserts
// the default (TolerateTrailingMissing=false) preserves strict
// behaviour: any missing file in a bounded range is an error.
func TestStream_TolerateTrailingMissing_DisabledStrictMode(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, store := tolerateTrailingEdgeStore(t, tmp)
	t.Cleanup(func() { _ = store.Close() })

	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}

	for seq := uint32(5); seq <= 7; seq++ {
		writeLedgerFixture(t, ctx, store, cfg.Schema, seq)
	}

	lsCfg := ledgerstream.Config{
		DataStore: cfg,
		// TolerateTrailingMissing default false
	}
	err := ledgerstream.Stream(ctx, lsCfg, 5, 9, func(_ xdr.LedgerCloseMeta) error {
		return nil
	})
	if err == nil {
		t.Fatalf("Stream returned nil; expected missing-file error in strict mode")
	}
}

// TestStream_TolerateTrailingMissing_SingleLedgerZeroDelivery_Errors pins that a single-ledger
// Stream (from == to) whose one ledger IS the missing one is not tolerated; otherwise it returns
// nil with ZERO callbacks, a silent success. Unlike a wider range (prefetch-cancel raciness, see
// the HappyPath test), delivered==0 here always means the ledger genuinely doesn't exist.
func TestStream_TolerateTrailingMissing_SingleLedgerZeroDelivery_Errors(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, store := tolerateTrailingEdgeStore(t, tmp)
	t.Cleanup(func() { _ = store.Close() })

	if _, _, err := datastore.PublishConfig(ctx, store, cfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}
	// Deliberately materialise NOTHING — ledger 9 doesn't exist.

	lsCfg := ledgerstream.Config{
		DataStore:               cfg,
		TolerateTrailingMissing: true,
		TrailingMissingWindow:   16,
	}
	callCount := 0
	err := ledgerstream.Stream(ctx, lsCfg, 9, 9, func(_ xdr.LedgerCloseMeta) error {
		callCount++
		return nil
	})
	if err == nil {
		t.Fatalf("Stream(from=to=9, TolerateTrailingMissing=true) returned nil with %d callback invocations — want an error; a single-ledger range that delivers nothing must never be silently tolerated", callCount)
	}
	if callCount != 0 {
		t.Errorf("callback invoked %d times, want 0", callCount)
	}
}

// hotOnlyConfig is a Filesystem hot tier holding ledgers 5 and 6.
func hotOnlyConfig(t *testing.T, ctx context.Context) ledgerstream.Config {
	t.Helper()
	tmp := t.TempDir()
	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hotCfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": tmp},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, hotCfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}
	writeLedgerFixture(t, ctx, store, hotCfg.Schema, 5)
	writeLedgerFixture(t, ctx, store, hotCfg.Schema, 6)
	return ledgerstream.Config{DataStore: hotCfg}
}

// tolerateTrailingEdgeStore creates a fresh filesystem datastore
// shaped like Galexie's default (1 ledger per file, 1 file per
// partition for test compactness). Returns the config + opened
// store; caller must Close.
func tolerateTrailingEdgeStore(t *testing.T, dir string) (datastore.DataStoreConfig, datastore.DataStore) {
	t.Helper()
	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	cfg := datastore.DataStoreConfig{
		Type: "Filesystem",
		Params: map[string]string{
			"destination_path": dir,
		},
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    1,
			FilesPerPartition: 1,
		},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	return cfg, store
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func pathCount(path string) float64 {
	return testutil.ToFloat64(obs.LedgerstreamStreamPathTotal.WithLabelValues(path))
}
