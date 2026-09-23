package ingest

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// fakeGatedSeedStore records upserts and serves no creation events: the
// DeFindex decoder admits no child from a `create` event anyway, so a real
// lake walk would contribute nothing either.
type fakeGatedSeedStore struct {
	upserts map[string]string // contract id → factory id
	walks   int
}

func (f *fakeGatedSeedStore) UpsertProtocolContract(_ context.Context, _, contractID, factoryID string, _ uint32) error {
	f.upserts[contractID] = factoryID
	return nil
}

func (f *fakeGatedSeedStore) StreamSorobanEvents(context.Context, uint32, uint32, []string, []string, []string,
	func(sorobanevents.Row) error,
) error {
	f.walks++
	return nil
}

// TestSeedOneGatedSource_defindexSeedsCuratedSet pins that
// `seed-protocol-contracts -source defindex` writes DeFindex's curated
// vaults + strategies. Its factories route it to the factory walk, which
// seeds nothing because the decoder never admits a child from the
// permissionless factory's `create` events; the CLI used to exit 0 with
// "upserted 0".
func TestSeedOneGatedSource_defindexSeedsCuratedSet(t *testing.T) {
	want := defindex.MainnetGatedSet()

	t.Run("write", func(t *testing.T) {
		store := &fakeGatedSeedStore{upserts: map[string]string{}}
		n, err := seedOneGatedSource(context.Background(), store, true, defindex.SourceName, 100)
		if err != nil {
			t.Fatalf("seedOneGatedSource: %v", err)
		}
		if n != len(want) {
			t.Errorf("reported %d contract(s) seeded, want %d", n, len(want))
		}
		for _, id := range want {
			factory, ok := store.upserts[id]
			if !ok {
				t.Errorf("curated defindex contract %s not upserted", id)
				continue
			}
			if factory != pipeline.CuratedFactoryID {
				t.Errorf("%s upserted with factory_id %q, want %q", id, factory, pipeline.CuratedFactoryID)
			}
		}
		if store.walks != 1 {
			t.Errorf("walked the factory creation events %d time(s), want 1", store.walks)
		}
	})

	t.Run("preview", func(t *testing.T) {
		store := &fakeGatedSeedStore{upserts: map[string]string{}}
		n, err := seedOneGatedSource(context.Background(), store, false, defindex.SourceName, 100)
		if err != nil {
			t.Fatalf("seedOneGatedSource preview: %v", err)
		}
		if n != len(want) {
			t.Errorf("preview reported %d contract(s), want the %d it would upsert", n, len(want))
		}
		if len(store.upserts) != 0 {
			t.Errorf("preview wrote %d row(s)", len(store.upserts))
		}
	})
}
