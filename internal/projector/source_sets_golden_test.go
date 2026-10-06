// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package projector_test

// Golden reference tables for every list that decides which source is
// projected, sole-written or dispatcher-written. They freeze today's sets
// (names and event types only, never Source struct fields) so a refactor
// that derives these lists from one spec can prove it changed nothing, and so
// a source added to one list and not another fails here instead of dropping
// or double-writing rows. External test package: pipeline cannot import
// projector.

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/accounts"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	claimable_balances "github.com/Stellar-Index/StellarIndex/internal/sources/claimable_balances"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/liquidity_pools"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	sac_balances "github.com/Stellar-Index/StellarIndex/internal/sources/sac_balances"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41_transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/trustlines"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
)

// goldenProjectedEvents is IsProjectedEvent's true set (42 types), keyed by
// the spelling used in pipeline/sink.go so the AST check can compare it.
var goldenProjectedEvents = map[string]consumer.Event{
	"soroswap.TradeEvent":           soroswap.TradeEvent{},
	"soroswap.SkimEvent":            soroswap.SkimEvent{},
	"soroswap.LiquidityEvent":       soroswap.LiquidityEvent{},
	"aquarius.TradeEvent":           aquarius.TradeEvent{},
	"aquarius.ReservesEvent":        aquarius.ReservesEvent{},
	"aquarius.LiquidityEvent":       aquarius.LiquidityEvent{},
	"aquarius.RewardsEvent":         aquarius.RewardsEvent{},
	"aquarius.AdminEvent":           aquarius.AdminEvent{},
	"aquarius.FeeEvent":             aquarius.FeeEvent{},
	"aquarius.KillEvent":            aquarius.KillEvent{},
	"phoenix.TradeEvent":            phoenix.TradeEvent{},
	"phoenix.LiquidityEvent":        phoenix.LiquidityEvent{},
	"phoenix.StakeEvent":            phoenix.StakeEvent{},
	"phoenix.InitializeEvent":       phoenix.InitializeEvent{},
	"phoenix.AdminEvent":            phoenix.AdminEvent{},
	"comet.TradeEvent":              comet.TradeEvent{},
	"comet.LiquidityEvent":          comet.LiquidityEvent{},
	"sushiswap_v3.TradeEvent":       sushiswap_v3.TradeEvent{},
	"sushiswap_v3.PositionEvent":    sushiswap_v3.PositionEvent{},
	"reflector.UpdateEvent":         reflector.UpdateEvent{},
	"redstone.UpdateEvent":          redstone.UpdateEvent{},
	"blend.NewAuctionEvent":         blend.NewAuctionEvent{},
	"blend.FillAuctionEvent":        blend.FillAuctionEvent{},
	"blend.DeleteAuctionEvent":      blend.DeleteAuctionEvent{},
	"blend.PositionEvent":           blend.PositionEvent{},
	"blend.EmissionEvent":           blend.EmissionEvent{},
	"blend.AdminEvent":              blend.AdminEvent{},
	"blend_backstop.Event":          blend_backstop.Event{},
	"blend_emitter.DistributeEvent": blend_emitter.DistributeEvent{},
	"blend_emitter.DropEvent":       blend_emitter.DropEvent{},
	"blend_emitter.SwapConfigEvent": blend_emitter.SwapConfigEvent{},
	"cctp.Event":                    cctp.Event{},
	"rozo.Event":                    rozo.Event{},
	"sorocredit.Event":              sorocredit.Event{},
	"defindex.Event":                defindex.Event{},
	"defindex.VaultEvent":           defindex.VaultEvent{},
	"defindex.DFeesEvent":           defindex.DFeesEvent{},
	"defindex.AdminEvent":           defindex.AdminEvent{},
	"upshift.Event":                 upshift.Event{},
	"spectra.Event":                 spectra.Event{},
	"sep41_supply.Event":            sep41_supply.Event{},
	"sep41_transfers.Event":         sep41_transfers.Event{},
}

