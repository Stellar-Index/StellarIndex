// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
)

// ── mock identity-gated decoder ─────────────────────────────────────────────
// Mirrors the aquarius/phoenix gate contract WITHOUT the XDR-heavy real bodies,
// so the test drives the REAL contractid.Registry (GatedSet / Seed / Has /
// IsFactory) and the REAL gatedPrefilter + re-derive entry points — a "biz"
// event is counted only from a registered contract, a "create" event
// (factory-only) registers the child named in its Value. This is exactly the
// identity gate ReDeriveOutputCountsByKindFromEvents' prefilter must preserve.

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

// countingEventStreamer models the CH contract_events reader: it applies the
// `contract_id IN (…)` prefilter (the load-bearing filter this fix adds) and
// tallies how many rows it actually streamed, so the test can prove the
// prefilter genuinely narrows the read (non-vacuity). topic0Syms is ignored —
// the walk passes it as a redundant secondary prefilter, and dropping it here
// only makes the fake stream a SUPERSET to the decoder, never fewer rows.
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

// TestGatedPrefilter_identicalCountsSelfSeedPreserved is the load-bearing proof
// of the -pass timeout fix: restricting the -ch re-derive to a factory-gated
// source's contract set yields BYTE-IDENTICAL per-ledger/-kind counts to the
// whole-lake stream, because Matches() rejects the excluded contracts anyway.
//
// Non-vacuous on three axes:
//   - the prefilter genuinely EXCLUDES a real streamed row (the foreign
//     look-alike emitting the identical "biz" shape) — the filtered read
//     touches fewer rows than the unfiltered one;
//   - it still counts an IN-WINDOW child's events (self-seeding preserved: the
//     prefilter walk captured the pool the factory announced inside [lo,hi]);
//   - it includes the factory trust root (task requirement — so add_pool
//     creation events keep streaming and self-seeding in-window children).
func TestGatedPrefilter_identicalCountsSelfSeedPreserved(t *testing.T) {
	const lo, hi = uint32(100), uint32(200)
	// Order matters: the factory's create event (index 1, ledger 100) precedes
	// the in-window child's business event (index 2, ledger 110), so the
	// UNFILTERED stream self-seeds the child before counting it — the exact
	// timing the prefilter must not disturb.
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
	pf, err := gatedPrefilter(context.Background(), countingEventStreamer{evs: evs, streamed: &pfStreamed}, src, hi)
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

// TestGatedContractSet_realDecoders proves the REAL aquarius and phoenix
// decoders enumerate their gate as factory-trust-root ∪ curated children — the
// set gatedPrefilter scopes the lake read to. If a decoder's seed shrinks (or
// the factory constant drifts) the prefilter would silently narrow and
// undercount, so this pins the exact membership.
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

// TestCatalogue_GatedPrefilterOptIn pins the exact opt-in set: aquarius and
// phoenix (identity gates that time out / risk timing out on the whole-lake
// re-derive) opt in; defindex must NOT (its decode correlates events ACROSS
// contracts in the same tx, which a contract-id prefilter would break), and
// neither may a census / oracle / ContractCall source.
func TestCatalogue_GatedPrefilterOptIn(t *testing.T) {
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	optedIn := map[string]bool{}
	for _, src := range cat {
		if src.newGatedDec != nil {
			optedIn[src.name] = true
		}
	}
	for _, name := range []string{"aquarius", "phoenix"} {
		if !optedIn[name] {
			t.Errorf("source %q must opt into the -ch gated prefilter (it streams the whole lake otherwise)", name)
		}
	}
	if optedIn["defindex"] {
		t.Errorf("defindex must NOT opt into the gated prefilter — its decode correlates events across contracts in the same tx, which a contract-id prefilter would break")
	}
	// Sanity: the opt-in stays narrow — exactly the two identity-gated AMMs.
	if len(optedIn) != 2 {
		t.Errorf("gated-prefilter opt-in set = %v, want exactly {aquarius, phoenix}", optedIn)
	}
}
