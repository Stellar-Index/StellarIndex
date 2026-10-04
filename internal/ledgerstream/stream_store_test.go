package ledgerstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
)

func TestStreamStore_WalksCallerOpenedStore(t *testing.T) {
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := datastore.NewFilesystemDataStoreWithPath(tmp)
	if err != nil {
		t.Fatalf("open filesystem datastore: %v", err)
	}
	dsCfg := datastore.DataStoreConfig{
		Type:              "Filesystem",
		Params:            map[string]string{"destination_path": tmp},
		Schema:            datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 1},
		NetworkPassphrase: "Test SDF Network ; September 2015",
		Compression:       "zstd",
	}
	if _, _, err := datastore.PublishConfig(ctx, store, dsCfg); err != nil {
		t.Fatalf("publish config: %v", err)
	}
	writeLedgerFixture(t, ctx, store, dsCfg.Schema, 5)
	writeLedgerFixture(t, ctx, store, dsCfg.Schema, 6)

	var got []uint32
	err = ledgerstream.StreamStore(ctx, ledgerstream.Config{DataStore: dsCfg}, store, 5, 6, func(lcm xdr.LedgerCloseMeta) error {
		got = append(got, lcm.LedgerSequence())
		return nil
	})
	if err != nil {
		t.Fatalf("StreamStore: %v", err)
	}
	if len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("delivered %v, want [5 6]", got)
	}
}