// goldenDispatcherWrittenEvents are the types IsProjectedEvent rejects: the
// dispatcher is their only writer.
var goldenDispatcherWrittenEvents = map[string]consumer.Event{
	"sdex.TradeEvent":                sdex.TradeEvent{},
	"external.TradeEvent":            external.TradeEvent{},
	"external.UpdateEvent":           external.UpdateEvent{},
	"band.UpdateEvent":               band.UpdateEvent{},
	"soroswap_router.Event":          soroswap_router.Event{},
	"accounts.Observation":           accounts.Observation{},
	"trustlines.Observation":         trustlines.Observation{},
	"claimable_balances.Observation": claimable_balances.Observation{},
	"liquidity_pools.Observation":    liquidity_pools.Observation{},
	"sac_balances.Observation":       sac_balances.Observation{},
}

// goldenSoleWriterEvents: the projector alone writes these, even in Phase 3.
var goldenSoleWriterEvents = []string{"rozo.Event", "sep41_supply.Event", "sep41_transfers.Event"}

// goldenProjectorSources is projector.KnownProjectorSources, i.e. buildSource's cases.
var goldenProjectorSources = []string{
	"aquarius", "blend", "blend_backstop", "blend_emitter", "cctp", "comet", "defindex",
	"phoenix", "redstone", "reflector-cex", "reflector-dex", "reflector-fx", "rozo",
	"sep41_supply", "sep41_transfers", "sorocredit", "soroswap", "spectra",
	"sushiswap_v3", "upshift",
}

// goldenDispatcherSources is every name BuildDispatcher accepts.
var goldenDispatcherSources = []string{
	"aquarius", "band", "blend", "blend_backstop", "blend_emitter", "cctp", "comet", "defindex",
	"phoenix", "redstone", "reflector-cex", "reflector-dex", "reflector-fx", "rozo", "sdex",
	"sorocredit", "soroswap", "soroswap-router", "spectra", "sushiswap_v3", "upshift",
}

// goldenSorobanSourceNames is pipeline.SorobanSourceNames: the dispatcher
// names minus sdex (classic ops, not soroban_events).
var goldenSorobanSourceNames = []string{
	"aquarius", "band", "blend", "blend_backstop", "blend_emitter", "cctp", "comet", "defindex",
	"phoenix", "redstone", "reflector-cex", "reflector-dex", "reflector-fx", "rozo",
	"sorocredit", "soroswap", "soroswap-router", "spectra", "sushiswap_v3", "upshift",
}

// goldenSupplyDecoders is what RegisterSupplyEventDecoders adds when contracts
// are watched: the dispatcher's sep41 half. sep41 is sole-writer projected, so
// the dispatcher's copy is skipped by SinkModeSkipSoleWriter.
var goldenSupplyDecoders = []string{"sep41_supply", "sep41_transfers"}

