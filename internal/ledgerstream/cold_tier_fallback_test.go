package ledgerstream_test

import (
	"context"
	"errors"
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

// TestStream_ColdDataStoreFactory_TakesPrecedence proves the ADR-0027
// cold-tier credential fix is actually reachable from Stream: when
// Config.ColdDataStoreFactory is set, streamTiered must open the cold
// tier through it and NOT through datastore.NewDataStore.
//
// Why that matters (2026-07-25 incident): datastore.NewDataStore builds
// every S3 client from the ambient AWS credential chain, which on r1
// carries local MinIO's credentials because the HOT tier authenticates
// through it. Those keys were then presented to real AWS and every cold
// read failed with `InvalidAccessKeyId: The AWS Access Key Id you
// provided does not exist in our records`. The factory is how
// pipeline.NewColdDataStore — which resolves the cold credentials
// explicitly — gets injected here.
//
// The test is arranged so the two paths are distinguishable by outcome
// rather than by inspection: ColdDataStore.Type is an unsupported type,
// so a datastore.NewDataStore call would fail and degrade to hot-only,
// which cannot serve ledger 6 (hot holds only 5). Delivering both
// ledgers is only possible if the factory opened the cold store.
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

// TestStream_ColdDataStoreFactoryError_FallsBackToHotOnly keeps the
// ADR-0027 "cold tier is optional" invariant across the new hook: a
// factory that fails (e.g. pipeline.NewColdDataStore refusing a
// half-configured credential pair) must degrade to hot-only exactly
// like a datastore.NewDataStore failure does, not abort the walk.
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
