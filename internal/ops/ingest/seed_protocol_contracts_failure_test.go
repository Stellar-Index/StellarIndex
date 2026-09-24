package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

const seedTestFactory = "CFACTORYSEEDTEST"

// flakyUpsertStore streams one creation row per ledger in ledgers and
// fails the upsert of any child named in failOn.
type flakyUpsertStore struct {
	ledgers  []uint32
	failOn   map[string]bool
	upserted []string
}

func (f *flakyUpsertStore) UpsertProtocolContract(_ context.Context, _, contractID, _ string, _ uint32) error {
	if f.failOn[contractID] {
		return errors.New("simulated postgres pressure")
	}
	f.upserted = append(f.upserted, contractID)
	return nil
}

func (f *flakyUpsertStore) StreamSorobanEvents(_ context.Context, _, _ uint32, _, _, _ []string,
	fn func(sorobanevents.Row) error,
) error {
	sym, err := base64.StdEncoding.DecodeString(scval.MustEncodeSymbol("deploy"))
	if err != nil {
		return err
	}
	for _, l := range f.ledgers {
		row := sorobanevents.Row{
			Ledger: l, TxHash: make([]byte, 32), ContractID: seedTestFactory,
			TopicCount: 1, Topic0XDR: sym,
		}
		if err := fn(row); err != nil {
			return err
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
