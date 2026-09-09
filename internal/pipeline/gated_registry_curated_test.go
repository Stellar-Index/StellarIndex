package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
)

// fakeProtocolContractStore is an in-memory protocol_contracts double.
// rows is the table; upserts records every write in order so a test can
// assert BOTH what was written and that nothing was.
type fakeProtocolContractStore struct {
	rows    map[string][]string // source → contract ids the table already holds
	upserts []upsertCall
	loadErr error
	failOn  string // contract id whose upsert returns an error
}

type upsertCall struct {
	source      string
	contractID  string
	factoryID   string
	firstLedger uint32
}

func (f *fakeProtocolContractStore) LoadProtocolContracts(_ context.Context, source string) ([]string, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return append([]string(nil), f.rows[source]...), nil
}

func (f *fakeProtocolContractStore) UpsertProtocolContract(_ context.Context, source, contractID, factoryID string, firstLedger uint32) error {
	if f.failOn != "" && contractID == f.failOn {
		return errors.New("upsert boom")
	}
	f.upserts = append(f.upserts, upsertCall{source, contractID, factoryID, firstLedger})
	f.rows[source] = append(f.rows[source], contractID)
	return nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// curatedSources returns every gatedSources entry whose trust root is an
// in-code curated set rather than a factory's creation events.
func curatedSources(t *testing.T) map[string]GatedMeta {
	t.Helper()
	out := map[string]GatedMeta{}
	for name, meta := range gatedSources {
		if len(meta.CuratedSet) > 0 {
			out[name] = meta
		}
	}
	if len(out) == 0 {
		t.Fatal("no curated-set source found in gatedSources — this test has stopped checking anything")
	}
	return out
}

// anyCuratedSource returns one curated entry; map order is irrelevant to
// the properties the callers assert.
func anyCuratedSource(t *testing.T) (string, GatedMeta) {
	t.Helper()
	for name, meta := range curatedSources(t) {
		return name, meta
	}
	t.Fatal("unreachable: curatedSources guarantees at least one entry")
	return "", GatedMeta{}
}

// TestGatedRegistryOptions_curatedSetSeededWithEmptyTable is THE invariant.
//
// It asserts on the OPTIONS the warm returns, deliberately NOT through
// meta.NewDecoder: every gated decoder's constructor re-installs its own
// MainnetGatedSet, so a decoder-level assertion passes whether or not the
// pipeline layer carries the curated set and would be vacuous. The thing
// that broke on r1 is the pipeline's warm — protocol_contracts held zero
// upshift rows and nothing but an operator command would ever have put one
// there — so the property to pin is that the warm's own output gates the
// declared trust root with an EMPTY table.
func TestGatedRegistryOptions_curatedSetSeededWithEmptyTable(t *testing.T) {
	store := &fakeProtocolContractStore{rows: map[string][]string{}} // the whole table is empty
	opts, err := gatedRegistryOptions(context.Background(), store, quietLogger(), context.Background(), false)
	if err != nil {
		t.Fatalf("gatedRegistryOptions: %v", err)
	}

	for name, meta := range curatedSources(t) {
		reg := contractid.New(opts[name]...)
		for _, id := range meta.CuratedSet {
			if !reg.Has(id) {
				t.Errorf("%s: the warm's options do not gate curated contract %s — "+
					"with protocol_contracts empty this source's events are dropped "+
					"and nothing reports it (r1, 2026-09-09)", name, id)
			}
		}
		if got, want := reg.Len(), len(meta.CuratedSet); got != want {
			t.Errorf("%s: warmed gate has %d contract(s), want the %d curated one(s)", name, got, want)
		}
	}
}

// TestGatedRegistryOptions_curatedSetUnionsWithTable pins that the warm
// ADDS the curated set to the table's rows rather than replacing them: a
// contract admitted by an operator (or by a live creation event) must
// survive alongside the in-code set.
func TestGatedRegistryOptions_curatedSetUnionsWithTable(t *testing.T) {
	const admitted = "CDADMITTEDBYOPERATORAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	name, meta := anyCuratedSource(t)
	store := &fakeProtocolContractStore{rows: map[string][]string{name: {admitted}}}
	opts, err := gatedRegistryOptions(context.Background(), store, quietLogger(), context.Background(), false)
	if err != nil {
		t.Fatalf("gatedRegistryOptions: %v", err)
	}
	reg := contractid.New(opts[name]...)
	if !reg.Has(admitted) {
		t.Errorf("%s: the operator-admitted contract %s was dropped by the warm", name, admitted)
	}
	for _, id := range meta.CuratedSet {
		if !reg.Has(id) {
			t.Errorf("%s: curated contract %s missing from the warmed gate", name, id)
		}
	}
}

// TestGatedRegistryOptions_readOnlyConsumersWriteNothing holds the clause
// the withHook flag exists for: the recognition / completeness audits must
// not mutate protocol_contracts while auditing it. Seeding an in-code
// constant into an in-memory registry is not that mutation (WithSeed fires
// no hook — TestRegistry_WithSeed_doesNotFireHook); writing a row is, and
// this asserts none is written.
func TestGatedRegistryOptions_readOnlyConsumersWriteNothing(t *testing.T) {
	store := &fakeProtocolContractStore{rows: map[string][]string{}}
	if _, err := gatedRegistryOptions(context.Background(), store, quietLogger(), context.Background(), false); err != nil {
		t.Fatalf("gatedRegistryOptions: %v", err)
	}
	if len(store.upserts) != 0 {
		t.Errorf("withHook=false wrote %d protocol_contracts row(s): %+v — an audit must not "+
			"register the contracts it is auditing", len(store.upserts), store.upserts)
	}
}

// TestGatedRegistryOptions_indexerReconcilesCuratedRows pins the second
// half of the fix: the indexer path heals the TABLE, which is what backs
// GET /v1/protocols/{name} and the explorer's contract attribution. The
// provenance must be byte-identical to what seed-protocol-contracts
// writes, or the two writers disagree about the same row.
func TestGatedRegistryOptions_indexerReconcilesCuratedRows(t *testing.T) {
	store := &fakeProtocolContractStore{rows: map[string][]string{}}
	if _, err := gatedRegistryOptions(context.Background(), store, quietLogger(), context.Background(), true); err != nil {
		t.Fatalf("gatedRegistryOptions: %v", err)
	}

	got := map[string][]upsertCall{}
	for _, u := range store.upserts {
		got[u.source] = append(got[u.source], u)
	}
	for name, meta := range curatedSources(t) {
		calls := got[name]
		if len(calls) != len(meta.CuratedSet) {
			t.Errorf("%s: reconcile wrote %d row(s), want %d", name, len(calls), len(meta.CuratedSet))
			continue
		}
		ids := make([]string, 0, len(calls))
		for _, c := range calls {
			ids = append(ids, c.contractID)
			if c.factoryID != CuratedFactoryID {
				t.Errorf("%s/%s: factory_id = %q, want %q (seed-protocol-contracts writes that)",
					name, c.contractID, c.factoryID, CuratedFactoryID)
			}
			if c.firstLedger != meta.Genesis {
				t.Errorf("%s/%s: first_ledger = %d, want the source genesis %d",
					name, c.contractID, c.firstLedger, meta.Genesis)
			}
		}
		want := append([]string(nil), meta.CuratedSet...)
		sort.Strings(ids)
		sort.Strings(want)
		for i := range want {
			if ids[i] != want[i] {
				t.Errorf("%s: reconciled %v, want the curated set %v", name, ids, want)
				break
			}
		}
	}

	// Factory-anchored sources have no in-code children to reconcile; the
	// warm must not invent rows for them.
	for name, meta := range gatedSources {
		if len(meta.CuratedSet) == 0 && len(got[name]) != 0 {
			t.Errorf("%s: warm wrote %d row(s) for a factory-anchored source", name, len(got[name]))
		}
	}
}

// TestGatedRegistryOptions_reconcileSkipsRowsAlreadyPresent keeps a
// restart from re-stamping observed_at on rows that are already correct —
// observed_at is the only freshness signal those rows carry.
func TestGatedRegistryOptions_reconcileSkipsRowsAlreadyPresent(t *testing.T) {
	rows := map[string][]string{}
	for name, meta := range curatedSources(t) {
		rows[name] = append([]string(nil), meta.CuratedSet...)
	}
	store := &fakeProtocolContractStore{rows: rows}
	if _, err := gatedRegistryOptions(context.Background(), store, quietLogger(), context.Background(), true); err != nil {
		t.Fatalf("gatedRegistryOptions: %v", err)
	}
	if len(store.upserts) != 0 {
		t.Errorf("a fully-seeded table still took %d write(s): %+v", len(store.upserts), store.upserts)
	}
}

// TestGatedRegistryOptions_reconcileFailureDoesNotBreakTheGate: the gate
// is complete in memory before the reconcile runs, so a database that
// refuses the write degrades the roster, never ingestion.
func TestGatedRegistryOptions_reconcileFailureDoesNotBreakTheGate(t *testing.T) {
	name, meta := anyCuratedSource(t)
	store := &fakeProtocolContractStore{rows: map[string][]string{}, failOn: meta.CuratedSet[0]}
	opts, err := gatedRegistryOptions(context.Background(), store, quietLogger(), context.Background(), true)
	if err != nil {
		t.Fatalf("a failed curated upsert must not fail the warm: %v", err)
	}
	reg := contractid.New(opts[name]...)
	for _, id := range meta.CuratedSet {
		if !reg.Has(id) {
			t.Errorf("%s: curated contract %s missing from the gate after a failed reconcile", name, id)
		}
	}
}

// TestGatedSources_curatedOnlyDeclaresTrustRoot is the registry-parity
// guard for the next curated source. A curated-only entry (no factories,
// no creation symbol) with an empty CuratedSet declares a gate with NO
// trust root: it drops every event, and `seed-protocol-contracts -source
// <name>` reports "upserted 0 child contract(s)" and exits 0.
func TestGatedSources_curatedOnlyDeclaresTrustRoot(t *testing.T) {
	for name, meta := range gatedSources {
		if len(meta.Factories) != 0 {
			continue
		}
		if len(meta.CuratedSet) == 0 {
			t.Errorf("%s is curated-only (no factories) but declares no CuratedSet — "+
				"its gate has no trust root and every event it sees is dropped", name)
		}
		if meta.CreationSym != "" {
			t.Errorf("%s has no factories but declares CreationSym %q — nothing will ever emit it",
				name, meta.CreationSym)
		}
		if meta.Genesis == 0 {
			t.Errorf("%s declares no Genesis; it is the first_ledger stamped on its curated rows", name)
		}
	}
}

// TestSeedCuratedContracts_emptySetIsAnError pins the CLI half: a curated
// source with nothing to seed must fail loudly rather than print a
// zero-row success an operator will read as "seeded".
func TestSeedCuratedContracts_emptySetIsAnError(t *testing.T) {
	store := &fakeProtocolContractStore{rows: map[string][]string{}}
	n, err := seedCuratedContracts(context.Background(), store, "nosuch", GatedMeta{}, nil)
	if err == nil {
		t.Fatalf("seeding an empty curated set returned (%d, nil), want an error", n)
	}
	if n != 0 {
		t.Errorf("seeded = %d, want 0", n)
	}
	if len(store.upserts) != 0 {
		t.Errorf("wrote %d row(s) for an empty curated set", len(store.upserts))
	}
}

// TestGatedRegistryOptions_loadErrorFailsTheWarm keeps a database error
// from being mistaken for "this source has no contracts": the warm must
// surface it, not return a silently empty gate.
func TestGatedRegistryOptions_loadErrorFailsTheWarm(t *testing.T) {
	store := &fakeProtocolContractStore{rows: map[string][]string{}, loadErr: errors.New("db down")}
	if _, err := gatedRegistryOptions(context.Background(), store, quietLogger(), context.Background(), false); err == nil {
		t.Fatal("a protocol_contracts read failure must fail the warm")
	}
}

// capturingHandler records the level and `source` attribute of every log
// record the warm emits.
type capturingHandler struct {
	records []capturedRecord
}

type capturedRecord struct {
	level  slog.Level
	msg    string
	source string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "source" {
			rec.source = a.Value.String()
		}
		return true
	})
	h.records = append(h.records, rec)
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

