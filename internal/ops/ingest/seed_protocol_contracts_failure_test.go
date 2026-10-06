package ingest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

const seedTestFactory = "CFACTORYSEEDTEST"

// flakyUpsertStore streams one creation event per ledger in ledgers and
// fails the upsert of any child named in failOn.
type flakyUpsertStore struct {
	ledgers  []uint32
	failOn   map[string]bool
	dupParts bool // stream every event twice, as an unmerged duplicate part does
	upserted []string
}

func (f *flakyUpsertStore) UpsertProtocolContract(_ context.Context, _, contractID, _ string, _ uint32) error {
	if f.failOn[contractID] {
		return errors.New("simulated postgres pressure")
	}
	f.upserted = append(f.upserted, contractID)
	return nil
}

func (f *flakyUpsertStore) StreamContractEvents(_ context.Context, _, _ uint32, _, _ []string,
	fn func(events.Event) error,
) error {
	for _, l := range f.ledgers {
		ev := events.Event{Ledger: l, TxHash: fmt.Sprintf("tx%d", l), ContractID: seedTestFactory}
		if err := fn(ev); err != nil {
			return err
		}
		if f.dupParts {
			if err := fn(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

// childPerLedgerDecoder admits one child per creation event, named after
// its ledger, through the registry hook the seed installs.
type childPerLedgerDecoder struct{ reg *contractid.Registry }

func (childPerLedgerDecoder) Name() string              { return "seedtest" }
func (childPerLedgerDecoder) Matches(events.Event) bool { return true }
func (d childPerLedgerDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	d.reg.Seed(fmt.Sprintf("CHILD%d", ev.Ledger), ev.ContractID, ev.Ledger)
	return nil, nil
}

func seedTestMeta() pipeline.GatedMeta {
	return pipeline.GatedMeta{
		Factories:   []string{seedTestFactory},
		CreationSym: "deploy",
		Genesis:     10,
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder {
			return childPerLedgerDecoder{reg: contractid.New(opts...)}
		},
	}
}

// A failed protocol_contracts upsert must fail the run: the gated decoder
// drops that child's events forever, so "upserted N" + exit 0 is a false
// deploy-precondition pass.
func TestWalkFactoryCreations_UpsertFailureFailsRun(t *testing.T) {
	store := &flakyUpsertStore{ledgers: []uint32{11, 12, 13}, failOn: map[string]bool{"CHILD12": true}}
	n, err := walkFactoryCreations(context.Background(), store, true, "seedtest", seedTestMeta(), 20)
	if err == nil {
		t.Fatalf("1 of 3 upserts failed but the walk returned nil (seeded %d)", n)
	}
	if !strings.Contains(err.Error(), "1 creation event(s) or child upsert(s) failed") {
		t.Errorf("error %q does not report the failure count", err)
	}
	if n != 2 {
		t.Errorf("seeded = %d, want the 2 that landed", n)
	}
	if len(store.upserted) != 2 {
		t.Errorf("walk stopped early: upserted %v, want the other 2 children still attempted", store.upserted)
	}
}

func TestWalkFactoryCreations_AllUpsertsLandIsNil(t *testing.T) {
	store := &flakyUpsertStore{ledgers: []uint32{11, 12}}
	n, err := walkFactoryCreations(context.Background(), store, true, "seedtest", seedTestMeta(), 20)
	if err != nil || n != 2 {
		t.Fatalf("clean walk = (%d, %v), want (2, nil)", n, err)
	}
}

// The lake stream reads without FINAL: a creation event in an unmerged
// duplicate part must be counted and upserted once, not twice.
func TestWalkFactoryCreations_DuplicatePartCountedOnce(t *testing.T) {
	store := &flakyUpsertStore{ledgers: []uint32{11, 12}, dupParts: true}
	n, err := walkFactoryCreations(context.Background(), store, true, "seedtest", seedTestMeta(), 20)
	if err != nil || n != 2 {
		t.Fatalf("walk over duplicated parts = (%d, %v), want (2, nil)", n, err)
	}
	if len(store.upserted) != 2 {
		t.Errorf("upserted %v, want each child once", store.upserted)
	}
}

// The summary line names the walk's real lower bound, the factory genesis.
func TestSeedScope_NamesFactoryGenesis(t *testing.T) {
	names := pipeline.GatedSourceNames()
	for _, src := range names {
		meta, _ := pipeline.GatedMetaFor(src)
		if len(meta.Factories) == 0 {
			continue
		}
		want := fmt.Sprintf("walked [%d, 999]", meta.Genesis)
		if got := seedScope(src, 999); !strings.Contains(got, want) {
			t.Errorf("seedScope(%s) = %q, want it to contain %q", src, got, want)
		}
		return
	}
	t.Skip("no factory-anchored gated source registered")
}
