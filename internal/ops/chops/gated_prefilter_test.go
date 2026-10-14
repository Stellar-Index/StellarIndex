// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
)

// Mock identity-gated decoder mirroring the aquarius/phoenix gate: a "biz" event counts only from a registered contract,
// and a factory-only "create" event registers the child named in its Value.

const (
	prefFactory = "CFACTORYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	prefSeed    = "CSEEDPOOLAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	prefInWin   = "CINWINDOWCHILDAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	prefForeign = "CFOREIGNLOOKALIKEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

type mockOut struct{}

func (mockOut) EventKind() string { return "mock.biz" }
func (mockOut) Source() string    { return "mock" }

type mockGatedDecoder struct{ reg *contractid.Registry }

func newMockGatedDecoder() *mockGatedDecoder {
	return &mockGatedDecoder{reg: contractid.New(
		contractid.WithFactories([]string{prefFactory}),
		contractid.WithSeed([]string{prefSeed}),
	)}
}

func mockTopic0(ev events.Event) string {
	if len(ev.Topic) == 0 {
		return ""
	}
	return ev.Topic[0]
}

func (d *mockGatedDecoder) Matches(ev events.Event) bool {
	switch mockTopic0(ev) {
	case "create":
		return d.reg.IsFactory(ev.ContractID) // only the factory announces children
	case "biz":
		return d.reg.Has(ev.ContractID) // only a registered child's business events count
	}
	return false
}

func (d *mockGatedDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	switch mockTopic0(ev) {
	case "create":
		d.reg.Seed(ev.Value, ev.ContractID, ev.Ledger) // Value carries the announced child
		return nil, nil
	case "biz":
		return []consumer.Event{mockOut{}}, nil
	}
	return nil, nil
}

func (d *mockGatedDecoder) GatedContractSet() []string { return d.reg.GatedSet() }

// countingEventStreamer models the CH contract_events reader: it applies the contract_id IN (...)
// prefilter and counts the rows it streamed, so tests can prove the prefilter really narrows the
// read. topic0Syms is ignored; dropping that redundant prefilter only streams a superset.
type countingEventStreamer struct {
	evs      []events.Event
	streamed *int
}

func (s countingEventStreamer) StreamContractEvents(
	_ context.Context, from, to uint32, contractIDs, _ []string,
	fn func(events.Event) error,
) error {
	var idset map[string]struct{}
	if len(contractIDs) > 0 {
		idset = make(map[string]struct{}, len(contractIDs))
		for _, c := range contractIDs {
			idset[c] = struct{}{}
		}
	}
	for _, ev := range s.evs {
		if ev.Ledger < from || ev.Ledger > to {
			continue
		}
		if idset != nil {
			if _, ok := idset[ev.ContractID]; !ok {
				continue
			}
		}
		if s.streamed != nil {
			*s.streamed++
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

func mockBiz(ledger uint32, contractID string, eventIndex int) events.Event {
	return events.Event{
		Ledger: ledger, ContractID: contractID, TxHash: "tx",
		OperationIndex: 0, EventIndex: eventIndex, Topic: []string{"biz"},
	}
}

func mockCreate(ledger uint32, factory, child string, eventIndex int) events.Event {
	return events.Event{
		Ledger: ledger, ContractID: factory, TxHash: "tx",
		OperationIndex: 0, EventIndex: eventIndex, Topic: []string{"create"}, Value: child,
	}
}

// Restricting the re-derive to a factory-gated source's contract set gives per-ledger/kind counts identical to the
// whole-lake stream. Non-vacuous: a foreign look-alike row is excluded, an in-window child still self-seeds, and the
// factory root keeps streaming.
func TestGatedPrefilter_identicalCountsSelfSeedPreserved(t *testing.T) {
	const lo, hi = uint32(100), uint32(200)
	// Order matters: the factory's create (ledger 100) precedes the child's business event (ledger 110), so the unfiltered
	// stream self-seeds the child before counting it; the prefilter must not disturb that timing.
	evs := []events.Event{
		mockBiz(100, prefSeed, 0),                  // curated seed member → counted
		mockCreate(100, prefFactory, prefInWin, 1), // factory announces the in-window child
		mockBiz(110, prefInWin, 2),                 // in-window child → counted (self-seeded)
		mockBiz(120, prefForeign, 3),               // foreign look-alike → Matches rejects
		mockBiz(130, prefSeed, 4),                  // curated seed member → counted
	}

	src := reconSource{
		name:        "mockgated",
		genesis:     1,
		dec:         newMockGatedDecoder(),
		factories:   []string{prefFactory},
		creationSym: "create",
		newGatedDec: func() gatedDecoder { return newMockGatedDecoder() },
	}

	// Build the prefilter.
	pfStreamed := 0
	pf, _, err := gatedPrefilter(context.Background(), countingEventStreamer{evs: evs, streamed: &pfStreamed}, src, hi)
	if err != nil {
		t.Fatalf("gatedPrefilter: %v", err)
	}
	pfSet := map[string]bool{}
	for _, c := range pf {
		pfSet[c] = true
	}
	// Task requirement: the factory trust root is in the prefilter.
	if !pfSet[prefFactory] {
		t.Errorf("prefilter %v omits the factory %q — its creation events would stop streaming and in-window children would silently drop", pf, prefFactory)
	}
	// The in-window child (announced by the factory inside [lo,hi]) is in the
	// prefilter — otherwise its business events would be filtered out and
	// undercounted (a false red).
	if !pfSet[prefInWin] {
		t.Errorf("prefilter %v omits the in-window child %q — self-seeded pools would undercount", pf, prefInWin)
	}
	// The foreign look-alike is NOT in the prefilter (it is not gated).
	if pfSet[prefForeign] {
		t.Errorf("prefilter %v includes the un-gated foreign contract %q", pf, prefForeign)
	}
	if !sort.StringsAreSorted(pf) {
		t.Errorf("prefilter is not sorted: %v", pf)
	}

	// Unfiltered re-derive (empty contractIDs = whole-lake stream).
	unfStreamed := 0
	byKindUnfiltered, blindU, err := completeness.ReDeriveOutputCountsByKindFromEvents(
		context.Background(), countingEventStreamer{evs: evs, streamed: &unfStreamed}, newMockGatedDecoder(), nil, nil, lo, hi)
	if err != nil {
		t.Fatalf("unfiltered re-derive: %v", err)
	}
	// Filtered re-derive (the prefilter this fix passes).
	filStreamed := 0
	byKindFiltered, blindF, err := completeness.ReDeriveOutputCountsByKindFromEvents(
		context.Background(), countingEventStreamer{evs: evs, streamed: &filStreamed}, newMockGatedDecoder(), pf, nil, lo, hi)
	if err != nil {
		t.Fatalf("filtered re-derive: %v", err)
	}

	// The whole point: identical counts.
	if !reflect.DeepEqual(byKindUnfiltered, byKindFiltered) {
		t.Fatalf("prefilter changed the counts: unfiltered=%v filtered=%v", byKindUnfiltered, byKindFiltered)
	}
	if blindU.Any() || blindF.Any() {
		t.Fatalf("unexpected blind spots: unfiltered=%v filtered=%v", blindU, blindF)
	}
	// Non-vacuous: the counts are not empty, the in-window (self-seeded) child
	// IS counted, and the seed member is counted at both its ledgers.
	if got := byKindFiltered["mock.biz"][110]; got != 1 {
		t.Errorf("in-window self-seeded child count at ledger 110 = %d, want 1 (self-seeding not preserved)", got)
	}
	if got := byKindFiltered["mock.biz"][100]; got != 1 {
		t.Errorf("seed-member count at ledger 100 = %d, want 1", got)
	}
	if got := byKindFiltered["mock.biz"][130]; got != 1 {
		t.Errorf("seed-member count at ledger 130 = %d, want 1", got)
	}
	// Non-vacuous on the filter itself: the prefiltered read touched FEWER rows
	// (the foreign look-alike's row was excluded), yet the counts still matched.
	if !(filStreamed < unfStreamed) {
		t.Errorf("prefiltered read streamed %d rows, unfiltered %d — the prefilter excluded nothing, so the identical-count claim is vacuous", filStreamed, unfStreamed)
	}
}

// sorocredit's Matches admits registered Collateral children, so the catalogue must not pin contractIDs to the root (a
// hard per-event filter in ch-rebuild/ch-reproject and the re-derive prefilter); it must scope by the gated set.
func TestSorocreditCatalogue_GatedNotRootPinned(t *testing.T) {
	var src *reconSource
	cat := builtCatalogue(t)
	for i := range cat {
		if cat[i].name == sorocredit.SourceName {
			src = &cat[i]
		}
	}
	if src == nil {
		t.Fatal("sorocredit missing from the catalogue")
	}
	if len(src.contractIDs) != 0 {
		t.Errorf("sorocredit contractIDs = %v: a static pin drops child-emitted events Matches admits", src.contractIDs)
	}
	if src.newGatedDec == nil || len(src.factories) != 1 || src.factories[0] != sorocredit.MainnetContract ||
		src.creationSym != sorocredit.TopicNewCollateralContract {
		t.Fatalf("sorocredit must be factory-anchored on its main contract with newGatedDec; got factories=%v creationSym=%q", src.factories, src.creationSym)
	}
	if set := src.newGatedDec().GatedContractSet(); !containsStr(set, sorocredit.MainnetContract) {
		t.Errorf("sorocredit GatedContractSet %v omits the trust root", set)
	}
}

// The real aquarius and phoenix decoders enumerate their gate as factory trust root plus curated children, the set
// gatedPrefilter scopes to. A shrunken seed or drifted factory constant would silently narrow the prefilter and
// undercount, so the membership is pinned exactly.
func TestGatedContractSet_realDecoders(t *testing.T) {
	aqSet := aquarius.NewDecoder().GatedContractSet()
	aqHave := map[string]bool{}
	for _, c := range aqSet {
		aqHave[c] = true
	}
	if !aqHave[aquarius.MainnetRouter] {
		t.Errorf("aquarius GatedContractSet omits the router (factory) %q", aquarius.MainnetRouter)
	}
	for _, p := range aquarius.MainnetPools {
		if !aqHave[p] {
			t.Errorf("aquarius GatedContractSet omits curated pool %q", p)
		}
	}
	if want := len(aquarius.MainnetPools) + 1; len(aqSet) != want {
		t.Errorf("aquarius GatedContractSet size = %d, want %d (pools + router)", len(aqSet), want)
	}

	phSet := phoenix.NewDecoder().GatedContractSet()
	phHave := map[string]bool{}
	for _, c := range phSet {
		phHave[c] = true
	}
	if !phHave[phoenix.MainnetFactory] {
		t.Errorf("phoenix GatedContractSet omits the factory %q", phoenix.MainnetFactory)
	}
	for _, p := range phoenix.MainnetGatedSet() {
		if !phHave[p] {
			t.Errorf("phoenix GatedContractSet omits curated member %q", p)
		}
	}
}

// A gated set too large to inline as a contract_id IN list falls back to an
// unscoped read rather than a query ClickHouse rejects for max_query_size.
func TestGatedPrefilter_OversizedSetIsUnscoped(t *testing.T) {
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	src := catalogueSource(t, cat, "blend")
	pf, _, err := gatedPrefilter(context.Background(), countingEventStreamer{}, src, src.genesis+10)
	if err != nil {
		t.Fatalf("gatedPrefilter: %v", err)
	}
	if len(pf) < 2 {
		t.Fatalf("blend prefilter has %d ids; the test needs at least 2", len(pf))
	}
	old := maxGatedPrefilterIDs
	maxGatedPrefilterIDs = len(pf) - 1
	t.Cleanup(func() { maxGatedPrefilterIDs = old })
	pf, _, err = gatedPrefilter(context.Background(), countingEventStreamer{}, src, src.genesis+10)
	if err != nil {
		t.Fatalf("gatedPrefilter: %v", err)
	}
	if pf != nil {
		t.Errorf("oversized prefilter = %d ids, want nil (unscoped)", len(pf))
	}
}

// In the aquarius/phoenix prefilter walk, a creation event whose decoder panics must surface as a blind spot (its child
// unregistered on the expected side) while the healthy child is still announced.
func TestGatedPrefilter_PanicIsBlindNotCrash(t *testing.T) {
	const badLedger = uint32(105)
	evs := []events.Event{
		mockCreate(100, prefFactory, prefInWin, 1),
		mockCreate(badLedger, prefFactory, prefForeign, 2),
	}
	src := reconSource{
		name: "mockgated", genesis: 1, dec: newMockGatedDecoder(),
		factories: []string{prefFactory}, creationSym: "create",
		newGatedDec: func() gatedDecoder {
			return panickingGatedDecoder{mockGatedDecoder: newMockGatedDecoder(), badChild: prefForeign}
		},
	}
	pf, blind, err := gatedPrefilter(context.Background(), countingEventStreamer{evs: evs}, src, 200)
	if err != nil {
		t.Fatalf("gatedPrefilter: %v", err)
	}
	if !strings.Contains(strings.Join(pf, ","), prefInWin) {
		t.Errorf("prefilter %v lost the healthy child %s", pf, prefInWin)
	}
	if blind.UndecodableMatched != 1 || len(blind.Ledgers) != 1 || blind.Ledgers[0] != badLedger {
		t.Errorf("blind = %+v, want 1 undecodable on ledger %d", blind, badLedger)
	}
}