// Recorded disagreements between lists. Each is a known gap, not a goal: the
// test fails when one changes so the change is a decision.
var (
	// sep41 is registered by BuildRegistry unconditionally and by
	// RegisterSupplyEventDecoders from supply.watched_sep41_contracts, never
	// via enabled_sources, so config.KnownSources does not list it.
	knownProjectedNotInConfig = []string{"sep41_supply", "sep41_transfers"}
	// config sources with no projector case (dispatcher-only writers).
	knownConfigNotProjected = []string{"band", "sdex", "soroswap-router"}
	// ch-rebuild-projected.sh KNOWN_SOURCES omits these projected sources.
	knownScriptOmitsProjected = []string{
		"blend_backstop", "blend_emitter", "redstone", "reflector-cex", "reflector-dex",
		"reflector-fx", "sep41_supply", "sep41_transfers", "sorocredit", "spectra",
		"sushiswap_v3", "upshift",
	}
	// completeness' static audit list omits these (oracles are config-gated
	// in AuditedSources; sep41 has no entry).
	knownStaticAuditOmitsProjected = []string{
		"redstone", "reflector-cex", "reflector-dex", "reflector-fx", "sep41_supply", "sep41_transfers",
	}
	// sourcenet.PubnetOnlySources and completeness (oracles configured) omit
	// the sep41 sources; external.Registry also lacks blend_backstop.
	knownSep41Omitted                   = []string{"sep41_supply", "sep41_transfers"}
	knownExternalRegistryOmitsProjected = []string{"blend_backstop", "sep41_supply", "sep41_transfers"}
)

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diff returns the members of a that are not in b, sorted.
func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range sortedCopy(a) {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

func requireSame(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(sortedCopy(got), ",") != strings.Join(sortedCopy(want), ",") {
		t.Errorf("%s drifted\n  missing from live: %v\n  extra in live:     %v", what, diff(want, got), diff(got, want))
	}
}

func fullOracle() config.OracleConfig {
	var o config.OracleConfig
	o.Reflector.DEXContract = "CDEX"
	o.Reflector.CEXContract = "CCEX"
	o.Reflector.FXContract = "CFX"
	o.Redstone.AdapterContract = "CREDSTONE"
	o.Band.StandardReferenceContract = "CBAND"
	return o
}

func universe() []string {
	set := map[string]bool{}
	for _, l := range [][]string{
		goldenProjectorSources, goldenDispatcherSources, keysOf(config.KnownSources),
		sourcenet.PubnetOnlySources, completeness.AuditedSources(config.Config{Oracle: fullOracle()}),
		keysOf(external.Registry),
	} {
		for _, s := range l {
			set[s] = true
		}
	}
	return keysOf(set)
}

func registryNames(r projector.Registry) []string {
	var out []string
	for _, s := range r.Sources {
		out = append(out, s.Name)
	}
	return out
}

func TestProjectedEventSet_Golden(t *testing.T) {
	if len(goldenProjectedEvents) != 42 {
		t.Fatalf("golden projected table has %d entries, want 42", len(goldenProjectedEvents))
	}
	for name, ev := range goldenProjectedEvents {
		if !pipeline.IsProjectedEvent(ev) {
			t.Errorf("%s: IsProjectedEvent = false, golden says projected", name)
		}
	}
	for name, ev := range goldenDispatcherWrittenEvents {
		if pipeline.IsProjectedEvent(ev) {
			t.Errorf("%s: IsProjectedEvent = true, golden says dispatcher-written", name)
		}
	}
	// Reverse direction: a type added to the switch but not to the golden.
	requireSame(t, "projected event types listed by pipeline.Specs", specProjectedEvents(), keysOf(goldenProjectedEvents))
}

func TestSoleWriterSet_Golden(t *testing.T) {
	want := map[string]bool{}
	for _, n := range goldenSoleWriterEvents {
		want[n] = true
	}
	for name, ev := range goldenProjectedEvents {
		if got := pipeline.IsSoleWriterProjected(ev); got != want[name] {
			t.Errorf("%s: IsSoleWriterProjected = %v, golden %v", name, got, want[name])
		}
	}
	for name, ev := range goldenDispatcherWrittenEvents {
		if pipeline.IsSoleWriterProjected(ev) {
			t.Errorf("%s: sole-writer but not projected", name)
		}
	}
}

func TestProjectorRegistry_Golden(t *testing.T) {
	requireSame(t, "projector.KnownProjectorSources", keysOf(projector.KnownProjectorSources), goldenProjectorSources)

	for _, name := range goldenProjectorSources {
		reg, err := projector.BuildRegistry([]string{name}, fullOracle(), []string{"CWATCHED"}, nil)
		if err != nil {
			t.Fatalf("BuildRegistry(%s): %v", name, err)
		}
		got := registryNames(reg)
		if !strings.HasPrefix(name, "sep41") {
			// A non-sep41 name yields itself plus the unconditional sep41 pair.
			got = diff(got, goldenSupplyDecoders)
			requireSame(t, "BuildRegistry("+name+")", got, []string{name})
		}
	}

	reg, err := projector.BuildRegistry(keysOf(config.KnownSources), fullOracle(), []string{"CWATCHED"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireSame(t, "BuildRegistry(all config sources, watched sep41)", registryNames(reg), goldenProjectorSources)

	reg, err = projector.BuildRegistry(keysOf(config.KnownSources), fullOracle(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireSame(t, "BuildRegistry(all config sources, no sep41)",
		registryNames(reg), diff(goldenProjectorSources, goldenSupplyDecoders))

	for _, name := range []string{"reflector-dex", "reflector-cex", "reflector-fx", "redstone"} {
		if _, err := projector.BuildRegistry([]string{name}, config.OracleConfig{}, nil, nil); err == nil {
			t.Errorf("BuildRegistry(%s) with empty oracle config: want error", name)
		}
	}
}

func TestDispatcherSet_Golden(t *testing.T) {
	var accepted []string
	for _, name := range universe() {
		if _, err := pipeline.BuildDispatcher([]string{name}, fullOracle(), nil); err == nil {
			accepted = append(accepted, name)
		}
	}
	requireSame(t, "names BuildDispatcher accepts", accepted, goldenDispatcherSources)
	requireSame(t, "pipeline.SorobanSourceNames", pipeline.SorobanSourceNames, goldenSorobanSourceNames)

	for _, name := range []string{"reflector-dex", "reflector-cex", "reflector-fx", "redstone", "band"} {
		if _, err := pipeline.BuildDispatcher([]string{name}, config.OracleConfig{}, nil); err == nil {
			t.Errorf("BuildDispatcher(%s) with empty oracle config: want error", name)
		}
	}
}

// The dispatcher's sep41 registration lives outside BuildDispatcher; sep41 is
// sole-writer projected, so this is the half that must never be the writer.
func TestRegisterSupplyEventDecoders_Golden(t *testing.T) {
	got, err := pipeline.RegisterSupplyEventDecoders(dispatcher.New(), config.SupplyConfig{WatchedSEP41Contracts: []string{"CWATCHED"}})
	if err != nil {
		t.Fatal(err)
	}
	requireSame(t, "RegisterSupplyEventDecoders (watched)", got, goldenSupplyDecoders)

	got, err = pipeline.RegisterSupplyEventDecoders(dispatcher.New(), config.SupplyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	requireSame(t, "RegisterSupplyEventDecoders (none watched)", got, nil)
}

func TestSourceSets_CrossCheck(t *testing.T) {
	projected := keysOf(projector.KnownProjectorSources)
	cfg := keysOf(config.KnownSources)

	requireSame(t, "projected names absent from config.KnownSources", diff(projected, cfg), knownProjectedNotInConfig)
	requireSame(t, "config.KnownSources absent from projector", diff(cfg, projected), knownConfigNotProjected)
	requireSame(t, "BuildDispatcher names vs config.KnownSources", goldenDispatcherSources, cfg)
	requireSame(t, "SorobanSourceNames vs dispatcher minus sdex", pipeline.SorobanSourceNames, diff(goldenDispatcherSources, []string{"sdex"}))

	// Every dispatcher-registered sep41 decoder is a projector source whose
	// events are projected and sole-written.
	for _, n := range goldenSupplyDecoders {
		if _, ok := projector.KnownProjectorSources[n]; !ok {
			t.Errorf("%s dispatcher-registered but not a projector source", n)
		}
	}

	requireSame(t, "projected absent from sourcenet.PubnetOnlySources",
		diff(projected, sourcenet.PubnetOnlySources), knownSep41Omitted)
	requireSame(t, "projected absent from completeness static audit list",
		diff(projected, completeness.AuditedSources(config.Config{})), knownStaticAuditOmitsProjected)
	requireSame(t, "projected absent from completeness audit list (oracles configured)",
		diff(projected, completeness.AuditedSources(config.Config{Oracle: fullOracle()})), knownSep41Omitted)
	requireSame(t, "projected absent from external.Registry",
		diff(projected, keysOf(external.Registry)), knownExternalRegistryOmitsProjected)
}

func TestScriptKnownSources_Golden(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "ops", "ch-rebuild-projected.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^KNOWN_SOURCES="([^"]*)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("KNOWN_SOURCES line not found in ch-rebuild-projected.sh")
	}
	script := strings.Fields(string(m[1]))
	requireSame(t, "ch-rebuild-projected.sh KNOWN_SOURCES", script,
		diff(keysOf(projector.KnownProjectorSources), knownScriptOmitsProjected))
	for _, s := range script {
		if _, ok := projector.KnownProjectorSources[s]; !ok {
			t.Errorf("script lists %q, which is not a projector source", s)
		}
	}
}

// specProjectedEvents names every event type a projected pipeline spec
// lists, spelled as sink.go spells it (package dir basename + type).
func specProjectedEvents() []string {
	names := map[string]bool{}
	for _, spec := range pipeline.Specs() {
		if spec.Projector == nil {
			continue
		}
		for _, ev := range spec.Events {
			t := reflect.TypeOf(ev)
			names[path.Base(t.PkgPath())+"."+t.Name()] = true
		}
	}
	return keysOf(names)
}
