package ledgerstream_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"
	sdklog "github.com/stellar/go-stellar-sdk/support/log"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

// TestStream_ColdTierInitFailure_FallsBackToHotOnly proves BACKLOG
// #56a: a cold-tier that fails to construct (bad Type, wrong
// region, unreachable endpoint, etc.) must NOT abort the walk. Per
// ADR-0027 the cold tier is an optional fallback for ranges trimmed
// from the local mirror — a broken cold config cannot make a
// perfectly-good hot read fail. streamTiered's cold branch logs a
// WARN (operator-visible) and degrades to the hot-only path instead
// of propagating the cold-side error.
//
// This exercises the multi-ledger branch of the fallback (From !=
// To), which closes the already-opened hot store and re-enters via
// the SDK's ingest.ApplyLedgerMetadata over cfg.DataStore alone —
// distinct from the single-ledger branch covered by
// TestStream_ColdTierInitFailure_SingleLedgerRange below.
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
		// simulation of "cold endpoint misconfigured" (the 2026-05-20
		// r1 incident: wrong region/endpoint for aws-public-blockchain).
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

// TestStream_ColdSchemaMismatch_ReturnsError is the regression test
// for INT-01 (audit-2026-07-23): when hot and cold both construct
// successfully but their Galexie export shapes disagree (here,
// LedgersPerFile), object keys computed from hot's schema are simply
// wrong for cold's layout — cold fallback would silently 404 forever
// instead of ever actually serving a trimmed-from-hot range. Stream
// must refuse loudly rather than wrap a TieredDataStore whose cold
// side can never be reached.
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

// TestStream_ColdTierInitFailure_SingleLedgerRange covers the other
// half of streamTiered's cold-init-failure branch: a single-ledger
// bounded range (From == To) reuses the already-open hot store via
// the in-house walk rather than closing it and re-entering
// ApplyLedgerMetadata (which itself rejects single-ledger ranges —
// see TestStream_singleLedgerBoundedRange). Both branches must
// degrade to hot-only on a broken cold config.
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