// TestGatedRegistryOptions_emptyGateWarns covers the case no in-code seed
// can fix: a FACTORY-anchored source whose genesis walk has not run holds
// an empty gate and drops every event while every upstream signal (cursor
// advancing, lake complete, decoder linked into the binary) still reads
// healthy. Its children are discovered from creation events, so there is
// nothing in code to seed them with — the only available answer is to
// stop `children=0` reading as normal.
func TestGatedRegistryOptions_emptyGateWarns(t *testing.T) {
	var factoryAnchored []string
	for name, meta := range gatedSources {
		if len(meta.CuratedSet) == 0 {
			factoryAnchored = append(factoryAnchored, name)
		}
	}
	if len(factoryAnchored) == 0 {
		t.Skip("no factory-anchored source without a curated set")
	}

	h := &capturingHandler{}
	store := &fakeProtocolContractStore{rows: map[string][]string{}}
	if _, err := gatedRegistryOptions(context.Background(), store, slog.New(h), context.Background(), true); err != nil {
		t.Fatalf("gatedRegistryOptions: %v", err)
	}

	warned := map[string]bool{}
	for _, r := range h.records {
		if r.level >= slog.LevelWarn {
			warned[r.source] = true
		}
	}
	for _, name := range factoryAnchored {
		if !warned[name] {
			t.Errorf("%s warmed an EMPTY gate with no WARN — every event it sees is dropped, "+
				"and the only line about it says children=0, which reads exactly like a "+
				"source that has not deployed a pool yet", name)
		}
	}
	// Curated sources can no longer reach the empty-gate state at all.
	for name := range curatedSources(t) {
		if warned[name] {
			t.Errorf("%s warned about an empty gate, but its curated set is seeded from code", name)
		}
	}
}
