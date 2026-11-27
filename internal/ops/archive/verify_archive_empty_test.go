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
	return datastore.DataStoreConfig{NetworkPassphrase: emptyTestPassphrase, Compression: "zst"}
}

// liveStore holds 64-ledger files (no partition dirs) from the file containing firstLedger, plus a manifest.
func liveStore(t *testing.T, firstLedger uint32, withManifest bool) *memStore {
	t.Helper()
	s := &memStore{files: map[string][]byte{}}
	if withManifest {
		b, err := json.Marshal(datastore.DatastoreManifest{
			NetworkPassphrase: emptyTestPassphrase, Version: datastore.Version,
			Compression: "zst", LedgersPerFile: 64, FilesPerPartition: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		s.files[".config.json"] = b
	}
	schema := datastore.DataStoreSchema{LedgersPerFile: 64, FilesPerPartition: 1, FileExtension: "zst"}
	for start := firstLedger / 64 * 64; start < firstLedger/64*64+64*8; start += 64 {
		s.files[schema.GetObjectKeyFromSequenceNumber(start)] = []byte("x")
	}
	return s
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
		{"empty archive, live first file at the bound", &memStore{}, liveStore(t, 64, true), true, false},
		{"empty archive, live starts above the bound", &memStore{}, liveStore(t, 128, true), false, false},
		{"empty archive, live starts high (range unfilled)", &memStore{}, liveStore(t, 60_000_000, true), false, false},
		{"empty archive, live has no ledger files", &memStore{}, &memStore{files: map[string][]byte{}}, false, true},
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
		if archiveBucketEmptyByDesign(context.Background(), cfg, c.bucket) {
			t.Errorf("%s: answered empty-by-design without probing the configured archive bucket", name)
		}
	}
}
