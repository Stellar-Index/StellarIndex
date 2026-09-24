package ledgerstream_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

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

func pathCount(path string) float64 {
	return testutil.ToFloat64(obs.LedgerstreamStreamPathTotal.WithLabelValues(path))
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
