package ledgerstream_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

// TestStreamArchiveThenLive_crossesSeam covers the production happy path: ledgers 5..9 in the
// archive bucket, 10..15 in live, seam=10. It must read archive [5,9] bounded, then live [10, inf)
// until the callback signals stop.
func TestStreamArchiveThenLive_crossesSeam(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const seam = 10
	const from = 5
	const lastLive = 15

	archiveCfg, _ := newSeededFilesystemDataStore(t, ctx, from, seam-1)
	liveCfg, _ := newSeededFilesystemDataStore(t, ctx, seam, lastLive)

	stop := errors.New("stop-after-last-live")
	got := make([]uint32, 0, lastLive-from+1)
	err := ledgerstream.StreamArchiveThenLive(
		ctx,
		ledgerstream.Config{DataStore: archiveCfg},
		ledgerstream.Config{DataStore: liveCfg},
		from, seam, nil,
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm.LedgerSequence())
			if lcm.LedgerSequence() == lastLive {
				return stop
			}
			return nil
		},
	)
	if !errors.Is(err, stop) {
		t.Fatalf("expected stop sentinel, got %v", err)
	}
	if len(got) != int(lastLive-from+1) {
		t.Fatalf("received %d ledgers, want %d (got=%v)",
			len(got), lastLive-from+1, got)
	}
	for i, seq := range got {
		want := uint32(from) + uint32(i)
		if seq != want {
			t.Errorf("got[%d]=%d, want %d (full=%v)", i, seq, want, got)
		}
	}
}

// TestStreamArchiveThenLive_seamZeroLiveOnly verifies the seam=0 short-circuit: the archive bucket is unused and the
// call degrades to a plain unbounded Stream on the live config, keeping the single-bucket deployment shape working.
func TestStreamArchiveThenLive_seamZeroLiveOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const from = 5
	const lastLive = 8

	// Archive cfg points at an empty datastore — if the function
	// reads from it, the test fails (no manifest, no objects).
	archiveCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": t.TempDir()},
	}
	liveCfg, _ := newSeededFilesystemDataStore(t, ctx, from, lastLive)

	stop := errors.New("stop")
	got := make([]uint32, 0, lastLive-from+1)
	err := ledgerstream.StreamArchiveThenLive(
		ctx,
		ledgerstream.Config{DataStore: archiveCfg},
		ledgerstream.Config{DataStore: liveCfg},
		from, 0, /*seam=0 → live-only*/
		nil,
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm.LedgerSequence())
			if lcm.LedgerSequence() == lastLive {
				return stop
			}
			return nil
		},
	)
	if !errors.Is(err, stop) {
		t.Fatalf("expected stop sentinel, got %v", err)
	}
	if len(got) != int(lastLive-from+1) {
		t.Fatalf("received %d ledgers, want %d (got=%v)",
			len(got), lastLive-from+1, got)
	}
}

// TestStreamArchiveThenLive_fromAboveSeamLiveOnly covers resuming from a cursor already past the seam (from=20,
// seam=10): the archive read is skipped, since seam-1=9 < from=20 would be an inverted bounded range that crashes the
// SDK.
func TestStreamArchiveThenLive_fromAboveSeamLiveOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const from = 20
	const seam = 10
	const lastLive = 22

	// Empty archive — same rationale as the seam=0 test.
	archiveCfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": t.TempDir()},
	}
	liveCfg, _ := newSeededFilesystemDataStore(t, ctx, from, lastLive)

	stop := errors.New("stop")
	got := make([]uint32, 0, lastLive-from+1)
	err := ledgerstream.StreamArchiveThenLive(
		ctx,
		ledgerstream.Config{DataStore: archiveCfg},
		ledgerstream.Config{DataStore: liveCfg},
		from, seam, nil,
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm.LedgerSequence())
			if lcm.LedgerSequence() == lastLive {
				return stop
			}
			return nil
		},
	)
	if !errors.Is(err, stop) {
		t.Fatalf("expected stop sentinel, got %v", err)
	}
	if len(got) != int(lastLive-from+1) {
		t.Fatalf("received %d ledgers, want %d (got=%v)",
			len(got), lastLive-from+1, got)
	}
}

// newSeededFilesystemDataStore creates a temp filesystem datastore
// pre-seeded with ledgers [from, to] inclusive. Returns the config
// (suitable for ledgerstream.Config.DataStore) and the dir.
func newSeededFilesystemDataStore(t *testing.T, ctx context.Context, from, to uint32) (datastore.DataStoreConfig, string) {
	t.Helper()
	dir := t.TempDir()

	store, err := datastore.NewFilesystemDataStoreWithPath(dir)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := datastore.DataStoreConfig{
		Type:   "Filesystem",
		Params: map[string]string{"destination_path": dir},
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
	for seq := from; seq <= to; seq++ {
		writeLedgerFixture(t, ctx, store, cfg.Schema, seq)
	}
	return cfg, dir
}

// TestStreamArchiveThenLive_trailingGapRefusesHandoff pins the silent seam gap. TolerateTrailingMissing makes a hole
// at the archive's trailing edge succeed; handing off to the fixed seam would skip those ledgers permanently. With
// [5,7] archived and a seam implying [5,9], the stream must refuse to hand off rather than skip 8 and 9.
func TestStreamArchiveThenLive_trailingGapRefusesHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const seam = 10
	const from = 5
	const archiveLast = 7 // two ledgers short of seam-1 (=9)

	archiveDS, _ := newSeededFilesystemDataStore(t, ctx, from, archiveLast)
	liveDS, _ := newSeededFilesystemDataStore(t, ctx, seam, 15)

	var got []uint32
	err := ledgerstream.StreamArchiveThenLive(
		ctx,
		// TolerateTrailingMissing mirrors production (pipeline.LedgerstreamConfig
		// sets it for both buckets) — it is what turns the hole into a success.
		ledgerstream.Config{DataStore: archiveDS, TolerateTrailingMissing: true},
		ledgerstream.Config{DataStore: liveDS, TolerateTrailingMissing: true},
		from, seam, nil,
		func(lcm xdr.LedgerCloseMeta) error {
			got = append(got, lcm.LedgerSequence())
			return nil
		},
	)
	if err == nil {
		t.Fatalf("expected an error: the archive delivered only up to %d but the seam is %d, "+
			"so handing off would skip ledgers %d-%d permanently. got=%v",
			archiveLast, seam, archiveLast+1, seam-1, got)
	}
	// The message must name the gap, not just fail — an operator needs to know
	// WHICH ledgers are missing to re-drive them.
	for _, want := range []string{"archive phase", "never delivered", "refusing to hand off"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
	// It must fail BEFORE the live phase runs — no live ledger may be consumed.
	for _, seq := range got {
		if seq >= seam {
			t.Errorf("consumed live ledger %d despite the archive gap — the handoff happened "+
				"anyway, which is the bug (got=%v)", seq, got)
		}
	}
}
