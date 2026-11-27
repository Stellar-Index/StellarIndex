package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// memStore is an in-memory datastore.DataStore; listErr makes every listing fail.
type memStore struct {
	files   map[string][]byte
	listErr error
}

func (m *memStore) GetFileMetadata(context.Context, string) (map[string]string, error) {
	return nil, nil
}

func (m *memStore) GetFileLastModified(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func (m *memStore) GetFile(_ context.Context, p string) (io.ReadCloser, int64, error) {
	b, ok := m.files[p]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (m *memStore) PutFile(context.Context, string, io.WriterTo, map[string]string) error {
	return errors.New("read-only")
}

func (m *memStore) PutFileIfNotExists(context.Context, string, io.WriterTo, map[string]string) (bool, error) {
	return false, errors.New("read-only")
}

func (m *memStore) Exists(_ context.Context, p string) (bool, error) {
	_, ok := m.files[p]
	return ok, nil
}

func (m *memStore) Size(_ context.Context, p string) (int64, error) {
	return int64(len(m.files[p])), nil
}
func (m *memStore) Close() error { return nil }
func (m *memStore) ListFilePaths(_ context.Context, o datastore.ListFileOptions) ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var keys []string
	for k := range m.files {
		if strings.HasPrefix(k, o.Prefix) && k > o.StartAfter {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if o.Limit > 0 && len(keys) > int(o.Limit) {
		keys = keys[:o.Limit]
	}
	return keys, nil
}

const emptyTestPassphrase = "Test SDF Network ; September 2015"

func emptyTestLiveCfg() datastore.DataStoreConfig {
	return datastore.DataStoreConfig{NetworkPassphrase: emptyTestPassphrase, Compression: "zstd"}
}

// liveStoreLPF holds 8 files of lpf ledgers each (no partition dirs) from the file containing
// firstLedger, plus a manifest when withManifest.
func liveStoreLPF(t *testing.T, firstLedger, lpf uint32, withManifest, withFiles bool) *memStore {
	t.Helper()
	s := &memStore{files: map[string][]byte{}}
	if withManifest {
		b, err := json.Marshal(datastore.DatastoreManifest{
			NetworkPassphrase: emptyTestPassphrase, Version: datastore.Version,
			Compression: "zstd", LedgersPerFile: lpf, FilesPerPartition: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		s.files[".config.json"] = b
	}
	if !withFiles {
		return s
	}
	schema := datastore.DataStoreSchema{LedgersPerFile: lpf, FilesPerPartition: 1, FileExtension: "zst"}
	for start := firstLedger / lpf * lpf; start < firstLedger/lpf*lpf+lpf*8; start += lpf {
		s.files[schema.GetObjectKeyFromSequenceNumber(start)] = []byte("x")
	}
	return s
}

// liveStore is a live bucket of 64-ledger files, the layout a test net uses.
func liveStore(t *testing.T, firstLedger uint32, withManifest bool) *memStore {
	t.Helper()
	return liveStoreLPF(t, firstLedger, 64, withManifest, true)
}

func TestArchiveEmptyByDesign(t *testing.T) {
	cases := []struct {
		name    string
		archive *memStore
		live    *memStore
		want    bool
		wantErr bool
	}{
		{"empty archive, live from ledger 2", &memStore{}, liveStore(t, 2, true), true, false},
		{"empty archive, live first file --0-63", &memStore{}, liveStore(t, 0, true), true, false},
		{"empty archive, 1-ledger live files from 2", &memStore{}, liveStoreLPF(t, 2, 1, true, true), true, false},
		{"empty archive, 1-ledger live files from 3 (ledger 2 missing)", &memStore{}, liveStoreLPF(t, 3, 1, true, true), false, false},
		{"empty archive, live first file --64-127 (ledgers 2-63 missing)", &memStore{}, liveStore(t, 64, true), false, false},
		{"empty archive, live starts above the bound", &memStore{}, liveStore(t, 128, true), false, false},
		{"empty archive, live starts high (range unfilled)", &memStore{}, liveStore(t, 60_000_000, true), false, false},
		{"empty archive, live has no manifest and no ledger files", &memStore{}, &memStore{files: map[string][]byte{}}, false, true},
		{"empty archive, live manifest but no ledger files (start unmeasurable)", &memStore{}, liveStoreLPF(t, 2, 64, true, false), false, true},
		{"empty archive, live manifest missing", &memStore{}, liveStore(t, 2, false), false, true},
		{"archive listing fails", &memStore{listErr: errors.New("AccessDenied")}, liveStore(t, 2, true), false, true},
		{"archive holds only a manifest", &memStore{files: map[string][]byte{".config.json": []byte("{}")}}, liveStore(t, 2, true), false, false},
		{
			"archive holds ledger files, manifest missing",
			&memStore{files: map[string][]byte{fmt.Sprintf("%08X--0-63.xdr.zst", uint32(0xFFFFFFFF)): []byte("x")}}, liveStore(t, 2, true), false, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := archiveEmptyByDesign(context.Background(), c.archive, c.live, emptyTestLiveCfg())
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want {
				t.Fatalf("archiveEmptyByDesign = %v, want %v", got, c.want)
			}
		})
	}
}

func TestArchiveBucketEmptyByDesign_OnlyForConfiguredArchiveBucket(t *testing.T) {
	var cfg config.Config
	cfg.Storage.S3BucketArchive = "galexie-archive"
	for name, c := range map[string]struct {
		live, bucket string
	}{
		"no live bucket":       {"", "galexie-archive"},
		"walk aimed elsewhere": {"galexie-live", "galexie-live"},
		"other bucket":         {"galexie-live", "somewhere-else"},
	} {
		cfg.Storage.S3BucketLive = c.live
		if archiveBucketEmptyByDesign(context.Background(), cfg, c.bucket, failOpen(t)) {
			t.Errorf("%s: answered empty-by-design without probing the configured archive bucket", name)
		}
	}
}

// failOpen fails the test if a store is opened: the gating cases must answer before any I/O.
func failOpen(t *testing.T) storeOpener {
	return func(context.Context, datastore.DataStoreConfig) (datastore.DataStore, error) {
		t.Helper()
		t.Error("opened a datastore for a bucket that is not the configured archive")
		return nil, errors.New("unexpected open")
	}
}

// byBucket opens the in-memory store configured for each bucket path.
func byBucket(stores map[string]*memStore, openErr error) storeOpener {
	return func(_ context.Context, cfg datastore.DataStoreConfig) (datastore.DataStore, error) {
		if openErr != nil {
			return nil, openErr
		}
		return stores[cfg.Params["destination_bucket_path"]], nil
	}
}

func wiringConfig() config.Config {
	var cfg config.Config
	cfg.Storage.S3BucketArchive = "galexie-archive"
	cfg.Storage.S3BucketLive = "galexie-live"
	cfg.Stellar.Network = "testnet"
	return cfg
}

// TestShouldWalkArchive pins the verifyArchive wiring end to end over the real probe: the
// walk is skipped only for an archive that is empty by design, and runs in every other state
// (including a probe that errors), so a green exit cannot hide an unverified archive.
func TestShouldWalkArchive(t *testing.T) {
	cfg := wiringConfig()
	liveLow := liveStore(t, 2, true)
	cases := []struct {
		name              string
		stores            map[string]*memStore
		openErr           error
		chain, checkpoint bool
		want              bool
	}{
		{"empty by design: walk skipped", map[string]*memStore{"galexie-archive": {}, "galexie-live": liveLow}, nil, true, false, false},
		{"empty by design: checkpoint tier skipped too", map[string]*memStore{"galexie-archive": {}, "galexie-live": liveLow}, nil, false, true, false},
		{
			"archive has ledger files: walk runs",
			map[string]*memStore{"galexie-archive": {files: map[string][]byte{"FFFFFFFF--0-63.xdr.zst": []byte("x")}}, "galexie-live": liveLow},
			nil, true, true, true,
		},
		{
			"archive has only a manifest: walk runs",
			map[string]*memStore{"galexie-archive": {files: map[string][]byte{".config.json": []byte("{}")}}, "galexie-live": liveLow},
			nil, true, false, true,
		},
		{"live starts high: walk runs", map[string]*memStore{"galexie-archive": {}, "galexie-live": liveStore(t, 128, true)}, nil, true, false, true},
		{"archive listing errors: walk runs", map[string]*memStore{"galexie-archive": {listErr: errors.New("AccessDenied")}, "galexie-live": liveLow}, nil, true, false, true},
		{"open error: walk runs", map[string]*memStore{}, errors.New("no such host"), true, false, true},
		{"no walk tier asked: nothing to walk", map[string]*memStore{"galexie-archive": {files: map[string][]byte{"k": nil}}, "galexie-live": liveLow}, nil, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probe := func(ctx context.Context, cfg config.Config, bucket string) bool {
				return archiveBucketEmptyByDesign(ctx, cfg, bucket, byBucket(c.stores, c.openErr))
			}
			if got := shouldWalkArchive(context.Background(), c.chain, c.checkpoint, cfg, "galexie-archive", probe); got != c.want {
				t.Fatalf("shouldWalkArchive = %v, want %v", got, c.want)
			}
		})
	}
}
